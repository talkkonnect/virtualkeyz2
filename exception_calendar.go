package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// migrateAccessTimeProfilesRespectsExceptionCalendar adds respects_exception_calendar to access_time_profiles when missing.
func migrateAccessTimeProfilesRespectsExceptionCalendar(db *sqlx.DB) error {
	if db == nil {
		return nil
	}
	ok, err := accessScheduleTableHasColumn(db, "access_time_profiles", "respects_exception_calendar")
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

func accessScheduleTableHasColumn(db *sqlx.DB, table, col string) (bool, error) {
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

// resolveAccessExceptionCalendarState returns the effective exception for civil date y-m-d in the site's calendar.
// Multi-tier: among enabled calendars, the highest priority wins; at that tier, full_closure beats early_closure;
// multiple early_closure rows use the earliest early_close_minute (most restrictive).
func resolveAccessExceptionCalendarState(db *sqlx.DB, y, m, d int) (fullClosure bool, earlyEndMinute int, earlyActive bool) {
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

// timeMatchesProfileWindowsWithExceptions extends weekly windows with site exception calendars.
// When respectsExceptions is false, behavior matches timeMatchesProfileWindows.
// fullClosure forces deny for profiles that respect exceptions (secured / weekend-equivalent for that day).
// earlyActive with earlyEndMinute clips same-day windows [start,end] to end at earlyEndMinute (inclusive).
func timeMatchesProfileWindowsWithExceptions(wd time.Weekday, minuteOfDay int, rows []struct {
	weekday    int
	start, end int
}, respectsExceptions bool, fullClosure bool, earlyActive bool, earlyEndMinute int) bool {
	if !respectsExceptions {
		return timeMatchesProfileWindows(wd, minuteOfDay, rows)
	}
	if fullClosure {
		return false
	}
	wdGo := int(wd)
	for _, r := range rows {
		w := r.weekday
		if w != 7 && w != wdGo {
			continue
		}
		sm, em := r.start, r.end
		if earlyActive {
			if sm <= em {
				if earlyEndMinute < sm {
					continue
				}
				if em > earlyEndMinute {
					em = earlyEndMinute
				}
			}
		}
		if minuteMatchesWindow(minuteOfDay, sm, em) {
			return true
		}
	}
	return false
}

func (ctx *AppContext) accessExceptionSiteLocation() *time.Location {
	if ctx == nil {
		return time.Local
	}
	ctx.configMu.RLock()
	tz := strings.TrimSpace(ctx.Config.AccessExceptionSiteTimezone)
	ctx.configMu.RUnlock()
	return accessScheduleTimeLocation(tz)
}

func civilDateInLocation(t time.Time, loc *time.Location) (y, m, d int) {
	if loc == nil {
		loc = time.Local
	}
	c := t.In(loc)
	return c.Year(), int(c.Month()), c.Day()
}

// techMenuACLCmdException manages multi-tier exception (holiday) calendars: full-day closure or early close.
func techMenuACLCmdException(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf(`usage: acl exception calendar|date|import …`)
	}
	kind := strings.ToLower(strings.TrimSpace(parts[2]))
	switch kind {
	case "calendar":
		return techMenuACLCmdExceptionCalendar(ctx, parts)
	case "date":
		return techMenuACLCmdExceptionDate(ctx, parts)
	case "import":
		return techMenuACLCmdExceptionImport(ctx, parts)
	default:
		return fmt.Errorf("exception: use calendar, date, or import — try: acl help")
	}
}

func techMenuACLCmdExceptionCalendar(ctx *AppContext, parts []string) error {
	if len(parts) < 4 {
		return fmt.Errorf("usage: acl exception calendar add <id> [display_name [priority [source_note…]]] | acl exception calendar list")
	}
	verb := strings.ToLower(strings.TrimSpace(parts[3]))
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_exception_calendars", "id", "display_name", "priority", "enabled", "source_note")
	case "add":
		if len(parts) < 5 {
			return fmt.Errorf("usage: acl exception calendar add <id> [display_name [priority [source_note…]]]")
		}
		id := strings.TrimSpace(parts[4])
		if id == "" {
			return fmt.Errorf("calendar id must not be empty")
		}
		display := ""
		prio := 0
		note := ""
		switch len(parts) {
		case 5:
			break
		case 6:
			display = strings.TrimSpace(parts[5])
		case 7:
			display = strings.TrimSpace(parts[5])
			var err error
			prio, err = strconv.Atoi(strings.TrimSpace(parts[6]))
			if err != nil {
				return fmt.Errorf("priority: integer: %w", err)
			}
		default:
			display = strings.TrimSpace(parts[5])
			var err error
			prio, err = strconv.Atoi(strings.TrimSpace(parts[6]))
			if err != nil {
				return fmt.Errorf("priority: integer: %w", err)
			}
			note = strings.TrimSpace(strings.Join(parts[7:], " "))
		}
		_, err := ctx.DB.Exec(`
			INSERT INTO access_exception_calendars (id, display_name, priority, enabled, source_note)
			VALUES (?, ?, ?, 1, ?)
			ON CONFLICT(id) DO UPDATE SET
				display_name = excluded.display_name,
				priority = excluded.priority,
				source_note = excluded.source_note`,
			id, nullStringIfEmpty(display), prio, nullStringIfEmpty(note))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl exception calendar add %q priority=%d", id, prio)
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Exception calendar %q saved (enabled). Add dates: acl exception date add %s <YYYY-MM-DD> full|early …\n", id, id)
		})
		return nil
	default:
		return fmt.Errorf("calendar: use add or list")
	}
}

