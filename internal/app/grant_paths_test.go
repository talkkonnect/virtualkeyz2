package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// webhookRecorder captures event webhooks posted by the app.
type webhookRecorder struct {
	mu   sync.Mutex
	evts []map[string]any
}

func newWebhookCtx(t *testing.T) (*AppContext, *webhookRecorder) {
	t.Helper()
	quietLogs(t)
	rec := &webhookRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		rec.mu.Lock()
		rec.evts = append(rec.evts, p)
		rec.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	ctx := newDefaultAppContext(context.Background(), nil)
	ctx.lcdUI = nil // no display goroutine in tests; lcdEnqueueFullSync would wait for its ack
	ctx.Config.WebhookEventEnabled = true
	ctx.Config.WebhookEventURL = srv.URL
	ctx.Config.FallbackAccessPin = "246810"
	ctx.Config.PinLockoutOverridePin = ""
	for _, p := range []*bool{&ctx.Config.SoundPinOKEnabled, &ctx.Config.SoundPinRejectEnabled, &ctx.Config.SoundAccessGrantedEnabled} {
		*p = false
	}
	normalizeKeypadAndPinUX(&ctx.Config)
	return ctx, rec
}

func (r *webhookRecorder) waitFor(t *testing.T, event string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, e := range r.evts {
			if e["event"] == event {
				r.mu.Unlock()
				return e
			}
		}
		r.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("webhook %q not received", event)
	return nil
}

func TestGrantPathFallbackPINDoor(t *testing.T) {
	ctx, rec := newWebhookCtx(t)
	ctx.Config.KeypadOperationMode = ModeAccessEntry
	start := time.Now()
	processPIN(ctx, "246810", "")
	if time.Since(start) > time.Second {
		t.Fatalf("processPIN blocked for %s; input path must not sleep", time.Since(start))
	}
	e := rec.waitFor(t, "pin_accepted")
	if e["keypad_role"] != "" || e["operation_mode"] != ModeAccessEntry {
		t.Fatalf("unexpected payload: %v", e)
	}
	if !ctx.inputHeld("") {
		t.Fatal("grant must arm the feedback input hold")
	}
}

func TestGrantPathStaticQRDoorAndElevator(t *testing.T) {
	ctx, rec := newWebhookCtx(t)
	ctx.Config.KeypadOperationMode = ModeAccessEntry
	ctx.grantStaticTestQRAccess(ctx.Config)
	e := rec.waitFor(t, "pin_accepted")
	if e["auth_method"] != "qr" || e["static_test_qr"] != true || e["keypad_role"] != "qr" {
		t.Fatalf("unexpected payload: %v", e)
	}

	ctx2, rec2 := newWebhookCtx(t)
	ctx2.Config.KeypadOperationMode = ModeElevatorWaitFloorButtons
	ctx2.grantStaticTestQRAccess(ctx2.Config)
	e2 := rec2.waitFor(t, "pin_accepted")
	if e2["elevator_phase"] != "wait_floor_input" || e2["auth_method"] != "qr" || e2["credential_label"] != "static_test_qr" {
		t.Fatalf("unexpected elevator payload: %v", e2)
	}
	ctx2.elevatorMu.Lock()
	gp := ctx2.elevatorGrantPIN
	ctx2.elevatorMu.Unlock()
	if gp != "static_test_qr" || !ctx2.elevatorStaticTestFloorACLBypass.Load() {
		t.Fatalf("elevator grant state: pin=%q bypass=%v", gp, ctx2.elevatorStaticTestFloorACLBypass.Load())
	}
}

func TestGrantPathWrongPINNoHold(t *testing.T) {
	ctx, rec := newWebhookCtx(t)
	ctx.Config.KeypadOperationMode = ModeAccessEntry
	processPIN(ctx, "000000", "entry")
	e := rec.waitFor(t, "pin_rejected")
	if e["reason"] != "invalid_pin" {
		t.Fatalf("unexpected payload: %v", e)
	}
	if ctx.inputHeld("entry") {
		t.Fatal("wrong-PIN reject must not hold input (re-entry allowed immediately)")
	}
}
