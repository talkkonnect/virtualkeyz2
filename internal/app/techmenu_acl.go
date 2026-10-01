package app

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"
)

func techMenuACLTopLevel() []string {
	return []string{
		"bind", "door", "door_group", "elevator", "elevator_group", "exception", "group", "help", "level", "pin", "profile", "summary", "target", "window",
	}
}

// techMenuACLSecondLevel returns verbs or nouns after "acl <category> ".
func techMenuACLSecondLevel(category string) []string {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "bind":
		return []string{"door", "elevator"}
	case "door", "elevator", "door_group", "elevator_group":
		return []string{"add", "list"}
	case "pin":
		return []string{"add", "disable", "enable", "hold_extra", "list"}
	case "group":
		return []string{"add", "join", "leave", "list"}
	case "profile":
		return []string{"add", "list", "respects_exceptions"}
	case "exception":
		return []string{"calendar", "date", "import"}
	case "window":
		return []string{"add"}
	case "level":
		return []string{"add", "disable", "enable", "list"}
	case "target":
		return []string{"door", "door_group", "elevator", "elevator_group", "list"}
	default:
		return nil
	}
}

// techMenuACLCompleteAddSpace returns whether Tab should append a space after completing `completed`.
func techMenuACLCompleteAddSpace(prefixLower []string, completed string) bool {
	if len(prefixLower) == 0 || prefixLower[0] != "acl" {
		return false
	}
	if len(prefixLower) == 1 {
		return true
	}
	cat := prefixLower[1]
	if sub := techMenuACLSecondLevel(cat); len(prefixLower) == 2 && len(sub) > 0 {
		return true
	}
	// acl <cat> <verb>
	if len(prefixLower) == 3 {
		switch cat {
		case "door", "elevator", "door_group", "elevator_group":
			if prefixLower[2] == "add" || prefixLower[2] == "list" {
				return true
			}
		case "pin":
			if prefixLower[2] == "add" || prefixLower[2] == "list" ||
				prefixLower[2] == "enable" || prefixLower[2] == "disable" ||
				prefixLower[2] == "hold_extra" {
				return true
			}
		case "group":
			if prefixLower[2] == "add" || prefixLower[2] == "list" ||
				prefixLower[2] == "join" || prefixLower[2] == "leave" {
				return true
			}
		case "profile":
			if prefixLower[2] == "add" || prefixLower[2] == "list" || prefixLower[2] == "respects_exceptions" {
				return true
			}
		case "exception":
			switch prefixLower[2] {
			case "calendar", "date", "import":
				return true
			}
		case "window":
			if prefixLower[2] == "add" {
				return true
			}
		case "level":
			if prefixLower[2] == "add" || prefixLower[2] == "list" ||
				prefixLower[2] == "enable" || prefixLower[2] == "disable" {
				return true
			}
		case "target":
			if prefixLower[2] == "door" || prefixLower[2] == "elevator" ||
				prefixLower[2] == "door_group" || prefixLower[2] == "elevator_group" ||
				prefixLower[2] == "list" {
				return true
			}
		case "bind":
			if prefixLower[2] == "door" || prefixLower[2] == "elevator" {
				return true
			}
		}
	}
	return false
}

func techMenuACLTabMatches(prefix []string, partial string, trailingSpace bool) (matches []string, ok bool) {
	pl := techMenuLowerPrefixSlice(prefix)
	if len(pl) < 1 || pl[0] != "acl" {
		return nil, false
	}
	lowPart := strings.ToLower(partial)
	if trailingSpace {
		lowPart = ""
	}
	switch len(pl) {
	case 1:
		if trailingSpace {
			return append([]string(nil), techMenuACLTopLevel()...), true
		}
		return techMenuFilterPrefixLower(techMenuACLTopLevel(), lowPart), true
	case 2:
		if trailingSpace {
			s := techMenuACLSecondLevel(pl[1])
			if s == nil {
				return nil, true
			}
			return append([]string(nil), s...), true
		}
		s := techMenuACLSecondLevel(pl[1])
		if s == nil {
			return nil, true
		}
		return techMenuFilterPrefixLower(s, lowPart), true
	default:
		// Deeper tokens are user data (ids, numbers); no completion.
		return nil, true
	}
}

