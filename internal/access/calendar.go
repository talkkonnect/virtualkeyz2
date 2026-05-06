package access

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/jmoiron/sqlx"
)

// MigrateTimeProfilesRespectsExceptionCalendar adds respects_exception_calendar to access_time_profiles when missing.
func MigrateTimeProfilesRespectsExceptionCalendar(db *sqlx.DB) error {
	if db == nil {
		return nil
	}
	ok, err := ScheduleTableHasColumn(db, "access_time_profiles", "respects_exception_calendar")
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE access_time_profiles ADD COLUMN respects_exception_calendar INTEGER NOT NULL DEFAULT 1`); err != nil {
		return fmt.Errorf("migrate access_time_profiles.respects_exception_calendar: %w", err)
	}
	log.Println("INFO: Added access_time_profiles.respects_exception_calendar (default 1 = apply holiday / exception calendars).")
	return nil
}

// ScheduleTableHasColumn reports whether table has a column (SQLite PRAGMA).
func ScheduleTableHasColumn(db *sqlx.DB, table, col string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
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
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ResolveExceptionCalendarState returns the effective exception for civil date y-m-d in the site's calendar.
// Multi-tier: among enabled calendars, the highest priority wins; at that tier, full_closure beats early_closure;
// multiple early_closure rows use the earliest early_close_minute (most restrictive).
func ResolveExceptionCalendarState(db *sqlx.DB, y, m, d int) (fullClosure bool, earlyEndMinute int, earlyActive bool) {
	if db == nil {
		return false, 0, false
	}
	rows, err := db.Query(`
		SELECT d.kind, d.early_close_minute, c.priority
		FROM access_exception_dates d
		INNER JOIN access_exception_calendars c ON c.id = d.calendar_id AND c.enabled = 1
		WHERE d.y = ? AND d.m = ? AND d.d = ?
		ORDER BY c.priority DESC, c.id ASC`, y, m, d)
	if err != nil {
		log.Printf("WARNING: access exception dates: %v", err)
		return false, 0, false
	}
	defer rows.Close()
	type row struct {
		kind  string
		early sql.NullInt64
		prio  int
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.early, &r.prio); err != nil {
			log.Printf("WARNING: access exception dates scan: %v", err)
			continue
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("WARNING: access exception dates: %v", err)
	}
	if len(list) == 0 {
		return false, 0, false
	}
	maxP := list[0].prio
	for _, r := range list {
		if r.prio != maxP {
			break
		}
		if strings.EqualFold(strings.TrimSpace(r.kind), "full_closure") {
			return true, 0, false
		}
	}
	minEarly := 1440
	found := false
	for _, r := range list {
		if r.prio != maxP {
			break
		}
		if !strings.EqualFold(strings.TrimSpace(r.kind), "early_closure") || !r.early.Valid {
			continue
		}
		em := int(r.early.Int64)
		if em < 0 || em > 1439 {
			continue
		}
		found = true
		if em < minEarly {
			minEarly = em
		}
	}
	if !found {
		return false, 0, false
	}
	return false, minEarly, true
}
