package main

import (
	"testing"
	"time"
)

func TestMinuteMatchesWindow(t *testing.T) {
	if !minuteMatchesWindow(540, 540, 1020) {
		t.Fatal("same-day inclusive start")
	}
	if !minuteMatchesWindow(1020, 540, 1020) {
		t.Fatal("same-day inclusive end")
	}
	if minuteMatchesWindow(539, 540, 1020) {
		t.Fatal("before window")
	}
	if minuteMatchesWindow(1021, 540, 1020) {
		t.Fatal("after window")
	}
	// Overnight 22:00 (1320) – 06:00 (360)
	if !minuteMatchesWindow(1320, 1320, 360) {
		t.Fatal("overnight at start")
	}
	if !minuteMatchesWindow(0, 1320, 360) {
		t.Fatal("overnight after midnight")
	}
	if !minuteMatchesWindow(360, 1320, 360) {
		t.Fatal("overnight at end")
	}
	if minuteMatchesWindow(720, 1320, 360) {
		t.Fatal("midday outside overnight")
	}
}

func TestTimeMatchesProfileWindows(t *testing.T) {
	// Monday 9:00 = 9*60 = 540; window 8:45–17:00
	rows := []struct {
		weekday      int
		start, end int
	}{{1, 8*60 + 45, 17 * 60}}
	if !timeMatchesProfileWindows(time.Monday, 9*60, rows) {
		t.Fatal("expected inside business window")
	}
	if timeMatchesProfileWindows(time.Monday, 8*60+44, rows) {
		t.Fatal("expected before grace")
	}
	// weekday 7 = any day
	anyDay := []struct {
		weekday      int
		start, end int
	}{{7, 0, 1439}}
	if !timeMatchesProfileWindows(time.Wednesday, 0, anyDay) {
		t.Fatal("wildcard day")
	}
}

func TestTimeMatchesProfileWindowsWithExceptionsEarlyClose(t *testing.T) {
	// Mon 8:45–17:00
	wins := []struct {
		weekday      int
		start, end int
	}{{1, 525, 1020}}
	// 12:30 still inside nominal window and inside 1pm early close (clip ends 780)
	if !timeMatchesProfileWindowsWithExceptions(time.Monday, 12*60+30, wins, true, false, true, 13*60) {
		t.Fatal("expected inside clipped window before 1pm close")
	}
	// 2 PM after 1pm early close
	if timeMatchesProfileWindowsWithExceptions(time.Monday, 14*60, wins, true, false, true, 13*60) {
		t.Fatal("expected outside after early close")
	}
	// respects_exceptions off → ignore early close
	if !timeMatchesProfileWindowsWithExceptions(time.Monday, 14*60, wins, false, false, true, 13*60) {
		t.Fatal("non-respecting profile should ignore early close")
	}
}

func TestResolveAccessExceptionCalendarPriority(t *testing.T) {
	db := testAccessDB(t)
	defer db.Close()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	must(`INSERT INTO access_exception_calendars (id, display_name, priority, enabled, source_note) VALUES ('low','',10,1,'')`)
	must(`INSERT INTO access_exception_calendars (id, display_name, priority, enabled, source_note) VALUES ('high','',100,1,'')`)
	must(`INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute, label) VALUES ('low',2026,7,3,'early_closure',660,'')`)
	must(`INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute, label) VALUES ('high',2026,7,3,'full_closure',NULL,'')`)

	full, _, active := resolveAccessExceptionCalendarState(db, 2026, 7, 3)
	if !full || active {
		t.Fatalf("expected full_closure from higher priority tier, got full=%v active=%v", full, active)
	}
}