func techMenuPrintACLHelp(w io.Writer) {
	fmt.Fprint(w, `
Access control (SQLite access_control.db + device binding)
  Use Tab after "acl " and "acl door " (etc.) to see subcommands.

Binding (which logical door/elevator this controller enforces — saved with cfg save):
  acl bind door <id>              → same as: cfg set access_control_door_id <id>
  acl bind elevator <id>          → same as: cfg set access_control_elevator_id <id>
  Then: cfg save                  persist JSON; door/elevator rows must exist in DB (see below).

Discover:
  acl summary                     current bind ids + row counts
  acl door list | acl elevator list | acl pin list | acl group list
  acl profile list | acl level list | acl target list

Typical setup (door + schedule + PIN + group + level + target):
  1) acl door add east Main_Entrance
  2) acl pin add 123456 Alice
  3) acl group add staff Staff
  4) acl group join staff 123456
  5) acl profile add biz Business_Hours
  6) acl window add biz 1 525 1020        (Mon 08:45–17:00; weekday 0=Sun … 6=Sat, 7=any)
  7) acl level add L1 biz staff L1_label  (time_profile user_group [display_name])
  8) acl target door L1 east
  9) acl bind door east
 10) cfg set access_schedule_enforce true
 11) cfg save

Notes:
  • Use underscores instead of spaces in display names (e.g. Main_Entrance).
  • Times are minutes from midnight (0–1439). Profile timezone: acl profile add id name Asia/Bangkok
  • Exception calendars (holidays): cfg set access_exception_site_timezone America/New_York (IANA; civil dates for holidays)
    acl exception calendar add national National 100
    acl exception date add national 2026-12-25 full Christmas
    acl exception date add national 2026-12-24 early 780 Christmas_Eve_1pm_close
    acl exception import national /path/to/holidays.csv   (CSV: YYYY-MM-DD,full|early[,minute][,label])
    acl profile respects_exceptions biz on   (default on: “standard business” profiles follow holidays; off = ignore exceptions)
  • Enforce schedules only when access_control_*_id matches a row and access_levels target that door/elevator.
  • PINs are stored in access_pins; they are never echoed by these list commands beyond what you typed.
  • Visitor/contractor (temporary) PINs require expires_at (RFC3339) and are rejected after that time; optional max_uses limits successful grants.

Commands (detail):
  acl help                        this text
  acl summary
  acl bind door|elevator <id>
  acl door add <id> [display_name]
  acl door_group add <id> [display_name]
  acl elevator add <id> [display_name]
  acl elevator_group add <id> [display_name]
  acl pin add <pin> [label]       employee PIN; label optional; enabled by default
  acl pin add temporary <pin> <expires_rfc3339> [label...] [--max-uses N]
  acl pin enable|disable <pin>
  acl group add <id> [display_name]
  acl group join|leave <group_id> <pin>
  acl profile add <id> [display_name [iana_timezone]]
  acl profile respects_exceptions <profile_id> on|off
  acl window add <profile_id> <weekday> <start_minute> <end_minute>
  acl level add <level_id> <time_profile_id> <user_group_id> [display_name]
  acl level enable|disable <level_id>
  acl target door|elevator|door_group|elevator_group <level_id> <target_id>
  acl target list
  acl exception calendar add <id> [display_name [priority [source_note]]]  |  acl exception calendar list
  acl exception date add <calendar_id> <YYYY-MM-DD> full [label]  |  … early <minute> [label]
  acl exception date list [calendar_id]  |  acl exception date delete <row_id>
  acl exception import <calendar_id> <csv_path>
`)
}

func techMenuHandleACL(ctx *AppContext, line string, parts []string) {
	if ctx == nil {
		return
	}
	if len(parts) < 2 {
		techMenuSyncPrint(func(w io.Writer) { techMenuPrintACLHelp(w) })
		return
	}
	sub := strings.ToLower(strings.TrimSpace(parts[1]))
	switch sub {
	case "help", "h", "?":
		techMenuSyncPrint(func(w io.Writer) { techMenuPrintACLHelp(w) })
	case "summary":
		techMenuACLSummary(ctx)
	default:
		if err := techMenuACLDispatch(ctx, parts); err != nil {
			log.Printf("WARNING: acl: %v", err)
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "acl: %v\n", err) })
		}
	}
}

