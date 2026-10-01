package app

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// Access scheduling (SQLite): see access_time_profiles, access_levels, access_level_targets in initAccessScheduleSchema.
//
// Model: access_doors / door_groups — door strikes (device.access_control_door_id = access_doors.id).
// access_elevators / elevator_groups — elevator banks (device.access_control_elevator_id = access_elevators.id; used in elevator_* keypad modes).
// access_user_groups + access_user_group_members — who (PIN in access_pins).
// access_time_profiles + access_time_windows — named schedules; weekday 0–6 Sun–Sat or 7 = any day; minutes 0–1439; start>end crosses midnight.
// access_exception_calendars + access_exception_dates — optional multi-tier holiday/exception calendars (priority, full day or early close). Dates use device access_exception_site_timezone. Profiles use respects_exception_calendar (default 1): when set, holidays override “standard business” windows; set 0 for 24/7-style profiles that ignore exception calendars.
// access_levels + access_level_targets — time profile + user group + exactly one target: door, door_group, elevator, or elevator_group.
//
// Example Mon–Fri 8:45–17:00 for door "east", group "staff", PIN 123456:
//
//	INSERT INTO access_doors VALUES ('east','East entry');
//	INSERT INTO access_user_groups VALUES ('staff','Staff');
//	INSERT INTO access_pins (pin,label,enabled,temporary,expires_at,max_uses,use_count) VALUES ('123456','Alice',1,0,NULL,NULL,0);
//	INSERT INTO access_user_group_members VALUES ('staff','123456');
//	INSERT INTO access_time_profiles VALUES ('biz','Standard Business','','');
//	INSERT INTO access_time_windows (time_profile_id,weekday,start_minute,end_minute) VALUES
//	  ('biz',1,525,1020),('biz',2,525,1020),('biz',3,525,1020),('biz',4,525,1020),('biz',5,525,1020);
//	INSERT INTO access_levels VALUES ('L1','Staff business hours','biz','staff',1);
//	INSERT INTO access_level_targets (access_level_id,door_id,door_group_id,elevator_id,elevator_group_id) VALUES ('L1','east',NULL,NULL,NULL);
//
// Elevator-only target example:
//
//	INSERT INTO access_elevators VALUES ('cab_a','Lobby car A');
//	INSERT INTO access_level_targets (access_level_id,door_id,door_group_id,elevator_id,elevator_group_id) VALUES ('L1',NULL,NULL,'cab_a',NULL);
//
// Per-PIN allowed floors (optional): access_elevator_pin_floors — only when device.access_control_elevator_id matches elevator_id.
// floor_index is 0-based in the same order as device.elevator_floor_input_pins / gpio.elevator_wait_floor_enable_pins / gpio.elevator_floor_dispatch_pins.
// If there are no rows for a PIN+elevator pair, all floors are allowed (backward compatible). If one or more rows exist, only listed indices are allowed.
// Bulk assignment: access_elevator_floor_groups + access_elevator_floor_group_members + access_elevator_pin_floor_groups (PIN may belong to groups; union of member floor_index values applies).
//
//	INSERT INTO access_elevator_pin_floors (pin,elevator_id,floor_index) VALUES ('123456','cab_a',0),('123456','cab_a',2);
//
// Logical labels / relay documentation: access_elevator_floor_labels — optional floor_name and relay_pin per elevator_id + floor_index (for logs and operator reference; relay_pin matches gpio expander/BCM index for that channel when set).
//
// Timed floor policy: access_elevator_floor_time_rules — per floor_index, reuse access_time_profiles + access_time_windows.
// action 'lock' denies that floor during matching windows (overrides PIN lists). action 'open' allows that floor during matching windows even when PIN would not list it (still subject to elevator access_schedule and valid credential).
//
//	INSERT INTO access_elevator_floor_labels (elevator_id,floor_index,floor_name,relay_pin) VALUES ('cab_a',0,'Lobby',5);
//	INSERT INTO access_elevator_floor_groups (id,elevator_id,display_name) VALUES ('grp_public','cab_a','Public');
//	INSERT INTO access_elevator_floor_group_members (group_id,floor_index) VALUES ('grp_public',0),('grp_public',1);
//	INSERT INTO access_elevator_pin_floor_groups (pin,group_id) VALUES ('123456','grp_public');
//	INSERT INTO access_elevator_floor_time_rules (elevator_id,floor_index,time_profile_id,action) VALUES ('cab_a',3,'nights','lock');

