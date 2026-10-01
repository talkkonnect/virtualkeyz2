package app

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"virtualkeyz2/internal/store"
)

func TestAuditLogBatchedWriter(t *testing.T) {
	quietLogs(t)
	dsn := "file:" + filepath.Join(t.TempDir(), "audit.db") + "?_fk=1&_busy_timeout=5000&_journal_mode=WAL"
	db, err := store.OpenAccessDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		auditMu.Lock()
		auditW = nil
		auditMu.Unlock()
		db.Close()
	})
	auditMu.Lock()
	auditW = nil
	auditMu.Unlock()

	ctx := newDefaultAppContext(context.Background(), db)
	const n = 120 // more than one batch
	for i := range n {
		auditLogEvent(ctx, "test_event", map[string]any{"i": i})
	}
	auditLogFlush(5 * time.Second)
	var got int
	if err := db.Get(&got, `SELECT COUNT(*) FROM logs WHERE event_name = 'test_event'`); err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("rows = %d, want %d", got, n)
	}
	var first, last string
	if err := db.Get(&first, `SELECT detail_json FROM logs WHERE event_name = 'test_event' ORDER BY id LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&last, `SELECT detail_json FROM logs WHERE event_name = 'test_event' ORDER BY id DESC LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if first != `{"i":0}` || last != fmt.Sprintf(`{"i":%d}`, n-1) {
		t.Fatalf("order not preserved: first=%s last=%s", first, last)
	}

	// The timer path (no explicit flush) also writes.
	auditLogEvent(ctx, "timer_event", nil)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := db.Get(&got, `SELECT COUNT(*) FROM logs WHERE event_name = 'timer_event'`); err == nil && got == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timer-triggered batch not written")
}
