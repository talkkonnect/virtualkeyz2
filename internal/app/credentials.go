package app

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"strings"
	"time"
	"virtualkeyz2/internal/store"

	"github.com/jmoiron/sqlx"
)

func initDatabase() *sqlx.DB {
	db, err := store.OpenAccessDB(store.DefaultDSN)
	if err != nil {
		releaseStartupLogBuffer(os.Stdout)
		log.Fatalf("CRITICAL: Failed to open database: %v", err)
	}
	if err := initAccessScheduleSchema(db); err != nil {
		log.Printf("WARNING: access schedule schema: %v", err)
	}
	return db
}

func normalizeDeviceUUID(deviceUUID string) string {
	return strings.ToLower(strings.TrimSpace(deviceUUID))
}

// upsertMobileDeviceForPIN inserts or reassigns a mobile UUID to a PIN while enforcing MaxDevicesPerUser.
//
//lint:ignore U1000 kept for mobile-device (phone QR) registration, not wired up yet
func (ctx *AppContext) upsertMobileDeviceForPIN(pin, deviceUUID string) error {
	if ctx.DB == nil {
		return errors.New("database unavailable")
	}
	pin = strings.TrimSpace(pin)
	deviceUUID = normalizeDeviceUUID(deviceUUID)
	if pin == "" {
		return errors.New("pin is required")
	}
	if deviceUUID == "" {
		return errors.New("device_uuid is required")
	}
	ctx.configMu.RLock()
	maxDevices := ctx.Config.MaxDevicesPerUser
	ctx.configMu.RUnlock()
	if maxDevices <= 0 {
		maxDevices = 1
	}
	tx, err := ctx.DB.Beginx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var existingPIN string
	err = tx.QueryRow(`SELECT pin FROM access_pin_mobile_devices WHERE device_uuid = ?`, deviceUUID).Scan(&existingPIN)
	switch {
	case err == nil:
		if strings.TrimSpace(existingPIN) == pin {
			_, err = tx.Exec(`UPDATE access_pin_mobile_devices SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE device_uuid = ?`, deviceUUID)
		} else {
			var n int
			if err = tx.QueryRow(`SELECT COUNT(*) FROM access_pin_mobile_devices WHERE pin = ?`, pin).Scan(&n); err == nil && n >= maxDevices {
				err = fmt.Errorf("pin %q already has max_devices_per_user=%d", maskPINForTechDisplay(pin), maxDevices)
			}
			if err == nil {
				_, err = tx.Exec(`UPDATE access_pin_mobile_devices SET pin = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE device_uuid = ?`, pin, deviceUUID)
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		var n int
		if err = tx.QueryRow(`SELECT COUNT(*) FROM access_pin_mobile_devices WHERE pin = ?`, pin).Scan(&n); err == nil && n >= maxDevices {
			err = fmt.Errorf("pin %q already has max_devices_per_user=%d", maskPINForTechDisplay(pin), maxDevices)
		}
		if err == nil {
			_, err = tx.Exec(`INSERT INTO access_pin_mobile_devices(pin, device_uuid) VALUES (?, ?)`, pin, deviceUUID)
		}
	default:
		return err
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// listMobileDeviceUUIDsByPIN fetches all UUIDs currently linked to a PIN.
//
//lint:ignore U1000 kept for mobile-device (phone QR) registration, not wired up yet
func (ctx *AppContext) listMobileDeviceUUIDsByPIN(pin string) ([]string, error) {
	if ctx.DB == nil {
		return nil, errors.New("database unavailable")
	}
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return nil, errors.New("pin is required")
	}
	rows, err := ctx.DB.Query(`SELECT device_uuid FROM access_pin_mobile_devices WHERE pin = ? ORDER BY device_uuid`, pin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 8)
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimSpace(uuid))
	}
	return out, rows.Err()
}

// resolvePINForMobileUUID returns a currently enabled PIN associated with device_uuid.
func (ctx *AppContext) resolvePINForMobileUUID(deviceUUID string) (pin string, err error) {
	if ctx.DB == nil {
		return "", errors.New("database unavailable")
	}
	deviceUUID = normalizeDeviceUUID(deviceUUID)
	if deviceUUID == "" {
		return "", errors.New("device_uuid is required")
	}
	err = ctx.DB.QueryRow(`
		SELECT ap.pin
		FROM access_pin_mobile_devices md
		INNER JOIN access_pins ap ON ap.pin = md.pin
		WHERE md.device_uuid = ? AND ap.enabled = 1
		LIMIT 1`, deviceUUID).Scan(&pin)
	if err != nil {
		return "", err
	}
	pin = strings.TrimSpace(pin)
	cred := ctx.accessCredentialForPIN(pin)
	if !cred.OK {
		if cred.LifecycleReason == "" {
			return "", sql.ErrNoRows
		}
		return "", fmt.Errorf("credential_lifecycle:%s", cred.LifecycleReason)
	}
	return pin, nil
}

// accessPinLookupResult is the outcome of validating a PIN against access_pins and optional fallback.
type accessPinLookupResult struct {
	OK              bool
	Label           string
	ViaFallback     bool
	LifecycleReason string // when OK is false because a DB row failed visitor/contractor lifecycle rules
	// DoorHoldExtra extends door_open_warning_after for the next door-open period after this credential unlocks the door.
	DoorHoldExtra time.Duration
}

// accessCredentialForPIN returns whether the PIN is allowed: row in access_pins with enabled=1, or FallbackAccessPin when set and no DB match.
// Visitor/contractor rows (temporary=1) require expires_at (RFC3339) and are rejected after that time or after max_uses successful grants, whichever comes first.
func (ctx *AppContext) accessCredentialForPIN(pin string) accessPinLookupResult {
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return accessPinLookupResult{}
	}
	if ctx.DB != nil {
		var lbl sql.NullString
		var temporary int
		var expiresAt sql.NullString
		var maxUses sql.NullInt64
		var useCount int64
		var holdExtraSec sql.NullInt64
		err := ctx.DB.QueryRow(`
			SELECT label, COALESCE(temporary, 0), expires_at, max_uses, COALESCE(use_count, 0), COALESCE(door_hold_extra_seconds, 0)
			FROM access_pins WHERE pin = ? AND enabled = 1`, pin).Scan(&lbl, &temporary, &expiresAt, &maxUses, &useCount, &holdExtraSec)
		if err == nil {
			lblStr := strings.TrimSpace(lbl.String)
			if temporary != 0 {
				if !expiresAt.Valid || strings.TrimSpace(expiresAt.String) == "" {
					return accessPinLookupResult{Label: lblStr, LifecycleReason: "temporary_requires_expires_at"}
				}
				expT, perr := time.Parse(time.RFC3339, strings.TrimSpace(expiresAt.String))
				if perr != nil {
					return accessPinLookupResult{Label: lblStr, LifecycleReason: "invalid_expires_at"}
				}
				if !time.Now().Before(expT) {
					_, _ = ctx.DB.Exec(`UPDATE access_pins SET enabled = 0 WHERE pin = ? AND COALESCE(temporary, 0) != 0`, pin)
					return accessPinLookupResult{Label: lblStr, LifecycleReason: "credential_expired"}
				}
				if maxUses.Valid && maxUses.Int64 > 0 && useCount >= maxUses.Int64 {
					_, _ = ctx.DB.Exec(`UPDATE access_pins SET enabled = 0 WHERE pin = ? AND COALESCE(temporary, 0) != 0`, pin)
					return accessPinLookupResult{Label: lblStr, LifecycleReason: "use_limit_exhausted"}
				}
			}
			extra := time.Duration(0)
			if holdExtraSec.Valid && holdExtraSec.Int64 > 0 {
				extra = time.Duration(holdExtraSec.Int64) * time.Second
				if extra > 24*time.Hour {
					extra = 24 * time.Hour
				}
			}
			return accessPinLookupResult{OK: true, Label: lblStr, DoorHoldExtra: extra}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("WARNING: access_pins lookup: %v", err)
		}
	}
	ctx.configMu.RLock()
	fallback := strings.TrimSpace(ctx.Config.FallbackAccessPin)
	ctx.configMu.RUnlock()
	if fallback != "" && pin == fallback {
		return accessPinLookupResult{OK: true, ViaFallback: true}
	}
	return accessPinLookupResult{}
}