func techMenuACLSummary(ctx *AppContext) {
	ctx.configMu.RLock()
	door := strings.TrimSpace(ctx.Config.AccessControlDoorID)
	elev := strings.TrimSpace(ctx.Config.AccessControlElevatorID)
	enforce := ctx.Config.AccessScheduleEnforce
	ctx.configMu.RUnlock()

	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Device binding: access_control_door_id=%q access_control_elevator_id=%q access_schedule_enforce=%v\n",
			door, elev, enforce)
		if ctx.DB == nil {
			fmt.Fprintln(w, "SQLite: (no database)")
			return
		}
		type pair struct {
			label string
			table string
		}
		counts := []pair{
			{"doors", "access_doors"},
			{"door_groups", "access_door_groups"},
			{"elevators", "access_elevators"},
			{"elevator_groups", "access_elevator_groups"},
			{"pins", "access_pins"},
			{"user_groups", "access_user_groups"},
			{"user_group_members", "access_user_group_members"},
			{"time_profiles", "access_time_profiles"},
			{"time_windows", "access_time_windows"},
			{"access_levels", "access_levels"},
			{"level_targets", "access_level_targets"},
			{"exception_calendars", "access_exception_calendars"},
			{"exception_dates", "access_exception_dates"},
			{"audit_logs", "logs"},
		}
		for _, c := range counts {
			var n int
			_ = ctx.DB.QueryRow(`SELECT COUNT(*) FROM ` + c.table).Scan(&n)
			fmt.Fprintf(w, "  %-20s %d\n", c.label+":", n)
		}
	})
	log.Println("INFO: Technician menu: acl summary")
}

func techMenuACLDispatch(ctx *AppContext, parts []string) error {
	if len(parts) < 2 {
		return nil
	}
	if ctx.DB == nil {
		return fmt.Errorf("database not available")
	}
	cat := strings.ToLower(strings.TrimSpace(parts[1]))
	switch cat {
	case "bind":
		return techMenuACLCmdBind(ctx, parts)
	case "door":
		return techMenuACLCmdDoor(ctx, parts)
	case "door_group":
		return techMenuACLCmdDoorGroup(ctx, parts)
	case "elevator":
		return techMenuACLCmdElevator(ctx, parts)
	case "elevator_group":
		return techMenuACLCmdElevatorGroup(ctx, parts)
	case "pin":
		return techMenuACLCmdPin(ctx, parts)
	case "group":
		return techMenuACLCmdGroup(ctx, parts)
	case "profile":
		return techMenuACLCmdProfile(ctx, parts)
	case "window":
		return techMenuACLCmdWindow(ctx, parts)
	case "level":
		return techMenuACLCmdLevel(ctx, parts)
	case "target":
		return techMenuACLCmdTarget(ctx, parts)
	case "exception":
		return techMenuACLCmdException(ctx, parts)
	default:
		return fmt.Errorf("unknown acl %q — try: acl help (Tab completes subcommands)", parts[1])
	}
}

func techMenuACLCmdBind(ctx *AppContext, parts []string) error {
	if len(parts) < 4 {
		return fmt.Errorf(`usage: acl bind door <id>  |  acl bind elevator <id>
same as cfg set access_control_*_id — use cfg save to persist JSON`)
	}
	kind := strings.ToLower(parts[2])
	id := strings.TrimSpace(parts[3])
	if id == "" {
		return fmt.Errorf("id must not be empty")
	}
	var key string
	switch kind {
	case "door":
		key = "access_control_door_id"
	case "elevator":
		key = "access_control_elevator_id"
	default:
		return fmt.Errorf("bind: want door or elevator, got %q", parts[2])
	}
	if err := techMenuCfgSetValue(ctx, key, id); err != nil {
		return err
	}
	log.Printf("INFO: Technician menu: acl bind %s %q (in memory; cfg save to persist)", kind, id)
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Set %s=%q in memory. Run: cfg save\n", key, id)
		if kind == "door" {
			fmt.Fprintln(w, "Hint: create row if missing: acl door add <id> [display_name]")
		} else {
			fmt.Fprintln(w, "Hint: create row if missing: acl elevator add <id> [display_name]")
		}
	})
	return nil
}

func techMenuACLCmdDoor(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl door add <id> [display_name] | acl door list")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_doors", "id", "display_name")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl door add <id> [display_name]")
		}
		id := strings.TrimSpace(parts[3])
		name := ""
		if len(parts) > 4 {
			name = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if id == "" {
			return fmt.Errorf("door id must not be empty")
		}
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_doors (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(name))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl door add %q", id)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Door %q saved. Bind with: acl bind door %s\n", id, id) })
		return nil
	default:
		return fmt.Errorf("door: use add or list (Tab after 'acl door ')")
	}
}

