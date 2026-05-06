package access

import (
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"
)

func TestResolveExceptionCalendarState_emptyDB(t *testing.T) {
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	full, early, active := ResolveExceptionCalendarState(db, 2026, 1, 1)
	if full || active || early != 0 {
		t.Fatalf("want no exception, got full=%v early=%d active=%v", full, early, active)
	}
}

func TestResolveExceptionCalendarState_fullClosureWins(t *testing.T) {
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE access_exception_calendars (id TEXT PRIMARY KEY, enabled INTEGER NOT NULL DEFAULT 1, priority INTEGER NOT NULL DEFAULT 0)`)
	exec(`CREATE TABLE access_exception_dates (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		calendar_id TEXT NOT NULL,
		y INTEGER NOT NULL, m INTEGER NOT NULL, d INTEGER NOT NULL,
		kind TEXT NOT NULL,
		early_close_minute INTEGER,
		UNIQUE (calendar_id, y, m, d)
	)`)
	exec(`INSERT INTO access_exception_calendars (id, enabled, priority) VALUES ('cal1', 1, 10)`)
	exec(`INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute) VALUES ('cal1', 2026, 6, 15, 'full_closure', NULL)`)

	full, early, active := ResolveExceptionCalendarState(db, 2026, 6, 15)
	if !full || active || early != 0 {
		t.Fatalf("want full closure, got full=%v early=%d active=%v", full, early, active)
	}
}

func TestResolveExceptionCalendarState_earlyClosure(t *testing.T) {
	db, err := sqlx.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE access_exception_calendars (id TEXT PRIMARY KEY, enabled INTEGER NOT NULL DEFAULT 1, priority INTEGER NOT NULL DEFAULT 0)`)
	exec(`CREATE TABLE access_exception_dates (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		calendar_id TEXT NOT NULL,
		y INTEGER NOT NULL, m INTEGER NOT NULL, d INTEGER NOT NULL,
		kind TEXT NOT NULL,
		early_close_minute INTEGER,
		UNIQUE (calendar_id, y, m, d)
	)`)
	exec(`INSERT INTO access_exception_calendars (id, enabled, priority) VALUES ('calA', 1, 5), ('calB', 1, 5)`)
	exec(`INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute) VALUES ('calA', 2026, 12, 24, 'early_closure', 900)`)
	exec(`INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute) VALUES ('calB', 2026, 12, 24, 'early_closure', 720)`)

	full, early, active := ResolveExceptionCalendarState(db, 2026, 12, 24)
	if full || !active || early != 720 {
		t.Fatalf("want earliest early=720, got full=%v early=%d active=%v", full, early, active)
	}
}

func TestCivilDateInLocation(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	// 2026-01-15 04:00 UTC = Jan 14 23:00 EST
	u := time.Date(2026, 1, 15, 4, 0, 0, 0, time.UTC)
	y, m, d := CivilDateInLocation(u, loc)
	if y != 2026 || m != 1 || d != 14 {
		t.Fatalf("got y=%d m=%d d=%d", y, m, d)
	}
}
