package store

import (
	"testing"

	"github.com/jmoiron/sqlx"
)

func TestOpenAccessDB_inMemory(t *testing.T) {
	dsn := "file:vkz_test_mem?mode=memory&cache=shared&_fk=1&_busy_timeout=5000"
	db, err := OpenAccessDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='access_pins'`); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("access_pins table missing, count=%d", n)
	}
}

func TestMigrateAccessPinsLifecycle_idempotent(t *testing.T) {
	db, err := sqlx.Open("sqlite3", "file:vkz_mig?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE access_pins (pin TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := MigrateAccessPinsLifecycle(db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateAccessPinsLifecycle(db); err != nil {
		t.Fatal(err)
	}
}