func techMenuACLCmdDoorGroup(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl door_group add <id> [display_name] | acl door_group list")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_door_groups", "id", "display_name")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl door_group add <id> [display_name]")
		}
		id := strings.TrimSpace(parts[3])
		name := ""
		if len(parts) > 4 {
			name = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if id == "" {
			return fmt.Errorf("door_group id must not be empty")
		}
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_door_groups (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(name))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl door_group add %q", id)
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Door group %q saved. acl target door_group <level_id> %s\n", id, id)
		})
		return nil
	default:
		return fmt.Errorf("door_group: use add or list")
	}
}

func techMenuACLCmdElevator(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl elevator add <id> [display_name] | acl elevator list")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_elevators", "id", "display_name")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl elevator add <id> [display_name]")
		}
		id := strings.TrimSpace(parts[3])
		name := ""
		if len(parts) > 4 {
			name = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if id == "" {
			return fmt.Errorf("elevator id must not be empty")
		}
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_elevators (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(name))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl elevator add %q", id)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Elevator %q saved. Bind with: acl bind elevator %s\n", id, id) })
		return nil
	default:
		return fmt.Errorf("elevator: use add or list")
	}
}

func techMenuACLCmdElevatorGroup(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl elevator_group add <id> [display_name] | acl elevator_group list")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_elevator_groups", "id", "display_name")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl elevator_group add <id> [display_name]")
		}
		id := strings.TrimSpace(parts[3])
		name := ""
		if len(parts) > 4 {
			name = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if id == "" {
			return fmt.Errorf("elevator_group id must not be empty")
		}
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_elevator_groups (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(name))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl elevator_group add %q", id)
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Elevator group %q saved. acl target elevator_group <level_id> %s\n", id, id)
		})
		return nil
	default:
		return fmt.Errorf("elevator_group: use add or list")
	}
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func techMenuACLCmdPinAddTemporary(ctx *AppContext, parts []string) error {
	if len(parts) < 6 {
		return fmt.Errorf(`usage: acl pin add temporary <pin> <expires_rfc3339> [label...] [--max-uses N]`)
	}
	pin := strings.TrimSpace(parts[4])
	expiresStr := strings.TrimSpace(parts[5])
	if pin == "" || expiresStr == "" {
		return fmt.Errorf("pin and expires must not be empty")
	}
	if _, err := time.Parse(time.RFC3339, expiresStr); err != nil {
		return fmt.Errorf("expires must be RFC3339 (e.g. 2026-04-16T18:00:00Z): %w", err)
	}
	tail := parts[6:]
	var maxUses sql.NullInt64
	labelParts := make([]string, 0, len(tail))
	for i := 0; i < len(tail); i++ {
		if strings.EqualFold(strings.TrimSpace(tail[i]), "--max-uses") && i+1 < len(tail) {
			n, err := strconv.ParseInt(strings.TrimSpace(tail[i+1]), 10, 64)
			if err != nil || n <= 0 {
				return fmt.Errorf("max-uses must be a positive integer")
			}
			maxUses = sql.NullInt64{Int64: n, Valid: true}
			i++
			continue
		}
		labelParts = append(labelParts, tail[i])
	}
	label := strings.TrimSpace(strings.Join(labelParts, " "))
	var err error
	if maxUses.Valid {
		_, err = ctx.DB.Exec(`INSERT OR REPLACE INTO access_pins (pin, label, enabled, temporary, expires_at, max_uses, use_count, door_hold_extra_seconds) VALUES (?, ?, 1, 1, ?, ?, 0, NULL)`,
			pin, nullIfEmpty(label), expiresStr, maxUses.Int64)
	} else {
		_, err = ctx.DB.Exec(`INSERT OR REPLACE INTO access_pins (pin, label, enabled, temporary, expires_at, max_uses, use_count, door_hold_extra_seconds) VALUES (?, ?, 1, 1, ?, NULL, 0, NULL)`,
			pin, nullIfEmpty(label), expiresStr)
	}
	if err != nil {
		return err
	}
	log.Printf("INFO: Technician menu: acl pin add temporary (enabled)")
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintln(w, "Temporary PIN saved (enabled). Add to a user group: acl group join <group_id> <pin>")
	})
	return nil
}