func nullStringIfEmpty(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

func techMenuACLCmdExceptionDate(ctx *AppContext, parts []string) error {
	if len(parts) < 4 {
		return fmt.Errorf("usage: acl exception date add … | list [calendar_id] | delete <row_id>")
	}
	verb := strings.ToLower(strings.TrimSpace(parts[3]))
	switch verb {
	case "list":
		cal := ""
		if len(parts) > 4 {
			cal = strings.TrimSpace(parts[4])
		}
		if cal == "" {
			rows, err := ctx.DB.Query(`
				SELECT id, calendar_id, y, m, d, kind, early_close_minute, label
				FROM access_exception_dates ORDER BY y, m, d, calendar_id`)
			if err != nil {
				return err
			}
			defer rows.Close()
			techMenuSyncPrint(func(w io.Writer) {
				fmt.Fprintln(w, "id | calendar_id | y | m | d | kind | early_close_minute | label")
				for rows.Next() {
					var id int64
					var cid string
					var y, m, d int
					var kind string
					var lab sql.NullString
					var early sql.NullInt64
					if err := rows.Scan(&id, &cid, &y, &m, &d, &kind, &early, &lab); err != nil {
						fmt.Fprintf(w, "(scan error: %v)\n", err)
						return
					}
					fmt.Fprintf(w, "%d | %s | %d | %d | %d | %s | %v | %v\n", id, cid, y, m, d, kind, early, lab)
				}
			})
			return rows.Err()
		}
		rows, err := ctx.DB.Query(`
			SELECT id, calendar_id, y, m, d, kind, early_close_minute, label
			FROM access_exception_dates WHERE calendar_id = ? ORDER BY y, m, d`, cal)
		if err != nil {
			return err
		}
		defer rows.Close()
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "id | calendar_id | y | m | d | kind | early_close_minute | label")
			for rows.Next() {
				var id int64
				var cid string
				var y, m, d int
				var kind string
				var lab sql.NullString
				var early sql.NullInt64
				if err := rows.Scan(&id, &cid, &y, &m, &d, &kind, &early, &lab); err != nil {
					fmt.Fprintf(w, "(scan error: %v)\n", err)
					return
				}
				fmt.Fprintf(w, "%d | %s | %d | %d | %d | %s | %v | %v\n", id, cid, y, m, d, kind, early, lab)
			}
		})
		return rows.Err()
	case "delete":
		if len(parts) < 5 {
			return fmt.Errorf("usage: acl exception date delete <row_id>")
		}
		rid := strings.TrimSpace(parts[4])
		res, err := ctx.DB.Exec(`DELETE FROM access_exception_dates WHERE id = ?`, rid)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("no exception date with id %q — acl exception date list", rid)
		}
		log.Printf("INFO: Technician menu: acl exception date delete id=%s", rid)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Deleted exception date row %q\n", rid) })
		return nil
	case "add":
		// acl exception date add <calendar_id> <YYYY-MM-DD> full [label...]
		// acl exception date add <calendar_id> <YYYY-MM-DD> early <minute> [label...]
		if len(parts) < 7 {
			return fmt.Errorf(`usage:
  acl exception date add <calendar_id> <YYYY-MM-DD> full [label...]
  acl exception date add <calendar_id> <YYYY-MM-DD> early <minute> [label...]`)
		}
		calID := strings.TrimSpace(parts[4])
		ds := strings.TrimSpace(parts[5])
		mode := strings.ToLower(strings.TrimSpace(parts[6]))
		if calID == "" || ds == "" {
			return fmt.Errorf("calendar_id and date must not be empty")
		}
		dt, err := time.ParseInLocation("2006-01-02", ds, time.Local)
		if err != nil {
			return fmt.Errorf("date: use YYYY-MM-DD: %w", err)
		}
		y, mth, d := dt.Date()
		if err := techMenuACLEnsureFK(ctx, "access_exception_calendars", "id", calID, "exception calendar"); err != nil {
			return err
		}
		switch mode {
		case "full", "full_closure":
			lbl := ""
			if len(parts) > 7 {
				lbl = strings.TrimSpace(strings.Join(parts[7:], " "))
			}
			_, err := ctx.DB.Exec(`
				INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute, label)
				VALUES (?, ?, ?, ?, 'full_closure', NULL, ?)
				ON CONFLICT(calendar_id, y, m, d) DO UPDATE SET
					kind = excluded.kind,
					early_close_minute = excluded.early_close_minute,
					label = excluded.label`,
				calID, y, int(mth), d, nullStringIfEmpty(lbl))
			if err != nil {
				return err
			}
		case "early", "early_closure":
			if len(parts) < 8 {
				return fmt.Errorf("usage: acl exception date add <cal> <date> early <minute_0_1439> [label]")
			}
			em, err := strconv.Atoi(strings.TrimSpace(parts[7]))
			if err != nil || em < 0 || em > 1439 {
				return fmt.Errorf("early close minute must be 0–1439")
			}
			lbl := ""
			if len(parts) > 8 {
				lbl = strings.TrimSpace(strings.Join(parts[8:], " "))
			}
			_, err = ctx.DB.Exec(`
				INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute, label)
				VALUES (?, ?, ?, ?, 'early_closure', ?, ?)
				ON CONFLICT(calendar_id, y, m, d) DO UPDATE SET
					kind = excluded.kind,
					early_close_minute = excluded.early_close_minute,
					label = excluded.label`,
				calID, y, int(mth), d, em, nullStringIfEmpty(lbl))
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("date kind: use full or early")
		}
		log.Printf("INFO: Technician menu: acl exception date add cal=%s %04d-%02d-%02d %s", calID, y, mth, d, mode)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Exception date saved.") })
		return nil
	default:
		return fmt.Errorf("date: use add, list, or delete")
	}
}

