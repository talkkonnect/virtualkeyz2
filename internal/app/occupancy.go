package app

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"sync/atomic"
)

// occupancyGetOrCreateAtomic returns the per-PIN counter for dual-keypad occupancy when DB is nil (secure zone = credential PIN).
func (ctx *AppContext) occupancyGetOrCreateAtomic(pin string) *atomic.Int32 {
	var fresh atomic.Int32
	v, _ := ctx.occupancyCounters.LoadOrStore(pin, &fresh)
	return v.(*atomic.Int32)
}

// sumOccupancyInMemory totals inside counts across all PINs (atomic path, DB nil).
func (ctx *AppContext) sumOccupancyInMemory() (total int) {
	ctx.occupancyCounters.Range(func(_, value any) bool {
		n := int(value.(*atomic.Int32).Load())
		if n > 0 {
			total += n
		}
		return true
	})
	return total
}

// adjustDualKeypadOccupancy updates per-PIN "inside" counts for access_dual_usb_keypad (entry +1, exit −1). Returns total people across all PINs and this PIN's inside count after the change.
// zoneBookkeepingChanged is true when this call modified stored occupancy (used to roll back after a failed hardware actuation).
func (ctx *AppContext) adjustDualKeypadOccupancy(pin, keypadRole string) (areaTotal int, insideThisPIN int, mismatch string, zoneBookkeepingChanged bool) {
	pin = strings.TrimSpace(pin)
	if ctx.DB != nil {
		return ctx.adjustDualKeypadOccupancyDB(pin, keypadRole)
	}
	if pin == "" {
		return 0, 0, "", false
	}
	switch keypadRole {
	case "entry":
		insideThisPIN = int(ctx.occupancyGetOrCreateAtomic(pin).Add(1))
		zoneBookkeepingChanged = true
	case "exit":
		v, ok := ctx.occupancyCounters.Load(pin)
		if !ok {
			mismatch = "exit_without_recorded_entry"
			insideThisPIN = 0
			break
		}
		ctr := v.(*atomic.Int32)
		for {
			cur := ctr.Load()
			if cur <= 0 {
				mismatch = "exit_without_recorded_entry"
				insideThisPIN = 0
				break
			}
			if ctr.CompareAndSwap(cur, cur-1) {
				insideThisPIN = int(cur - 1)
				zoneBookkeepingChanged = true
				break
			}
		}
	default:
		return 0, 0, "", false
	}
	return ctx.sumOccupancyInMemory(), insideThisPIN, mismatch, zoneBookkeepingChanged
}

