package store

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/jmoiron/sqlx"
)

// MigrateAccessPinsLifecycle adds visitor/contractor lifecycle columns to access_pins on older databases.
func MigrateAccessPinsLifecycle(db *sqlx.DB) error {
	if db == nil {
		return nil
	}
	rows, err := db.Query(`PRAGMA table_info(access_pins)`)
	if err != nil {
		return err
	}
	have := make(map[string]struct{})
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = struct{}{}
	}
	rows.Close()
	alters := []struct {
		name string
		ddl  string
	}{
		{"temporary", `ALTER TABLE access_pins ADD COLUMN temporary INTEGER NOT NULL DEFAULT 0`},
		{"expires_at", `ALTER TABLE access_pins ADD COLUMN expires_at TEXT`},
		{"max_uses", `ALTER TABLE access_pins ADD COLUMN max_uses INTEGER`},
		{"use_count", `ALTER TABLE access_pins ADD COLUMN use_count INTEGER NOT NULL DEFAULT 0`},
		{"door_hold_extra_seconds", `ALTER TABLE access_pins ADD COLUMN door_hold_extra_seconds INTEGER`},
	}
	for _, a := range alters {
		if _, ok := have[a.name]; ok {
			continue
		}
		if _, err := db.Exec(a.ddl); err != nil {
			return fmt.Errorf("%s: %w", a.name, err)
		}
		log.Printf("INFO: access_pins migrated: added column %s", a.name)
	}
	return nil
}