// credentialRecordSuccessfulUse increments use_count for temporary credentials after a successful access grant.
// Dual-USB exit unlocks do not consume a use (free egress). Empty pin is a no-op (e.g. physical exit button).
func (ctx *AppContext) credentialRecordSuccessfulUse(pin, mode, keypadRole string) {
	pin = strings.TrimSpace(pin)
	if pin == "" || ctx.DB == nil {
		return
	}
	if NormalizeKeypadOperationMode(mode) == ModeAccessDualUSBKeypad && strings.TrimSpace(keypadRole) == "exit" {
		return
	}
	_, err := ctx.DB.Exec(`
		UPDATE access_pins SET
			use_count = use_count + 1,
			enabled = CASE
				WHEN COALESCE(temporary, 0) != 0 AND max_uses IS NOT NULL AND max_uses > 0
					AND (use_count + 1) >= max_uses THEN 0
				ELSE enabled
			END
		WHERE pin = ? AND COALESCE(temporary, 0) != 0`, pin)
	if err != nil {
		log.Printf("WARNING: access_pins use_count update: %v", err)
	}
}

// pinRejectCredentialLifecycle plays reject feedback for lifecycle denial without counting toward wrong-PIN lockout streak.
func (ctx *AppContext) pinRejectCredentialLifecycle(cfg DeviceConfig, keypadRole, reason string, extra map[string]any) {
	lcdShowInvalidCard(ctx)
	wh := map[string]any{"reason": reason, "keypad_role": keypadRole}
	maps.Copy(wh, extra)
	// Fire the webhook BEFORE the (blocking) reject sound so remote displays
	// (fb-virtualkeyz2) show ACCESS DENIED in sync with the LCD, not after the
	// sound has finished playing.
	fireEventWebhook(ctx, "pin_rejected", wh)
	ctx.playRejectSound(keypadRole, cfg)
	// No feedback-delay input hold: re-entry after a rejection is allowed immediately. The
	// displays hold ACCESS DENIED on their own (LCD auto-idle + fb result hold).
}
