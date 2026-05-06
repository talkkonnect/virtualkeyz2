package store

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"path"
	"sort"

	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3" // SQLite driver
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// DefaultDSN is the on-disk SQLite URL used by the access-control service.
const DefaultDSN = "file:./access_control.db?_fk=1&_busy_timeout=5000&_journal_mode=WAL"

// OpenAccessDB opens SQLite, applies WAL, runs embedded core migrations, and migrates access_pins columns.
func OpenAccessDB(dsn string) (*sqlx.DB, error) {
	db, err := sqlx.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		log.Printf("WARNING: SQLite PRAGMA journal_mode=WAL: %v", err)
	} else {
		log.Println("INFO: SQLite journal_mode=WAL (readers no longer block writers; reduces SQLITE_BUSY vs rollback journal).")
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := migrationFiles.ReadFile(path.Join("migrations", name))
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("exec migration %s: %w", name, err)
		}
	}
	if err := MigrateAccessPinsLifecycle(db); err != nil {
		log.Printf("WARNING: access_pins lifecycle migration: %v", err)
	}
	logCoreTablesReady()
	return db, nil
}

func logCoreTablesReady() {
	log.Println("INFO: SQLite access_pins table ready (PINs optional; device.fallback_access_pin used when set and no row matches).")
	log.Println("INFO: SQLite logs table ready (audit trail for event activities).")
	log.Println("INFO: SQLite dual_keypad_zone_occupancy ready (dual USB keypad zone counts; survives restart).")
	log.Println("INFO: SQLite access_pin_mobile_devices ready (1:N PIN->mobile UUID mappings).")
}
