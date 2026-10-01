package app

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
)

// The audit log (table logs) is written by one background goroutine that batches rows into a
// single transaction, so callers on the keypad/QR path never wait for an SQLite commit (an fsync
// on the SD card). Rows are timestamped when queued, so batching does not change created_at.
const (
	auditQueueSize     = 512
	auditBatchMax      = 50
	auditBatchInterval = 200 * time.Millisecond
)

type auditRow struct {
	createdAt, event, clientID, detailJSON string
}

type auditWriter struct {
	db      *sqlx.DB
	rows    chan auditRow
	flushes chan chan struct{}
}

var (
	auditMu          sync.Mutex
	auditW           *auditWriter
	auditDropLastLog atomic.Int64 // unix nanoseconds of the last "queue full" warning
)

// auditWriterFor returns the process audit writer, starting it on first use.
func auditWriterFor(db *sqlx.DB) *auditWriter {
	auditMu.Lock()
	defer auditMu.Unlock()
	if auditW == nil {
		auditW = &auditWriter{db: db, rows: make(chan auditRow, auditQueueSize), flushes: make(chan chan struct{})}
		go auditW.run()
	}
	return auditW
}

// auditLogEvent records an event activity row in logs (same semantic events as webhooks). Runs even when
// webhook_event_enabled is false. detail_json matches webhook detail maps (no PINs or secrets).
func auditLogEvent(ctx *AppContext, event string, detail map[string]any) {
	if ctx == nil || ctx.DB == nil || strings.TrimSpace(event) == "" {
		return
	}
	ctx.configMu.RLock()
	cid := ctx.Config.MQTTClientID
	ctx.configMu.RUnlock()
	det := detail
	if det == nil {
		det = map[string]any{}
	}
	b, err := json.Marshal(det)
	if err != nil {
		log.Printf("WARNING: audit log JSON marshal: %v", err)
		return
	}
	row := auditRow{createdAt: time.Now().UTC().Format(time.RFC3339Nano), event: event, clientID: cid, detailJSON: string(b)}
	select {
	case auditWriterFor(ctx.DB).rows <- row:
	default:
		if now := time.Now().UnixNano(); now-auditDropLastLog.Load() > int64(time.Minute) {
			auditDropLastLog.Store(now)
			log.Printf("WARNING: audit log queue full; dropping %q row(s).", event)
		}
	}
}

// auditLogFlush writes queued rows now, waiting at most timeout (used before exit / re-exec).
func auditLogFlush(timeout time.Duration) {
	auditMu.Lock()
	w := auditW
	auditMu.Unlock()
	if w == nil {
		return
	}
	done := make(chan struct{})
	select {
	case w.flushes <- done:
	case <-time.After(timeout):
		return
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (w *auditWriter) run() {
	batch := make([]auditRow, 0, auditBatchMax)
	timer := time.NewTimer(auditBatchInterval)
	timer.Stop()
	for {
		select {
		case r := <-w.rows:
			if len(batch) == 0 {
				timer.Reset(auditBatchInterval)
			}
			batch = append(batch, r)
			if len(batch) < auditBatchMax {
				continue
			}
		case <-timer.C:
		case done := <-w.flushes:
			for drained := false; !drained; {
				select {
				case r := <-w.rows:
					batch = append(batch, r)
				default:
					drained = true
				}
			}
			w.write(batch)
			batch = batch[:0]
			close(done)
			continue
		}
		timer.Stop()
		w.write(batch)
		batch = batch[:0]
	}
}

func (w *auditWriter) write(batch []auditRow) {
	if len(batch) == 0 {
		return
	}
	tx, err := w.db.Beginx()
	if err != nil {
		log.Printf("WARNING: audit log insert (begin): %v", err)
		return
	}
	stmt, err := tx.Prepare(`INSERT INTO logs (created_at, event_type, event_name, device_client_id, detail_json) VALUES (?, 'event', ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		log.Printf("WARNING: audit log insert (prepare): %v", err)
		return
	}
	defer stmt.Close()
	for _, r := range batch {
		if _, err := stmt.Exec(r.createdAt, r.event, r.clientID, r.detailJSON); err != nil {
			_ = tx.Rollback()
			log.Printf("WARNING: audit log insert: %v (%d row(s) lost)", err, len(batch))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("WARNING: audit log insert (commit): %v (%d row(s) lost)", err, len(batch))
	}
}
