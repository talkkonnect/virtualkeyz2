package main

import (
	"testing"
	"time"
)

func TestAccessCredentialTemporaryExpires(t *testing.T) {
	db := testAccessDB(t)
	defer db.Close()

	ctx := &AppContext{DB: db}
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO access_pins (pin, label, enabled, temporary, expires_at, max_uses, use_count) VALUES ('111111', 'v', 1, 1, ?, NULL, 0)`, past); err != nil {
		t.Fatal(err)
	}
	r := ctx.accessCredentialForPIN("111111")
	if r.OK || r.LifecycleReason != "credential_expired" {
		t.Fatalf("expected expired temporary, got %#v", r)
	}
}

func TestAccessCredentialTemporaryUseLimit(t *testing.T) {
	db := testAccessDB(t)
	defer db.Close()

	ctx := &AppContext{DB: db}
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO access_pins (pin, label, enabled, temporary, expires_at, max_uses, use_count) VALUES ('222222', 'v', 1, 1, ?, 2, 2)`, future); err != nil {
		t.Fatal(err)
	}
	r := ctx.accessCredentialForPIN("222222")
	if r.OK || r.LifecycleReason != "use_limit_exhausted" {
		t.Fatalf("expected use limit, got %#v", r)
	}
}

func TestAccessCredentialEmployeeIgnoresExpiry(t *testing.T) {
	db := testAccessDB(t)
	defer db.Close()

	ctx := &AppContext{DB: db}
	past := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO access_pins (pin, label, enabled, temporary, expires_at, max_uses, use_count) VALUES ('333333', 'e', 1, 0, ?, NULL, 0)`, past); err != nil {
		t.Fatal(err)
	}
	r := ctx.accessCredentialForPIN("333333")
	if !r.OK || r.ViaFallback {
		t.Fatalf("expected employee ok, got %#v", r)
	}
}

func TestCredentialRecordSuccessfulUseDisablesAtMax(t *testing.T) {
	db := testAccessDB(t)
	defer db.Close()

	ctx := &AppContext{DB: db}
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO access_pins (pin, label, enabled, temporary, expires_at, max_uses, use_count) VALUES ('444444', 'v', 1, 1, ?, 2, 1)`, future); err != nil {
		t.Fatal(err)
	}
	ctx.credentialRecordSuccessfulUse("444444", "access", "")
	var en int
	if err := db.QueryRow(`SELECT enabled FROM access_pins WHERE pin = '444444'`).Scan(&en); err != nil {
		t.Fatal(err)
	}
	if en != 0 {
		t.Fatalf("expected disabled after last use, enabled=%d", en)
	}
}
