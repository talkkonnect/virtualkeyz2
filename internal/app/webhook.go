package app

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

func cloneStringBoolMap(m map[string]bool) map[string]bool {
	if m == nil {
		return nil
	}
	out := make(map[string]bool, len(m))
	maps.Copy(out, m)
	return out
}

func cloneWebhookEventEndpoints(src []WebhookEventEndpoint) []WebhookEventEndpoint {
	if src == nil {
		return nil
	}
	out := make([]WebhookEventEndpoint, len(src))
	for i := range src {
		out[i] = src[i]
		out[i].EventTypes = cloneStringBoolMap(src[i].EventTypes)
	}
	return out
}

func webhookEventTypesAllowlistPass(m map[string]bool, event string) bool {
	if len(m) == 0 {
		return true
	}
	return m[event]
}

// webhookSharedTransport pools outbound webhook TCP/TLS connections (bounded idle per host).
var webhookSharedTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          64,
	MaxIdleConnsPerHost:   16,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

const (
	webhookBreakerClosed = iota
	webhookBreakerOpen
	webhookBreakerHalfOpen
)

// webhookOutboundCfg is a snapshot of outbound webhook limits at enqueue time.
type webhookOutboundCfg struct {
	HTTPTimeout           time.Duration
	MaxConcurrent         int
	CircuitBreakerEnabled bool
	FailureThreshold      int
	OpenDuration          time.Duration
}

type webhookOutboundGuard struct {
	mu sync.Mutex
	// inFlight is webhook HTTP workers that passed acquire and have not yet called release.
	inFlight  int
	state     int
	failures  int
	openUntil time.Time

	lastRejectLogMu sync.Mutex
	lastRejectLog   time.Time
}

func (g *webhookOutboundGuard) logRejectThrottled(msg string) {
	now := time.Now()
	g.lastRejectLogMu.Lock()
	ok := now.Sub(g.lastRejectLog) >= 30*time.Second
	if ok {
		g.lastRejectLog = now
	}
	g.lastRejectLogMu.Unlock()
	if ok {
		log.Printf("WARNING: %s", msg)
	}
}