func techMenuACLCmdPin(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl pin add|list|hold_extra|enable|disable …")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_pins", "pin", "label", "enabled", "temporary", "expires_at", "max_uses", "use_count", "door_hold_extra_seconds")
	case "add":
		if len(parts) >= 5 && strings.EqualFold(strings.TrimSpace(parts[3]), "temporary") {
			return techMenuACLCmdPinAddTemporary(ctx, parts)
		}
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl pin add <pin> [label]")
		}
		pin := strings.TrimSpace(parts[3])
		label := ""
		if len(parts) > 4 {
			label = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if pin == "" {
			return fmt.Errorf("pin must not be empty")
		}
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_pins (pin, label, enabled, temporary, expires_at, max_uses, use_count, door_hold_extra_seconds) VALUES (?, ?, 1, 0, NULL, NULL, 0, NULL)`, pin, nullIfEmpty(label))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl pin add (enabled)")
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "PIN saved (enabled). Add to a user group: acl group join <group_id> <pin>")
		})
		return nil
	case "hold_extra":
		if len(parts) < 5 {
			return fmt.Errorf("usage: acl pin hold_extra <pin> <extra_seconds> (0 clears; extends door_open_warning_after for next door open)")
		}
		pin := strings.TrimSpace(parts[3])
		if pin == "" {
			return fmt.Errorf("pin must not be empty")
		}
		sec, err := strconv.Atoi(strings.TrimSpace(parts[4]))
		if err != nil {
			return fmt.Errorf("extra_seconds: %w", err)
		}
		if sec < 0 {
			return fmt.Errorf("extra_seconds must be >= 0")
		}
		if sec > int((24*time.Hour)/time.Second) {
			return fmt.Errorf("extra_seconds too large (max 24h)")
		}
		res, err := ctx.DB.Exec(`UPDATE access_pins SET door_hold_extra_seconds = ? WHERE pin = ?`, sec, pin)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("no access_pins row for pin %q — use: acl pin add %s", pin, pin)
		}
		log.Printf("INFO: Technician menu: acl pin hold_extra %d s for pin (masked)", sec)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "door_hold_extra_seconds=%d for PIN (update saved)\n", sec) })
		return nil
	case "enable", "disable":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl pin %s <pin>", verb)
		}
		pin := strings.TrimSpace(parts[3])
		if pin == "" {
			return fmt.Errorf("pin must not be empty")
		}
		en := 1
		if verb == "disable" {
			en = 0
		}
		res, err := ctx.DB.Exec(`UPDATE access_pins SET enabled = ? WHERE pin = ?`, en, pin)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("no access_pins row for pin %q — use: acl pin add %s", pin, pin)
		}
		log.Printf("INFO: Technician menu: acl pin %s", verb)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "PIN %q enabled=%d\n", pin, en) })
		return nil
	default:
		return fmt.Errorf("pin: use add, list, hold_extra, enable, or disable")
	}
}

func techMenuACLCmdGroup(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl group add|list|join|leave …")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		rows, err := ctx.DB.Query(`SELECT g.id, g.display_name, COUNT(m.pin) FROM access_user_groups g LEFT JOIN access_user_group_members m ON m.group_id = g.id GROUP BY g.id ORDER BY g.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "user_group id | display_name | member_pins")
			for rows.Next() {
				var id, dn sql.NullString
				var cnt int
				if err := rows.Scan(&id, &dn, &cnt); err != nil {
					fmt.Fprintf(w, "(scan error: %v)\n", err)
					return
				}
				disp := ""
				if dn.Valid {
					disp = dn.String
				}
				fmt.Fprintf(w, "  %s | %s | %d\n", id.String, disp, cnt)
			}
		})
		log.Println("INFO: Technician menu: acl group list")
		return rows.Err()
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl group add <id> [display_name]")
		}
		id := strings.TrimSpace(parts[3])
		name := ""
		if len(parts) > 4 {
			name = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if id == "" {
			return fmt.Errorf("group id must not be empty")
		}
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_user_groups (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(name))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl group add %q", id)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "User group %q saved. acl group join %s <pin>\n", id, id) })
		return nil
	case "join":
		if len(parts) < 5 {
			return fmt.Errorf("usage: acl group join <group_id> <pin>")
		}
		gid := strings.TrimSpace(parts[3])
		pin := strings.TrimSpace(parts[4])
		if gid == "" || pin == "" {
			return fmt.Errorf("group_id and pin must not be empty")
		}
		if err := techMenuACLEnsureFK(ctx, "access_user_groups", "id", gid, "user group"); err != nil {
			return err
		}
		if err := techMenuACLEnsureFK(ctx, "access_pins", "pin", pin, "PIN"); err != nil {
			return err
		}
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_user_group_members (group_id, pin) VALUES (?, ?)`, gid, pin)
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl group join %q %q", gid, pin)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "PIN %q added to group %q\n", pin, gid) })
		return nil
	case "leave":
		if len(parts) < 5 {
			return fmt.Errorf("usage: acl group leave <group_id> <pin>")
		}
		gid := strings.TrimSpace(parts[3])
		pin := strings.TrimSpace(parts[4])
		_, err := ctx.DB.Exec(`DELETE FROM access_user_group_members WHERE group_id = ? AND pin = ?`, gid, pin)
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl group leave %q %q", gid, pin)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Removed PIN %q from group %q (if it was present)\n", pin, gid) })
		return nil
	default:
		return fmt.Errorf("group: use add, list, join, or leave")
	}
}

func techMenuACLCreateHint(table string) string {
	switch table {
	case "access_doors":
		return "acl door add <id>"
	case "access_door_groups":
		return "acl door_group add <id>"
	case "access_elevators":
		return "acl elevator add <id>"
	case "access_elevator_groups":
		return "acl elevator_group add <id>"
	case "access_pins":
		return "acl pin add <pin>"
	case "access_user_groups":
		return "acl group add <id>"
	case "access_time_profiles":
		return "acl profile add <id>"
	case "access_levels":
		return "acl level add <level_id> <time_profile_id> <user_group_id>"
	default:
		return "acl help"
	}
}

func techMenuACLEnsureFK(ctx *AppContext, table, col, id, what string) error {
	var dummy string
	err := ctx.DB.QueryRow(`SELECT `+col+` FROM `+table+` WHERE `+col+` = ? LIMIT 1`, id).Scan(&dummy)
	if err == sql.ErrNoRows {
		return fmt.Errorf("unknown %s %q — create it first (%s)", what, id, techMenuACLCreateHint(table))
	}
	return err
}

func techMenuACLCmdProfile(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl profile add|list|respects_exceptions …")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_time_profiles", "id", "display_name", "iana_timezone", "respects_exception_calendar")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl profile add <id> [display_name [iana_timezone]]")
		}
		id := strings.TrimSpace(parts[3])
		if id == "" {
			return fmt.Errorf("profile id must not be empty")
		}
		display := ""
		tz := ""
		switch len(parts) {
		case 4:
			break
		case 5:
			display = strings.TrimSpace(parts[4])
		default:
			display = strings.TrimSpace(parts[4])
			tz = strings.TrimSpace(strings.Join(parts[5:], " "))
		}
		_, err := ctx.DB.Exec(`
			INSERT INTO access_time_profiles (id, display_name, description, iana_timezone, respects_exception_calendar)
			VALUES (?, ?, ?, ?, 1)
			ON CONFLICT(id) DO UPDATE SET
				display_name = excluded.display_name,
				description = excluded.description,
				iana_timezone = excluded.iana_timezone`,
			id, nullIfEmpty(display), nil, tz)
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl profile add %q", id)
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Time profile %q saved. Add windows: acl window add %s <weekday> <start_min> <end_min>\n", id, id)
			fmt.Fprintln(w, "  weekday: 0=Sun … 6=Sat, 7=any day; minutes 0–1439 (start>end crosses midnight)")
		})
		return nil
	case "respects_exceptions":
		if len(parts) < 5 {
			return fmt.Errorf("usage: acl profile respects_exceptions <profile_id> on|off")
		}
		pid := strings.TrimSpace(parts[3])
		if pid == "" {
			return fmt.Errorf("profile_id must not be empty")
		}
		sw := strings.ToLower(strings.TrimSpace(parts[4]))
		var v int
		switch sw {
		case "on", "1", "true", "yes":
			v = 1
		case "off", "0", "false", "no":
			v = 0
		default:
			return fmt.Errorf("respects_exceptions: want on or off")
		}
		if err := techMenuACLEnsureFK(ctx, "access_time_profiles", "id", pid, "time profile"); err != nil {
			return err
		}
		_, err := ctx.DB.Exec(`UPDATE access_time_profiles SET respects_exception_calendar = ? WHERE id = ?`, v, pid)
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl profile respects_exceptions %q = %d", pid, v)
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Profile %q: respects_exception_calendar=%d (1=apply holiday/exception calendars)\n", pid, v)
		})
		return nil
	default:
		return fmt.Errorf("profile: use add, list, or respects_exceptions")
	}
}

func techMenuACLCmdWindow(ctx *AppContext, parts []string) error {
	if len(parts) < 7 {
		return fmt.Errorf(`usage: acl window add <profile_id> <weekday> <start_minute> <end_minute>
example: acl window add biz 1 525 1020   (Mon 08:45–17:00)
hint: acl profile list — use existing profile_id`)
	}
	if strings.ToLower(parts[2]) != "add" {
		return fmt.Errorf("window: only add is supported")
	}
	pid := strings.TrimSpace(parts[3])
	wd, err := strconv.Atoi(parts[4])
	if err != nil {
		return fmt.Errorf("weekday: integer 0–7: %w", err)
	}
	sm, err := strconv.Atoi(parts[5])
	if err != nil {
		return fmt.Errorf("start_minute: %w", err)
	}
	em, err := strconv.Atoi(parts[6])
	if err != nil {
		return fmt.Errorf("end_minute: %w", err)
	}
	if wd < 0 || wd > 7 || sm < 0 || sm > 1439 || em < 0 || em > 1439 {
		return fmt.Errorf("weekday must be 0–7, minutes 0–1439")
	}
	if err := techMenuACLEnsureFK(ctx, "access_time_profiles", "id", pid, "time profile"); err != nil {
		return err
	}
	_, err = ctx.DB.Exec(`INSERT INTO access_time_windows (time_profile_id, weekday, start_minute, end_minute) VALUES (?, ?, ?, ?)`, pid, wd, sm, em)
	if err != nil {
		return err
	}
	log.Printf("INFO: Technician menu: acl window add profile=%s weekday=%d %d-%d", pid, wd, sm, em)
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Time window added for profile %q. Next: acl level add <level_id> %s <user_group_id> [name]\n", pid, pid)
	})
	return nil
}

func techMenuACLCmdLevel(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl level add|list|enable|disable …")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		rows, err := ctx.DB.Query(`SELECT id, display_name, time_profile_id, user_group_id, enabled FROM access_levels ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "id | display_name | time_profile_id | user_group_id | enabled")
			for rows.Next() {
				var id, dn, tp, ug string
				var en int
				if err := rows.Scan(&id, &dn, &tp, &ug, &en); err != nil {
					fmt.Fprintf(w, "(scan error: %v)\n", err)
					return
				}
				fmt.Fprintf(w, "  %s | %s | %s | %s | %d\n", id, dn, tp, ug, en)
			}
		})
		log.Println("INFO: Technician menu: acl level list")
		return rows.Err()
	case "add":
		if len(parts) < 6 {
			return fmt.Errorf(`usage: acl level add <level_id> <time_profile_id> <user_group_id> [display_name]
hint: acl profile list | acl group list`)
		}
		lid := strings.TrimSpace(parts[3])
		tpid := strings.TrimSpace(parts[4])
		ugid := strings.TrimSpace(parts[5])
		dname := ""
		if len(parts) > 6 {
			dname = strings.TrimSpace(strings.Join(parts[6:], " "))
		}
		if lid == "" || tpid == "" || ugid == "" {
			return fmt.Errorf("level_id, time_profile_id, and user_group_id must not be empty")
		}
		if err := techMenuACLEnsureFK(ctx, "access_time_profiles", "id", tpid, "time profile"); err != nil {
			return err
		}
		if err := techMenuACLEnsureFK(ctx, "access_user_groups", "id", ugid, "user group"); err != nil {
			return err
		}
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_levels (id, display_name, time_profile_id, user_group_id, enabled) VALUES (?, ?, ?, ?, 1)`,
			lid, nullIfEmpty(dname), tpid, ugid)
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl level add %q", lid)
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Access level %q saved (enabled). Grant door/elevator: acl target door %s <door_id>\n", lid, lid)
		})
		return nil
	case "enable", "disable":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl level %s <level_id>", verb)
		}
		lid := strings.TrimSpace(parts[3])
		en := 1
		if verb == "disable" {
			en = 0
		}
		res, err := ctx.DB.Exec(`UPDATE access_levels SET enabled = ? WHERE id = ?`, en, lid)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("no access_levels row for id %q — acl level list", lid)
		}
		log.Printf("INFO: Technician menu: acl level %s %q", verb, lid)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Level %q enabled=%d\n", lid, en) })
		return nil
	default:
		return fmt.Errorf("level: use add, list, enable, or disable")
	}
}

func techMenuACLCmdTarget(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl target door|elevator|door_group|elevator_group <level_id> <id> | acl target list")
	}
	verb := strings.ToLower(parts[2])
	if verb == "list" {
		rows, err := ctx.DB.Query(`
			SELECT t.id, t.access_level_id, t.door_id, t.door_group_id, t.elevator_id, t.elevator_group_id
			FROM access_level_targets t ORDER BY t.access_level_id, t.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "row_id | level_id | door_id | door_group_id | elevator_id | elevator_group_id")
			for rows.Next() {
				var rid int
				var lid string
				var did, dgid, eid, egid sql.NullString
				if err := rows.Scan(&rid, &lid, &did, &dgid, &eid, &egid); err != nil {
					fmt.Fprintf(w, "(scan error: %v)\n", err)
					return
				}
				fmt.Fprintf(w, "  %d | %s | %v | %v | %v | %v\n", rid, lid, ns(did), ns(dgid), ns(eid), ns(egid))
			}
		})
		log.Println("INFO: Technician menu: acl target list")
		return rows.Err()
	}
	if len(parts) < 5 {
		return fmt.Errorf("usage: acl target %s <level_id> <target_id>", verb)
	}
	lid := strings.TrimSpace(parts[3])
	tid := strings.TrimSpace(parts[4])
	if lid == "" || tid == "" {
		return fmt.Errorf("level_id and target id must not be empty")
	}
	if err := techMenuACLEnsureFK(ctx, "access_levels", "id", lid, "access level"); err != nil {
		return err
	}
	var err error
	switch verb {
	case "door":
		if err := techMenuACLEnsureFK(ctx, "access_doors", "id", tid, "door"); err != nil {
			return err
		}
		_, err = ctx.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, ?, NULL, NULL, NULL)`, lid, tid)
	case "door_group":
		if err := techMenuACLEnsureFK(ctx, "access_door_groups", "id", tid, "door group"); err != nil {
			return err
		}
		_, err = ctx.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, NULL, ?, NULL, NULL)`, lid, tid)
	case "elevator":
		if err := techMenuACLEnsureFK(ctx, "access_elevators", "id", tid, "elevator"); err != nil {
			return err
		}
		_, err = ctx.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, NULL, NULL, ?, NULL)`, lid, tid)
	case "elevator_group":
		if err := techMenuACLEnsureFK(ctx, "access_elevator_groups", "id", tid, "elevator group"); err != nil {
			return err
		}
		_, err = ctx.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, NULL, NULL, NULL, ?)`, lid, tid)
	default:
		return fmt.Errorf("target: use door, elevator, door_group, elevator_group, or list")
	}
	if err != nil {
		return err
	}
	log.Printf("INFO: Technician menu: acl target %s level=%q target=%q", verb, lid, tid)
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Target row added. Bind device: acl bind door|elevator <id> (must match this target)\n")
	})
	return nil
}

func ns(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}

func techMenuACLQueryStrings(ctx *AppContext, table string, cols ...string) error {
	if len(cols) == 0 {
		return fmt.Errorf("internal: no columns")
	}
	sb := strings.Builder{}
	for i, c := range cols {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(c)
	}
	q := `SELECT ` + sb.String() + ` FROM ` + table + ` ORDER BY 1`
	rows, err := ctx.DB.Query(q)
	if err != nil {
		return err
	}
	defer rows.Close()
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintln(w, strings.Join(cols, " | "))
		for rows.Next() {
			scans := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cols {
				ptrs[i] = &scans[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				fmt.Fprintf(w, "(scan error: %v)\n", err)
				return
			}
			for i, v := range scans {
				if i > 0 {
					fmt.Fprint(w, " | ")
				}
				fmt.Fprint(w, techMenuACLFormatCell(v))
			}
			fmt.Fprintln(w)
		}
	})
	log.Printf("INFO: Technician menu: acl list %s", table)
	return rows.Err()
}

func techMenuACLFormatCell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	default:
		return fmt.Sprint(x)
	}
}