func techMenuACLCmdExceptionImport(ctx *AppContext, parts []string) error {
	if len(parts) < 5 {
		return fmt.Errorf("usage: acl exception import <calendar_id> <file_path>")
	}
	calID := strings.TrimSpace(parts[3])
	path := strings.TrimSpace(parts[4])
	if calID == "" || path == "" {
		return fmt.Errorf("calendar_id and file_path must not be empty")
	}
	if err := techMenuACLEnsureFK(ctx, "access_exception_calendars", "id", calID, "exception calendar"); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rec, err := parseExceptionImportLine(line)
		if err != nil {
			return fmt.Errorf("%s line %d: %w", path, lineNo, err)
		}
		if rec == nil {
			continue
		}
		y, mth, d := rec.at.Date()
		if rec.full {
			_, err = ctx.DB.Exec(`
				INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute, label)
				VALUES (?, ?, ?, ?, 'full_closure', NULL, ?)
				ON CONFLICT(calendar_id, y, m, d) DO UPDATE SET
					kind = excluded.kind,
					early_close_minute = excluded.early_close_minute,
					label = excluded.label`,
				calID, y, int(mth), d, nullStringIfEmpty(rec.label))
		} else {
			_, err = ctx.DB.Exec(`
				INSERT INTO access_exception_dates (calendar_id, y, m, d, kind, early_close_minute, label)
				VALUES (?, ?, ?, ?, 'early_closure', ?, ?)
				ON CONFLICT(calendar_id, y, m, d) DO UPDATE SET
					kind = excluded.kind,
					early_close_minute = excluded.early_close_minute,
					label = excluded.label`,
				calID, y, int(mth), d, rec.earlyMin, nullStringIfEmpty(rec.label))
		}
		if err != nil {
			return fmt.Errorf("%s line %d: %w", path, lineNo, err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return err
	}
	log.Printf("INFO: Technician menu: acl exception import %q -> calendar %q (%d rows)", path, calID, n)
	techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Imported %d exception date(s) into calendar %q\n", n, calID) })
	return nil
}

type exceptionImportRec struct {
	at       time.Time
	full     bool
	earlyMin int
	label    string
}

func parseExceptionImportLine(line string) (*exceptionImportRec, error) {
	// CSV: YYYY-MM-DD,full[,label]  or  YYYY-MM-DD,early,<minute>[,label]
	parts := strings.Split(line, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) < 2 {
		return nil, fmt.Errorf("want CSV: date,full|early[,minute][,label]")
	}
	dt, err := time.ParseInLocation("2006-01-02", parts[0], time.Local)
	if err != nil {
		return nil, fmt.Errorf("column 1 date: %w", err)
	}
	kind := strings.ToLower(parts[1])
	switch kind {
	case "full", "full_closure":
		lbl := ""
		if len(parts) > 2 {
			lbl = strings.Join(parts[2:], ",")
		}
		return &exceptionImportRec{at: dt, full: true, label: lbl}, nil
	case "early", "early_closure":
		if len(parts) < 3 {
			return nil, fmt.Errorf("early rows need a minute column")
		}
		em, err := strconv.Atoi(parts[2])
		if err != nil || em < 0 || em > 1439 {
			return nil, fmt.Errorf("early minute 0–1439")
		}
		lbl := ""
		if len(parts) > 3 {
			lbl = strings.Join(parts[3:], ",")
		}
		return &exceptionImportRec{at: dt, full: false, earlyMin: em, label: lbl}, nil
	default:
		return nil, fmt.Errorf("kind column: full or early")
	}
}
