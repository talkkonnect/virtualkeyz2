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