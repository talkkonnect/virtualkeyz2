package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Outbound webhooks are delivered by one worker goroutine per endpoint, in the order they were
// raised (a goroutine per POST could reorder e.g. pin_progress digit counts on the remote display).
// Queues are bounded; when one is full the newest event is dropped, as before. Consecutive
// pin_progress events still waiting are coalesced, since only the latest digit count matters.
const (
	webhookQueueMax      = 64
	webhookWorkerIdleTTL = 5 * time.Minute
)

// webhookHTTPClient is shared by all workers; per-request timeouts come from the request context.
var webhookHTTPClient = &http.Client{Transport: webhookSharedTransport}

type webhookJob struct {
	event        string
	url          string
	tokenEnabled bool
	token        string
	body         []byte
}

type webhookQueue struct {
	pending []webhookJob
	wake    chan struct{}
}

var (
	webhookQueuesMu sync.Mutex
	webhookQueues   = map[string]*webhookQueue{}
)

// webhookPostJSONAsync queues payload for delivery to url (marshalled once by the caller path).
func webhookPostJSONAsync(app *AppContext, url string, tokenEnabled bool, token string, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("WARNING: webhook JSON marshal: %v", err)
		return
	}
	event, _ := payload["event"].(string)
	webhookEnqueue(app, webhookJob{event: event, url: url, tokenEnabled: tokenEnabled, token: token, body: body})
}

// webhookEnqueue adds a job to its endpoint's queue without blocking.
func webhookEnqueue(app *AppContext, j webhookJob) {
	if app == nil {
		return
	}
	j.url = strings.TrimSpace(j.url)
	j.token = strings.TrimSpace(j.token)
	if j.url == "" {
		return
	}
	key := j.url + "\x00" + j.token
	webhookQueuesMu.Lock()
	defer webhookQueuesMu.Unlock()
	q, ok := webhookQueues[key]
	if !ok {
		q = &webhookQueue{wake: make(chan struct{}, 1)}
		webhookQueues[key] = q
		go runWebhookWorker(app, key, q)
	}
	switch n := len(q.pending); {
	case j.event == "pin_progress" && n > 0 && q.pending[n-1].event == "pin_progress":
		q.pending[n-1] = j
	case n >= webhookQueueMax:
		app.webhookOutbound.logRejectThrottled("webhook outbound: queue full for " + j.url + ", dropping POST")
		return
	default:
		q.pending = append(q.pending, j)
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func runWebhookWorker(app *AppContext, key string, q *webhookQueue) {
	idle := time.NewTimer(webhookWorkerIdleTTL)
	defer idle.Stop()
	for {
		webhookQueuesMu.Lock()
		if len(q.pending) == 0 {
			webhookQueuesMu.Unlock()
			select {
			case <-q.wake:
			case <-idle.C:
				webhookQueuesMu.Lock()
				if len(q.pending) == 0 {
					delete(webhookQueues, key)
					webhookQueuesMu.Unlock()
					return
				}
				webhookQueuesMu.Unlock()
			}
			idle.Reset(webhookWorkerIdleTTL)
			continue
		}
		j := q.pending[0]
		q.pending[0] = webhookJob{}
		q.pending = q.pending[1:]
		webhookQueuesMu.Unlock()
		webhookDeliver(app, j)
	}
}

// webhookDeliver POSTs one job, honouring the outbound guard (max concurrent + circuit breaker).
func webhookDeliver(app *AppContext, j webhookJob) {
	cfg, ok := app.webhookOutbound.acquire(app)
	if !ok {
		return
	}
	var okHTTP bool
	var statusCode int
	defer func() {
		app.webhookOutbound.release(cfg, okHTTP, statusCode)
	}()
	reqCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, j.url, bytes.NewReader(j.body))
	if err != nil {
		log.Printf("WARNING: webhook request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "virtualkeyz2-webhook/1.0")
	if j.tokenEnabled && j.token != "" {
		req.Header.Set("Authorization", "Bearer "+j.token)
	}
	resp, err := webhookHTTPClient.Do(req)
	if err != nil {
		log.Printf("WARNING: webhook POST %q: %v", j.url, err)
		return
	}
	okHTTP = true
	statusCode = resp.StatusCode
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		log.Printf("WARNING: webhook POST %q: read body: %v", j.url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		log.Printf("WARNING: webhook POST %q: HTTP %s", j.url, resp.Status)
	}
}