func (g *webhookOutboundGuard) acquire(app *AppContext) (webhookOutboundCfg, bool) {
	app.configMu.RLock()
	cfg := webhookOutboundCfg{
		HTTPTimeout:           app.Config.WebhookHTTPTimeout,
		MaxConcurrent:         app.Config.WebhookMaxConcurrent,
		CircuitBreakerEnabled: app.Config.WebhookCircuitBreakerEnabled,
		FailureThreshold:      app.Config.WebhookCircuitFailureThreshold,
		OpenDuration:          app.Config.WebhookCircuitOpenDuration,
	}
	app.configMu.RUnlock()
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 25 * time.Second
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 16
	}
	if cfg.CircuitBreakerEnabled {
		if cfg.FailureThreshold <= 0 {
			cfg.FailureThreshold = 5
		}
		if cfg.OpenDuration <= 0 {
			cfg.OpenDuration = 60 * time.Second
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if cfg.MaxConcurrent > 0 && g.inFlight >= cfg.MaxConcurrent {
		g.logRejectThrottled(fmt.Sprintf("webhook outbound: max concurrent (%d) reached, dropping POST", cfg.MaxConcurrent))
		return webhookOutboundCfg{}, false
	}
	if cfg.CircuitBreakerEnabled {
		switch g.state {
		case webhookBreakerOpen:
			if now.Before(g.openUntil) {
				g.logRejectThrottled(fmt.Sprintf("webhook outbound: circuit open until %s, dropping POST", g.openUntil.UTC().Format(time.RFC3339)))
				return webhookOutboundCfg{}, false
			}
			g.state = webhookBreakerHalfOpen
			g.failures = 0
		case webhookBreakerHalfOpen, webhookBreakerClosed:
		}
	}
	g.inFlight++
	return cfg, true
}

func webhookOutboundFailureForBreaker(okHTTP bool, statusCode int) bool {
	if !okHTTP {
		return true
	}
	return statusCode >= 500
}

func webhookOutboundSuccessForBreaker(okHTTP bool, statusCode int) bool {
	if !okHTTP {
		return false
	}
	return statusCode < 500
}

func (g *webhookOutboundGuard) release(cfg webhookOutboundCfg, okHTTP bool, statusCode int) {
	thr := cfg.FailureThreshold
	openDur := cfg.OpenDuration
	if thr <= 0 {
		thr = 5
	}
	if openDur <= 0 {
		openDur = 60 * time.Second
	}
	now := time.Now()
	succ := webhookOutboundSuccessForBreaker(okHTTP, statusCode)
	fail := webhookOutboundFailureForBreaker(okHTTP, statusCode)

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight > 0 {
		g.inFlight--
	}
	if !cfg.CircuitBreakerEnabled {
		return
	}
	if succ {
		g.failures = 0
		g.state = webhookBreakerClosed
		return
	}
	if !fail {
		return
	}
	if g.state == webhookBreakerHalfOpen {
		g.state = webhookBreakerOpen
		g.openUntil = now.Add(openDur)
		g.failures = 0
		return
	}
	g.failures++
	if g.failures >= thr {
		g.state = webhookBreakerOpen
		g.openUntil = now.Add(openDur)
		g.failures = 0
	}
}

// fireEventWebhook POSTs JSON for door/PIN/MQTT events when webhook_event_enabled.
// Uses webhook_event_endpoints when non-empty (each entry may set enabled and optional event_types allowlist);
// otherwise uses legacy webhook_event_url. webhook_event_types is an optional global allowlist (same semantics as endpoint event_types).
// Payload never includes PINs or tokens. Optional Bearer token when token_enabled and token non-empty.
func fireEventWebhook(ctx *AppContext, event string, detail map[string]any) {
	postEventWebhook(ctx, event, detail)
	auditLogEvent(ctx, event, detail)
}

// postEventWebhook POSTs an event to the configured webhook endpoint(s) WITHOUT
// writing an audit-log row. Use it for high-frequency, transient UI events such
// as pin_progress (masked digit count for remote displays) that must not bloat
// the audit log. Security-relevant events keep using fireEventWebhook.
func postEventWebhook(ctx *AppContext, event string, detail map[string]any) {
	ctx.configMu.RLock()
	defer ctx.configMu.RUnlock()
	if !ctx.Config.WebhookEventEnabled || !webhookEventTypesAllowlistPass(ctx.Config.WebhookEventTypes, event) {
		return
	}
	pay := map[string]any{
		"type":             "event",
		"event":            event,
		"timestamp":        time.Now().UTC().Format(time.RFC3339Nano),
		"device_client_id": ctx.Config.MQTTClientID,
	}
	maps.Copy(pay, detail)
	body, err := json.Marshal(pay)
	if err != nil {
		log.Printf("WARNING: webhook JSON marshal: %v", err)
		return
	}
	// Enqueueing never blocks, so the endpoints can be read under the config lock without cloning.
	if eps := ctx.Config.WebhookEventEndpoints; len(eps) > 0 {
		for i := range eps {
			ep := &eps[i]
			if !ep.Enabled || !webhookEventTypesAllowlistPass(ep.EventTypes, event) {
				continue
			}
			webhookEnqueue(ctx, webhookJob{event: event, url: ep.URL, tokenEnabled: ep.TokenEnabled, token: ep.Token, body: body})
		}
		return
	}
	webhookEnqueue(ctx, webhookJob{event: event, url: ctx.Config.WebhookEventURL, tokenEnabled: ctx.Config.WebhookEventTokenEnabled, token: ctx.Config.WebhookEventToken, body: body})
}

// fireHeartbeatWebhook POSTs to webhook_heartbeat_url on each heartbeat tick when enabled.
func fireHeartbeatWebhook(ctx *AppContext) {
	ctx.configMu.RLock()
	if !ctx.Config.WebhookHeartbeatEnabled {
		ctx.configMu.RUnlock()
		return
	}
	url := strings.TrimSpace(ctx.Config.WebhookHeartbeatURL)
	tokEn := ctx.Config.WebhookHeartbeatTokenEnabled
	tok := ctx.Config.WebhookHeartbeatToken
	cid := ctx.Config.MQTTClientID
	interval := ctx.Config.HeartbeatInterval
	ctx.configMu.RUnlock()
	if url == "" {
		return
	}
	if interval <= 0 {
		interval = 60 * time.Second
	}
	pay := map[string]any{
		"type":               "heartbeat",
		"event":              "heartbeat",
		"timestamp":          time.Now().UTC().Format(time.RFC3339Nano),
		"device_client_id":   cid,
		"heartbeat_interval": interval.String(),
	}
	webhookPostJSONAsync(ctx, url, tokEn, tok, pay)
}

func startHeartbeatAPI(ctx *AppContext) {
	for {
		ctx.configMu.RLock()
		d := ctx.Config.HeartbeatInterval
		ctx.configMu.RUnlock()
		if d <= 0 {
			d = 60 * time.Second
		}
		timer := time.NewTimer(d)
		<-timer.C
		debugf("Heartbeat tick (webhook if configured).")
		fireHeartbeatWebhook(ctx)
	}
}