// adjustDualKeypadOccupancyDB updates dual_keypad_zone_occupancy in a single transaction.
func (ctx *AppContext) adjustDualKeypadOccupancyDB(pin, keypadRole string) (areaTotal int, insideThisPIN int, mismatch string, zoneBookkeepingChanged bool) {
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return 0, 0, "", false
	}
	switch keypadRole {
	case "entry", "exit":
	default:
		return 0, 0, "", false
	}

	tx, err := ctx.DB.BeginTx(context.Background(), nil)
	if err != nil {
		log.Printf("WARNING: dual keypad occupancy tx begin: %v", err)
		return 0, 0, "", false
	}
	defer func() { _ = tx.Rollback() }()

	switch keypadRole {
	case "entry":
		_, err = tx.Exec(`
			INSERT INTO dual_keypad_zone_occupancy (pin, inside_count) VALUES (?, 1)
			ON CONFLICT(pin) DO UPDATE SET inside_count = dual_keypad_zone_occupancy.inside_count + 1
		`, pin)
		if err != nil {
			log.Printf("WARNING: dual keypad occupancy entry: %v", err)
			return 0, 0, "", false
		}
		if err = tx.QueryRow(`SELECT inside_count FROM dual_keypad_zone_occupancy WHERE pin = ?`, pin).Scan(&insideThisPIN); err != nil {
			log.Printf("WARNING: dual keypad occupancy entry read: %v", err)
			return 0, 0, "", false
		}
		zoneBookkeepingChanged = true
	case "exit":
		var cur int
		err = tx.QueryRow(`SELECT inside_count FROM dual_keypad_zone_occupancy WHERE pin = ?`, pin).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			mismatch = "exit_without_recorded_entry"
			insideThisPIN = 0
		} else if err != nil {
			log.Printf("WARNING: dual keypad occupancy exit read: %v", err)
			return 0, 0, "", false
		} else if cur <= 0 {
			mismatch = "exit_without_recorded_entry"
			insideThisPIN = 0
		} else {
			if _, err = tx.Exec(`UPDATE dual_keypad_zone_occupancy SET inside_count = inside_count - 1 WHERE pin = ? AND inside_count > 0`, pin); err != nil {
				log.Printf("WARNING: dual keypad occupancy exit update: %v", err)
				return 0, 0, "", false
			}
			insideThisPIN = cur - 1
			if insideThisPIN == 0 {
				if _, err = tx.Exec(`DELETE FROM dual_keypad_zone_occupancy WHERE pin = ?`, pin); err != nil {
					log.Printf("WARNING: dual keypad occupancy exit delete: %v", err)
					return 0, 0, "", false
				}
			}
			zoneBookkeepingChanged = true
		}
	}

	var sum int64
	if err = tx.QueryRow(`SELECT COALESCE(SUM(inside_count), 0) FROM dual_keypad_zone_occupancy`).Scan(&sum); err != nil {
		log.Printf("WARNING: dual keypad occupancy sum: %v", err)
		return 0, 0, "", false
	}
	areaTotal = int(sum)
	if err = tx.Commit(); err != nil {
		log.Printf("WARNING: dual keypad occupancy commit: %v", err)
		return 0, 0, "", false
	}
	return areaTotal, insideThisPIN, mismatch, zoneBookkeepingChanged
}

// revertDualKeypadZoneAfterFailedActuation undoes adjustDualKeypadOccupancy when a door relay actuation failed after occupancy was updated.
func (ctx *AppContext) revertDualKeypadZoneAfterFailedActuation(pin, keypadRole string) {
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return
	}
	switch strings.TrimSpace(keypadRole) {
	case "entry":
		_, _, _, _ = ctx.adjustDualKeypadOccupancy(pin, "exit")
	case "exit":
		_, _, _, _ = ctx.adjustDualKeypadOccupancy(pin, "entry")
	default:
		return
	}
	log.Printf("INFO: Dual keypad zone occupancy reverted after hardware actuation failure (pin=%s role=%s).", maskPINForTechDisplay(pin), keypadRole)
}

func keypadLogTag(keypadRole string) string {
	if strings.TrimSpace(keypadRole) == "" {
		return "single"
	}
	return keypadRole
}

// dualKeypadExitWouldMismatch is true when exit would not decrement an existing inside count (no prior entry for this PIN).
func (ctx *AppContext) dualKeypadExitWouldMismatch(pin string) bool {
	pin = strings.TrimSpace(pin)
	if ctx.DB != nil {
		var n int
		err := ctx.DB.QueryRow(`SELECT inside_count FROM dual_keypad_zone_occupancy WHERE pin = ?`, pin).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			return true
		}
		if err != nil {
			log.Printf("WARNING: dual keypad occupancy exit check: %v", err)
			return true
		}
		return n <= 0
	}
	v, ok := ctx.occupancyCounters.Load(pin)
	if !ok {
		return true
	}
	return v.(*atomic.Int32).Load() <= 0
}

// maskPINForTechDisplay hides a credential for technician output (last two digits visible).
func maskPINForTechDisplay(pin string) string {
	pin = strings.TrimSpace(pin)
	if len(pin) <= 2 {
		return "****"
	}
	return strings.Repeat("*", len(pin)-2) + pin[len(pin)-2:]
}
