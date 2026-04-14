package main

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func testAccessDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:acltest?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE access_pins (
		pin TEXT PRIMARY KEY NOT NULL,
		label TEXT,
		enabled INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		t.Fatal(err)
	}
	if err := initAccessScheduleSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestLoadElevatorPinFloorAllowSetUnionFloorGroups(t *testing.T) {
	db := testAccessDB(t)
	defer db.Close()

	mustExec := func(q string, args ...any) {
		t.Helper()
		_, err := db.Exec(q, args...)
		if err != nil {
			t.Fatal(err)
		}
	}

	mustExec(`INSERT INTO access_elevators (id, display_name) VALUES ('e1','Car 1')`)
	mustExec(`INSERT INTO access_pins (pin, label, enabled) VALUES ('111111', 'u1', 1)`)
	mustExec(`INSERT INTO access_elevator_floor_groups (id, elevator_id, display_name) VALUES ('g_pub','e1','Public')`)
	mustExec(`INSERT INTO access_elevator_floor_group_members (group_id, floor_index) VALUES ('g_pub',0),('g_pub',2)`)
	mustExec(`INSERT INTO access_elevator_pin_floor_groups (pin, group_id) VALUES ('111111','g_pub')`)
	mustExec(`INSERT INTO access_elevator_pin_floors (pin, elevator_id, floor_index) VALUES ('111111','e1',5)`)

	m, has, err := loadElevatorPinFloorAllowSet(db, "111111", "e1")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("expected hasRows true (direct + group)")
	}
	for _, fi := range []int{0, 2, 5} {
		if !m[fi] {
			t.Fatalf("expected floor %d allowed", fi)
		}
	}
	if m[1] {
		t.Fatal("floor 1 should not be allowed")
	}
}

func TestElevatorFloorChannelAllowedTimedLockAndOpen(t *testing.T) {
	db := testAccessDB(t)
	defer db.Close()

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}

	mustExec(`INSERT INTO access_elevators (id, display_name) VALUES ('e1','Car 1')`)
	mustExec(`INSERT INTO access_pins (pin, label, enabled) VALUES ('222222', 'u2', 1)`)
	mustExec(`INSERT INTO access_elevator_pin_floors (pin, elevator_id, floor_index) VALUES ('222222','e1',1)`)
	mustExec(`INSERT INTO access_time_profiles (id, display_name, description, iana_timezone) VALUES ('tp1','t','','')`)
	// Monday 10:00–11:00 local
	mustExec(`INSERT INTO access_time_windows (time_profile_id, weekday, start_minute, end_minute) VALUES ('tp1',1,600,660)`)
	mustExec(`INSERT INTO access_elevator_floor_time_rules (elevator_id, floor_index, time_profile_id, action, enabled) VALUES ('e1',0,'tp1','lock',1)`)
	mustExec(`INSERT INTO access_elevator_floor_time_rules (elevator_id, floor_index, time_profile_id, action, enabled) VALUES ('e1',2,'tp1','open',1)`)

	ctx := &AppContext{DB: db, Config: DeviceConfig{}}

	// Monday 10:30 local
	mon := time.Date(2026, 4, 13, 10, 30, 0, 0, time.Local)

	if ctx.elevatorFloorChannelAllowed("222222", "e1", 0, false, mon) {
		t.Fatal("floor 0 should be locked by schedule")
	}
	if !ctx.elevatorFloorChannelAllowed("222222", "e1", 1, false, mon) {
		t.Fatal("floor 1 allowed by PIN list")
	}
	if !ctx.elevatorFloorChannelAllowed("222222", "e1", 2, false, mon) {
		t.Fatal("floor 2 should be open by schedule despite missing from PIN list")
	}
	if ctx.elevatorFloorChannelAllowed("222222", "e1", 3, false, mon) {
		t.Fatal("floor 3 not in PIN list and no open rule")
	}

	// Lock overrides open on same floor if both apply — add open+lock for floor 4
	mustExec(`INSERT INTO access_elevator_pin_floors (pin, elevator_id, floor_index) VALUES ('222222','e1',4)`)
	mustExec(`INSERT INTO access_elevator_floor_time_rules (elevator_id, floor_index, time_profile_id, action, enabled) VALUES ('e1',4,'tp1','open',1)`)
	mustExec(`INSERT INTO access_elevator_floor_time_rules (elevator_id, floor_index, time_profile_id, action, enabled) VALUES ('e1',4,'tp1','lock',1)`)
	if ctx.elevatorFloorChannelAllowed("222222", "e1", 4, false, mon) {
		t.Fatal("lock should override open when both match")
	}
}
