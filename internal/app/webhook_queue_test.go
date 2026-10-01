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

func TestWebhookQueueOrderedAndCoalesced(t *testing.T) {
	quietLogs(t)
	release := make(chan struct{})
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		ev, _ := p["event"].(string)
		if ev == "first" {
			<-release // hold the worker so later events queue up behind it
		}
		if n, ok := p["entered"].(float64); ok {
			ev += ":" + string(rune('0'+int(n)))
		}
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}))
	defer srv.Close()

	ctx := newDefaultAppContext(context.Background(), nil)
	ctx.Config.WebhookEventEnabled = true
	ctx.Config.WebhookEventURL = srv.URL

	postEventWebhook(ctx, "first", nil)
	time.Sleep(100 * time.Millisecond) // worker is now blocked inside "first"
	for i := 1; i <= 4; i++ {
		postEventWebhook(ctx, "pin_progress", map[string]any{"entered": i})
	}
	postEventWebhook(ctx, "pin_accepted", nil)
	postEventWebhook(ctx, "pin_progress", map[string]any{"entered": 0})
	close(release)

	want := []string{"first", "pin_progress:4", "pin_accepted", "pin_progress:0"}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= len(want) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