func accessLevelTargetsTableHasElevatorColumns(db *sqlx.DB) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(access_level_targets)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == "elevator_id" {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// migrateAccessLevelTargetsElevatorSupport rebuilds access_level_targets when upgrading from a schema
// that only had door targets, so elevator_id / elevator_group_id and the four-way CHECK apply.
func migrateAccessLevelTargetsElevatorSupport(db *sqlx.DB) error {
	ok, err := accessLevelTargetsTableHasElevatorColumns(db)
	if err != nil || ok {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmts := []string{
		`CREATE TABLE access_level_targets_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			access_level_id TEXT NOT NULL REFERENCES access_levels(id) ON DELETE CASCADE,
			door_id TEXT REFERENCES access_doors(id) ON DELETE CASCADE,
			door_group_id TEXT REFERENCES access_door_groups(id) ON DELETE CASCADE,
			elevator_id TEXT REFERENCES access_elevators(id) ON DELETE CASCADE,
			elevator_group_id TEXT REFERENCES access_elevator_groups(id) ON DELETE CASCADE,
			CHECK (
				(door_id IS NOT NULL AND door_group_id IS NULL AND elevator_id IS NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NOT NULL AND elevator_id IS NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NULL AND elevator_id IS NOT NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NULL AND elevator_id IS NULL AND elevator_group_id IS NOT NULL)
			)
		)`,
		`INSERT INTO access_level_targets_new (id, access_level_id, door_id, door_group_id, elevator_id, elevator_group_id)
			SELECT id, access_level_id, door_id, door_group_id, NULL, NULL FROM access_level_targets`,
		`DROP TABLE access_level_targets`,
		`ALTER TABLE access_level_targets_new RENAME TO access_level_targets`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migrate access_level_targets for elevators: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Println("INFO: Migrated access_level_targets for elevator access control columns.")
	return nil
}

// initAccessScheduleSchema creates tables for named time profiles, user groups, door/elevator groups, and access levels.
func initAccessScheduleSchema(db *sqlx.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("pragma foreign_keys: %w", err)
	}

	tableStmts := []string{
		`CREATE TABLE IF NOT EXISTS access_doors (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_door_groups (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_door_group_members (
			door_group_id TEXT NOT NULL REFERENCES access_door_groups(id) ON DELETE CASCADE,
			door_id TEXT NOT NULL REFERENCES access_doors(id) ON DELETE CASCADE,
			PRIMARY KEY (door_group_id, door_id)
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevators (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_groups (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_group_members (
			elevator_group_id TEXT NOT NULL REFERENCES access_elevator_groups(id) ON DELETE CASCADE,
			elevator_id TEXT NOT NULL REFERENCES access_elevators(id) ON DELETE CASCADE,
			PRIMARY KEY (elevator_group_id, elevator_id)
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_pin_floors (
			pin TEXT NOT NULL,
			elevator_id TEXT NOT NULL REFERENCES access_elevators(id) ON DELETE CASCADE,
			floor_index INTEGER NOT NULL,
			PRIMARY KEY (pin, elevator_id, floor_index),
			FOREIGN KEY (pin) REFERENCES access_pins(pin) ON DELETE CASCADE,
			CHECK (floor_index >= 0)
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_floor_labels (
			elevator_id TEXT NOT NULL REFERENCES access_elevators(id) ON DELETE CASCADE,
			floor_index INTEGER NOT NULL,
			floor_name TEXT NOT NULL,
			relay_pin INTEGER,
			PRIMARY KEY (elevator_id, floor_index),
			CHECK (floor_index >= 0)
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_floor_groups (
			id TEXT PRIMARY KEY NOT NULL,
			elevator_id TEXT NOT NULL REFERENCES access_elevators(id) ON DELETE CASCADE,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_floor_group_members (
			group_id TEXT NOT NULL REFERENCES access_elevator_floor_groups(id) ON DELETE CASCADE,
			floor_index INTEGER NOT NULL,
			PRIMARY KEY (group_id, floor_index),
			CHECK (floor_index >= 0)
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_pin_floor_groups (
			pin TEXT NOT NULL,
			group_id TEXT NOT NULL REFERENCES access_elevator_floor_groups(id) ON DELETE CASCADE,
			PRIMARY KEY (pin, group_id),
			FOREIGN KEY (pin) REFERENCES access_pins(pin) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_floor_time_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			elevator_id TEXT NOT NULL REFERENCES access_elevators(id) ON DELETE CASCADE,
			floor_index INTEGER NOT NULL,
			time_profile_id TEXT NOT NULL REFERENCES access_time_profiles(id) ON DELETE CASCADE,
			action TEXT NOT NULL CHECK (action IN ('open','lock')),
			enabled INTEGER NOT NULL DEFAULT 1,
			CHECK (floor_index >= 0)
		)`,
		`CREATE TABLE IF NOT EXISTS access_user_groups (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_user_group_members (
			group_id TEXT NOT NULL REFERENCES access_user_groups(id) ON DELETE CASCADE,
			pin TEXT NOT NULL,
			PRIMARY KEY (group_id, pin),
			FOREIGN KEY (pin) REFERENCES access_pins(pin) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS access_time_profiles (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT,
			description TEXT,
			iana_timezone TEXT NOT NULL DEFAULT '',
			respects_exception_calendar INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS access_time_windows (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time_profile_id TEXT NOT NULL REFERENCES access_time_profiles(id) ON DELETE CASCADE,
			weekday INTEGER NOT NULL,
			start_minute INTEGER NOT NULL,
			end_minute INTEGER NOT NULL,
			CHECK (weekday >= 0 AND weekday <= 7),
			CHECK (start_minute >= 0 AND start_minute <= 1439),
			CHECK (end_minute >= 0 AND end_minute <= 1439)
		)`,
		`CREATE TABLE IF NOT EXISTS access_levels (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT,
			time_profile_id TEXT NOT NULL REFERENCES access_time_profiles(id),
			user_group_id TEXT NOT NULL REFERENCES access_user_groups(id),
			enabled INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS access_level_targets (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			access_level_id TEXT NOT NULL REFERENCES access_levels(id) ON DELETE CASCADE,
			door_id TEXT REFERENCES access_doors(id) ON DELETE CASCADE,
			door_group_id TEXT REFERENCES access_door_groups(id) ON DELETE CASCADE,
			elevator_id TEXT REFERENCES access_elevators(id) ON DELETE CASCADE,
			elevator_group_id TEXT REFERENCES access_elevator_groups(id) ON DELETE CASCADE,
			CHECK (
				(door_id IS NOT NULL AND door_group_id IS NULL AND elevator_id IS NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NOT NULL AND elevator_id IS NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NULL AND elevator_id IS NOT NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NULL AND elevator_id IS NULL AND elevator_group_id IS NOT NULL)
			)
		)`,
		`CREATE TABLE IF NOT EXISTS access_exception_calendars (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT,
			priority INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			source_note TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_exception_dates (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			calendar_id TEXT NOT NULL REFERENCES access_exception_calendars(id) ON DELETE CASCADE,
			y INTEGER NOT NULL,
			m INTEGER NOT NULL,
			d INTEGER NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('full_closure','early_closure')),
			early_close_minute INTEGER,
			label TEXT,
			UNIQUE (calendar_id, y, m, d),
			CHECK (y >= 1 AND y <= 9999 AND m >= 1 AND m <= 12 AND d >= 1 AND d <= 31),
			CHECK (
				(kind = 'early_closure' AND early_close_minute IS NOT NULL AND early_close_minute >= 0 AND early_close_minute <= 1439)
				OR (kind = 'full_closure' AND early_close_minute IS NULL)
			)
		)`,
	}
	for _, q := range tableStmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("access schedule schema: %w", err)
		}
	}
	if err := migrateAccessLevelTargetsElevatorSupport(db); err != nil {
		return fmt.Errorf("access schedule schema: %w", err)
	}
	if err := migrateAccessTimeProfilesRespectsExceptionCalendar(db); err != nil {
		return fmt.Errorf("access schedule schema: %w", err)
	}
	indexStmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_access_level_targets_level ON access_level_targets(access_level_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_level_targets_door ON access_level_targets(door_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_level_targets_elevator ON access_level_targets(elevator_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_level_targets_door_group ON access_level_targets(door_group_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_level_targets_elevator_group ON access_level_targets(elevator_group_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_levels_user_group ON access_levels(user_group_id, enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_access_elevator_pin_floors_lookup ON access_elevator_pin_floors(elevator_id, pin)`,
		`CREATE INDEX IF NOT EXISTS idx_access_elevator_floor_groups_elevator ON access_elevator_floor_groups(elevator_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_elevator_pin_floor_groups_pin ON access_elevator_pin_floor_groups(pin)`,
		`CREATE INDEX IF NOT EXISTS idx_access_elevator_floor_time_rules_lookup ON access_elevator_floor_time_rules(elevator_id, floor_index, enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_access_time_windows_profile ON access_time_windows(time_profile_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_user_group_members_pin ON access_user_group_members(pin)`,
		`CREATE INDEX IF NOT EXISTS idx_access_exception_dates_ymd ON access_exception_dates(y, m, d)`,
		`CREATE INDEX IF NOT EXISTS idx_access_exception_dates_cal ON access_exception_dates(calendar_id)`,
	}
	for _, q := range indexStmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("access schedule schema: %w", err)
		}
	}
	log.Println("INFO: SQLite access schedule tables ready (doors, elevators, time profiles, user groups, access levels).")
	return nil
}

// accessScheduleLocations caches time.LoadLocation results (which read zoneinfo from disk on every
// call) for the IANA names used by schedules; invalid names cache time.Local.
var accessScheduleLocations sync.Map // string -> *time.Location

func accessScheduleTimeLocation(iana string) *time.Location {
	s := strings.TrimSpace(iana)
	if s == "" {
		return time.Local
	}
	if loc, ok := accessScheduleLocations.Load(s); ok {
		return loc.(*time.Location)
	}
	loc, err := time.LoadLocation(s)
	if err != nil {
		log.Printf("WARNING: access_time_profiles.iana_timezone %q invalid (%v); using local time.", s, err)
		loc = time.Local
	}
	accessScheduleLocations.Store(s, loc)
	return loc
}

// minuteMatchesWindow reports whether minute-of-day m is inside [start, end] inclusive.
// If start > end, the window crosses midnight (e.g. 22:00–06:00).
func minuteMatchesWindow(m, start, end int) bool {
	if start <= end {
		return m >= start && m <= end
	}
	return m >= start || m <= end
}

// timeMatchesProfileWindows returns true if t (already in the profile location) matches any window.
// weekday is Go's time.Weekday() (Sunday=0). SQL weekday 7 means "any day".
func timeMatchesProfileWindows(weekday time.Weekday, minuteOfDay int, rows []struct {
	weekday    int
	start, end int
}) bool {
	wd := int(weekday)
	for _, r := range rows {
		w := r.weekday
		if w != 7 && w != wd {
			continue
		}
		if minuteMatchesWindow(minuteOfDay, r.start, r.end) {
			return true
		}
	}
	return false
}

func accessScheduleHasTargetsForDoor(db *sqlx.DB, doorID string) (bool, error) {
	if db == nil || strings.TrimSpace(doorID) == "" {
		return false, nil
	}
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT 1
			FROM access_levels al
			INNER JOIN access_level_targets t ON t.access_level_id = al.id
			WHERE al.enabled = 1 AND (
				t.door_id = ?
				OR EXISTS (
					SELECT 1 FROM access_door_group_members dgm
					WHERE dgm.door_group_id = t.door_group_id AND dgm.door_id = ?
				)
			)
			LIMIT 1
		)`, doorID, doorID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// accessScheduleAllows returns whether PIN may open the given door at atTime under schedule rules.
// When scheduling does not apply, returns (true, "").
func (ctx *AppContext) accessScheduleAllows(pin, doorID string, atTime time.Time, viaFallback bool) (bool, string) {
	pin = strings.TrimSpace(pin)
	doorID = strings.TrimSpace(doorID)
	ctx.configMu.RLock()
	enforce := ctx.Config.AccessScheduleEnforce
	applyFallback := ctx.Config.AccessScheduleApplyToFallbackPin
	ctx.configMu.RUnlock()

	if ctx.DB == nil || doorID == "" || !enforce {
		return true, ""
	}
	if ctx.FiremansServiceActive() {
		return true, ""
	}
	if viaFallback && !applyFallback {
		return true, ""
	}
	hasRules, err := accessScheduleHasTargetsForDoor(ctx.DB, doorID)
	if err != nil {
		log.Printf("WARNING: access schedule door target check: %v", err)
		return false, "schedule_db_error"
	}
	if !hasRules {
		return true, ""
	}

	siteLoc := ctx.accessExceptionSiteLocation()
	cy, cm, cd := civilDateInLocation(atTime, siteLoc)
	fullExc, earlyEnd, earlyActive := resolveAccessExceptionCalendarState(ctx.DB, cy, cm, cd)

	rows, err := ctx.DB.Query(`
		SELECT DISTINCT al.time_profile_id, tp.iana_timezone, COALESCE(tp.respects_exception_calendar, 1)
		FROM access_levels al
		INNER JOIN access_time_profiles tp ON tp.id = al.time_profile_id
		INNER JOIN access_level_targets t ON t.access_level_id = al.id
		INNER JOIN access_user_group_members ugm ON ugm.group_id = al.user_group_id AND ugm.pin = ?
		WHERE al.enabled = 1 AND (
			t.door_id = ?
			OR EXISTS (
				SELECT 1 FROM access_door_group_members dgm
				WHERE dgm.door_group_id = t.door_group_id AND dgm.door_id = ?
			)
		)`, pin, doorID, doorID)
	if err != nil {
		log.Printf("WARNING: access schedule level query: %v", err)
		return false, "schedule_db_error"
	}
	defer rows.Close()

	type profTZ struct {
		id          string
		tz          string
		key         string
		respectsExc bool
	}
	var list []profTZ
	for rows.Next() {
		var pid, iana string
		var respects sql.NullInt64
		if err := rows.Scan(&pid, &iana, &respects); err != nil {
			log.Printf("WARNING: access schedule scan: %v", err)
			continue
		}
		rf := true
		if respects.Valid {
			rf = respects.Int64 != 0
		}
		pid = strings.TrimSpace(pid)
		iana = strings.TrimSpace(iana)
		list = append(list, profTZ{id: pid, tz: iana, key: pid + "\x00" + iana, respectsExc: rf})
	}
	if err := rows.Err(); err != nil {
		return false, "schedule_db_error"
	}
	if len(list) == 0 {
		return false, "no_access_level_for_credential"
	}

	seen := make(map[string]struct{})
	for _, pt := range list {
		if _, ok := seen[pt.key]; ok {
			continue
		}
		seen[pt.key] = struct{}{}

		loc := accessScheduleTimeLocation(pt.tz)
		tLocal := atTime.In(loc)
		minuteOfDay := tLocal.Hour()*60 + tLocal.Minute()
		wd := tLocal.Weekday()

		wrows, err := ctx.DB.Query(`
			SELECT weekday, start_minute, end_minute
			FROM access_time_windows
			WHERE time_profile_id = ?
			ORDER BY id`, pt.id)
		if err != nil {
			log.Printf("WARNING: access schedule windows: %v", err)
			return false, "schedule_db_error"
		}
		var wins []struct {
			weekday    int
			start, end int
		}
		for wrows.Next() {
			var wk, sm, em int
			if err := wrows.Scan(&wk, &sm, &em); err != nil {
				_ = wrows.Close()
				return false, "schedule_db_error"
			}
			wins = append(wins, struct {
				weekday    int
				start, end int
			}{wk, sm, em})
		}
		if err := wrows.Close(); err != nil {
			return false, "schedule_db_error"
		}

		if len(wins) == 0 {
			continue
		}
		matchesBase := timeMatchesProfileWindows(wd, minuteOfDay, wins)
		matchesExc := timeMatchesProfileWindowsWithExceptions(wd, minuteOfDay, wins, pt.respectsExc, fullExc, earlyActive, earlyEnd)
		if matchesExc {
			return true, ""
		}
		if fullExc && pt.respectsExc && matchesBase && !matchesExc {
			return false, "holiday_closure"
		}
	}

	return false, "outside_scheduled_hours"
}

func accessScheduleHasTargetsForElevator(db *sqlx.DB, elevatorID string) (bool, error) {
	if db == nil || strings.TrimSpace(elevatorID) == "" {
		return false, nil
	}
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT 1
			FROM access_levels al
			INNER JOIN access_level_targets t ON t.access_level_id = al.id
			WHERE al.enabled = 1 AND (
				t.elevator_id = ?
				OR EXISTS (
					SELECT 1 FROM access_elevator_group_members egm
					WHERE egm.elevator_group_id = t.elevator_group_id AND egm.elevator_id = ?
				)
			)
			LIMIT 1
		)`, elevatorID, elevatorID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// accessScheduleAllowsElevator returns whether PIN may use elevator control at atTime under schedule rules.
// When scheduling does not apply, returns (true, "").
func (ctx *AppContext) accessScheduleAllowsElevator(pin, elevatorID string, atTime time.Time, viaFallback bool) (bool, string) {
	pin = strings.TrimSpace(pin)
	elevatorID = strings.TrimSpace(elevatorID)
	ctx.configMu.RLock()
	enforce := ctx.Config.AccessScheduleEnforce
	applyFallback := ctx.Config.AccessScheduleApplyToFallbackPin
	ctx.configMu.RUnlock()

	if ctx.DB == nil || elevatorID == "" || !enforce {
		return true, ""
	}
	if ctx.FiremansServiceActive() {
		return true, ""
	}
	if viaFallback && !applyFallback {
		return true, ""
	}
	hasRules, err := accessScheduleHasTargetsForElevator(ctx.DB, elevatorID)
	if err != nil {
		log.Printf("WARNING: access schedule elevator target check: %v", err)
		return false, "schedule_db_error"
	}
	if !hasRules {
		return true, ""
	}

	siteLoc := ctx.accessExceptionSiteLocation()
	cy, cm, cd := civilDateInLocation(atTime, siteLoc)
	fullExc, earlyEnd, earlyActive := resolveAccessExceptionCalendarState(ctx.DB, cy, cm, cd)

	rows, err := ctx.DB.Query(`
		SELECT DISTINCT al.time_profile_id, tp.iana_timezone, COALESCE(tp.respects_exception_calendar, 1)
		FROM access_levels al
		INNER JOIN access_time_profiles tp ON tp.id = al.time_profile_id
		INNER JOIN access_level_targets t ON t.access_level_id = al.id
		INNER JOIN access_user_group_members ugm ON ugm.group_id = al.user_group_id AND ugm.pin = ?
		WHERE al.enabled = 1 AND (
			t.elevator_id = ?
			OR EXISTS (
				SELECT 1 FROM access_elevator_group_members egm
				WHERE egm.elevator_group_id = t.elevator_group_id AND egm.elevator_id = ?
			)
		)`, pin, elevatorID, elevatorID)
	if err != nil {
		log.Printf("WARNING: access schedule elevator level query: %v", err)
		return false, "schedule_db_error"
	}
	defer rows.Close()

	type profTZ struct {
		id          string
		tz          string
		key         string
		respectsExc bool
	}
	var list []profTZ
	for rows.Next() {
		var pid, iana string
		var respects sql.NullInt64
		if err := rows.Scan(&pid, &iana, &respects); err != nil {
			log.Printf("WARNING: access schedule elevator scan: %v", err)
			continue
		}
		rf := true
		if respects.Valid {
			rf = respects.Int64 != 0
		}
		pid = strings.TrimSpace(pid)
		iana = strings.TrimSpace(iana)
		list = append(list, profTZ{id: pid, tz: iana, key: pid + "\x00" + iana, respectsExc: rf})
	}
	if err := rows.Err(); err != nil {
		return false, "schedule_db_error"
	}
	if len(list) == 0 {
		return false, "no_access_level_for_credential"
	}

	seen := make(map[string]struct{})
	for _, pt := range list {
		if _, ok := seen[pt.key]; ok {
			continue
		}
		seen[pt.key] = struct{}{}

		loc := accessScheduleTimeLocation(pt.tz)
		tLocal := atTime.In(loc)
		minuteOfDay := tLocal.Hour()*60 + tLocal.Minute()
		wd := tLocal.Weekday()

		wrows, err := ctx.DB.Query(`
			SELECT weekday, start_minute, end_minute
			FROM access_time_windows
			WHERE time_profile_id = ?
			ORDER BY id`, pt.id)
		if err != nil {
			log.Printf("WARNING: access schedule elevator windows: %v", err)
			return false, "schedule_db_error"
		}
		var wins []struct {
			weekday    int
			start, end int
		}
		for wrows.Next() {
			var wk, sm, em int
			if err := wrows.Scan(&wk, &sm, &em); err != nil {
				_ = wrows.Close()
				return false, "schedule_db_error"
			}
			wins = append(wins, struct {
				weekday    int
				start, end int
			}{wk, sm, em})
		}
		if err := wrows.Close(); err != nil {
			return false, "schedule_db_error"
		}

		if len(wins) == 0 {
			continue
		}
		matchesBase := timeMatchesProfileWindows(wd, minuteOfDay, wins)
		matchesExc := timeMatchesProfileWindowsWithExceptions(wd, minuteOfDay, wins, pt.respectsExc, fullExc, earlyActive, earlyEnd)
		if matchesExc {
			return true, ""
		}
		if fullExc && pt.respectsExc && matchesBase && !matchesExc {
			return false, "holiday_closure"
		}
	}

	return false, "outside_scheduled_hours"
}

func (ctx *AppContext) effectiveAccessDoorID() string {
	ctx.configMu.RLock()
	defer ctx.configMu.RUnlock()
	return strings.TrimSpace(ctx.Config.AccessControlDoorID)
}

func (ctx *AppContext) effectiveAccessElevatorID() string {
	ctx.configMu.RLock()
	defer ctx.configMu.RUnlock()
	return strings.TrimSpace(ctx.Config.AccessControlElevatorID)
}

// loadElevatorPinFloorAllowSet reads per-floor allow list for this PIN and elevator from
// access_elevator_pin_floors and from access_elevator_pin_floor_groups (union of group members).
// When hasRows is false, the caller should treat the credential as unrestricted for floors (PIN-only rules).
func loadElevatorPinFloorAllowSet(db *sqlx.DB, pin, elevatorID string) (map[int]bool, bool, error) {
	pin = strings.TrimSpace(pin)
	elevatorID = strings.TrimSpace(elevatorID)
	if db == nil || pin == "" || elevatorID == "" {
		return nil, false, nil
	}
	rows, err := db.Query(`
		SELECT floor_index FROM access_elevator_pin_floors
		WHERE pin = ? AND elevator_id = ?
		UNION
		SELECT m.floor_index FROM access_elevator_pin_floor_groups pfg
		INNER JOIN access_elevator_floor_groups g ON g.id = pfg.group_id AND g.elevator_id = ?
		INNER JOIN access_elevator_floor_group_members m ON m.group_id = g.id
		WHERE pfg.pin = ?
		ORDER BY floor_index`, pin, elevatorID, elevatorID, pin)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	m := make(map[int]bool)
	for rows.Next() {
		var fi int
		if err := rows.Scan(&fi); err != nil {
			return nil, false, err
		}
		if fi >= 0 {
			m[fi] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return m, len(m) > 0, nil
}

// elevatorPinMayUseFloor enforces access_elevator_pin_floors when device.access_control_elevator_id is set
// and there is at least one row for this PIN+elevator. Fallback PIN behavior matches access_schedule_apply_to_fallback_pin.
func (ctx *AppContext) elevatorPinMayUseFloor(pin, elevatorID string, floorIndex int, viaFallback bool) bool {
	pin = strings.TrimSpace(pin)
	elevatorID = strings.TrimSpace(elevatorID)
	if ctx.DB == nil || elevatorID == "" || floorIndex < 0 {
		return true
	}
	ctx.configMu.RLock()
	applyFallback := ctx.Config.AccessScheduleApplyToFallbackPin
	ctx.configMu.RUnlock()
	if viaFallback && !applyFallback {
		return true
	}
	m, hasRows, err := loadElevatorPinFloorAllowSet(ctx.DB, pin, elevatorID)
	if err != nil || !hasRows {
		return true
	}
	return m[floorIndex]
}

// elevatorFloorTimedPolicy reports whether active time windows mark the floor locked and/or open.
// lock takes precedence in elevatorFloorChannelAllowed. open allows bypass of PIN floor lists only.
func (ctx *AppContext) elevatorFloorTimedPolicy(elevatorID string, floorIndex int, at time.Time) (locked, openActive bool) {
	elevatorID = strings.TrimSpace(elevatorID)
	if ctx.DB == nil || elevatorID == "" || floorIndex < 0 {
		return false, false
	}
	siteLoc := ctx.accessExceptionSiteLocation()
	cy, cm, cd := civilDateInLocation(at, siteLoc)
	fullExc, earlyEnd, earlyActive := resolveAccessExceptionCalendarState(ctx.DB, cy, cm, cd)

	rows, err := ctx.DB.Query(`
		SELECT r.action, tp.iana_timezone, tw.weekday, tw.start_minute, tw.end_minute, COALESCE(tp.respects_exception_calendar, 1)
		FROM access_elevator_floor_time_rules r
		INNER JOIN access_time_profiles tp ON tp.id = r.time_profile_id
		INNER JOIN access_time_windows tw ON tw.time_profile_id = tp.id
		WHERE r.enabled = 1 AND r.elevator_id = ? AND r.floor_index = ?
		ORDER BY r.id, tw.id`, elevatorID, floorIndex)
	if err != nil {
		log.Printf("WARNING: access_elevator_floor_time_rules: %v", err)
		return false, false
	}
	defer rows.Close()
	for rows.Next() {
		var action, iana string
		var wk, sm, em int
		var respects sql.NullInt64
		if err := rows.Scan(&action, &iana, &wk, &sm, &em, &respects); err != nil {
			log.Printf("WARNING: access_elevator_floor_time_rules scan: %v", err)
			continue
		}
		respectsExc := !respects.Valid || respects.Int64 != 0
		loc := accessScheduleTimeLocation(iana)
		tLocal := at.In(loc)
		minuteOfDay := tLocal.Hour()*60 + tLocal.Minute()
		wd := tLocal.Weekday()
		wins := []struct {
			weekday    int
			start, end int
		}{{wk, sm, em}}
		if !timeMatchesProfileWindowsWithExceptions(wd, minuteOfDay, wins, respectsExc, fullExc, earlyActive, earlyEnd) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "lock":
			locked = true
		case "open":
			openActive = true
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("WARNING: access_elevator_floor_time_rules: %v", err)
	}
	return locked, openActive
}

// elevatorFloorChannelAllowed is the full per-floor check: timed lock/open rules, then PIN floor list (and groups).
func (ctx *AppContext) elevatorFloorChannelAllowed(pin, elevatorID string, floorIndex int, viaFallback bool, at time.Time) bool {
	pin = strings.TrimSpace(pin)
	elevatorID = strings.TrimSpace(elevatorID)
	if ctx.FiremansServiceActive() {
		return true
	}
	if ctx.elevatorStaticTestFloorACLBypass.Load() {
		return true
	}
	if ctx.DB == nil || elevatorID == "" || floorIndex < 0 {
		return true
	}
	locked, openWin := ctx.elevatorFloorTimedPolicy(elevatorID, floorIndex, at)
	if locked {
		return false
	}
	if openWin {
		return true
	}
	return ctx.elevatorPinMayUseFloor(pin, elevatorID, floorIndex, viaFallback)
}

// elevatorFloorLogLabel returns a short label for logs/webhooks: "name [index N]" or "index N".
func elevatorFloorLogLabel(db *sqlx.DB, elevatorID string, floorIndex int) string {
	elevatorID = strings.TrimSpace(elevatorID)
	if db == nil || elevatorID == "" || floorIndex < 0 {
		return fmt.Sprintf("index %d", floorIndex)
	}
	var name sql.NullString
	err := db.QueryRow(`
		SELECT floor_name FROM access_elevator_floor_labels
		WHERE elevator_id = ? AND floor_index = ?`, elevatorID, floorIndex).Scan(&name)
	if err != nil || !name.Valid {
		return fmt.Sprintf("index %d", floorIndex)
	}
	n := strings.TrimSpace(name.String)
	if n == "" {
		return fmt.Sprintf("index %d", floorIndex)
	}
	return fmt.Sprintf("%q [index %d]", n, floorIndex)
}

func elevatorFloorLogLabels(db *sqlx.DB, elevatorID string, indices []int) []string {
	out := make([]string, 0, len(indices))
	for _, fi := range indices {
		out = append(out, elevatorFloorLogLabel(db, elevatorID, fi))
	}
	return out
}

// elevatorPredefinedDispatchIndexForACL returns the 0-based floor index used for access_elevator_pin_floors and dispatch wiring.
func (ctx *AppContext) elevatorPredefinedDispatchIndexForACL(cfg DeviceConfig) int {
	nf := len(cfg.ElevatorPredefinedFloors)
	nDisp := len(ctx.elevatorFloorDispatchPins)
	idx := cfg.ElevatorPredefinedFloor
	if nf == 0 {
		if nDisp > 0 {
			if idx < 0 {
				idx = 0
			}
			if idx >= nDisp {
				idx = nDisp - 1
			}
		}
		return idx
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= nf {
		idx = nf - 1
	}
	return idx
}
