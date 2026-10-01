package app

import (
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Keypad function keys (see OPERATOR.md "Keypad function keys"):
//   - Cancel clears the entry.
//   - Enter on an empty buffer rings the doorbell (keypad_doorbell_enabled).
//   - "<code> Space <PIN>" submits the PIN with a function code: the duress code
//     (keypad_fn_duress_code) or, with keypad_function_codes_enabled, an extended door hold,
//     a door latch toggle, or a direct elevator floor index.

const (
	keypadFnExtendedHold = "1" // door modes: longer relay pulse + extra door-open grace
	keypadFnLatch        = "2" // door modes: toggle the door relay held on (keypad_fn_latch_enabled)

	keypadFnCodeMaxLen = 2
)

// pinOpts carries the function code parsed from a keypad entry into processPINOpts.
type pinOpts struct {
	fnCode string // "" = none; never the duress code
	duress bool
}

// parseKeypadEntry splits a keypad buffer "<code> <PIN>" into its function code and PIN.
// A buffer without a space is a plain PIN (code "").
func parseKeypadEntry(buf string) (code, pin string) {
	code, pin, ok := strings.Cut(buf, " ")
	if !ok {
		return "", buf
	}
	return code, pin
}

// keypadEntryPINPart returns the PIN digits of a keypad buffer (the part after the code separator).
func keypadEntryPINPart(buf string) string {
	_, pin := parseKeypadEntry(buf)
	return pin
}

// keypadSpaceAllowed reports whether Space may be appended to buf as the code separator:
// only once, after a 1-2 digit code.
func keypadSpaceAllowed(buf string) bool {
	return buf != "" && len(buf) <= keypadFnCodeMaxLen && isAllDigits(buf)
}

// resolveKeypadEntry parses buf and maps the duress code onto pinOpts.duress.
func resolveKeypadEntry(cfg DeviceConfig, buf string) (string, pinOpts) {
	code, pin := parseKeypadEntry(strings.TrimSpace(buf))
	if code == "" {
		return pin, pinOpts{}
	}
	if d := strings.TrimSpace(cfg.KeypadFnDuressCode); d != "" && code == d {
		return pin, pinOpts{duress: true}
	}
	return pin, pinOpts{fnCode: code}
}

// keypadFnFloorIndex returns the elevator floor index for a function code, or ok=false.
func keypadFnFloorIndex(code string) (int, bool) {
	if code == "" || !isAllDigits(code) {
		return 0, false
	}
	n, err := strconv.Atoi(code)
	return n, err == nil
}

// keypadFnCodeValid reports whether opts.fnCode is usable in mode with cfg.
func (ctx *AppContext) keypadFnCodeValid(cfg DeviceConfig, mode string, opts pinOpts) bool {
	if opts.fnCode == "" {
		return true
	}
	if !cfg.KeypadFunctionCodesEnabled {
		return false
	}
	switch mode {
	case ModeElevatorWaitFloorButtons:
		idx, ok := keypadFnFloorIndex(opts.fnCode)
		return ok && idx < len(ctx.elevatorFloorDispatchPins)
	case ModeElevatorPredefinedFloor:
		return false
	}
	switch opts.fnCode {
	case keypadFnExtendedHold:
		return true
	case keypadFnLatch:
		return cfg.KeypadFnLatchEnabled
	}
	return false
}

// raiseDuressAlarm fires the silent duress event. Nothing is shown or played locally.
func (ctx *AppContext) raiseDuressAlarm(keypadRole, credLabel string) {
	log.Printf("INFO: Duress code entered (%s keypad; credential=%s).", keypadLogTag(keypadRole), credLabel)
	detail := map[string]any{"keypad_role": keypadRole, "credential_label": credLabel}
	fireEventWebhook(ctx, "duress_alarm", detail)
	go mqttPublishEvent(ctx, "duress_alarm", detail)
}

// ringDoorbell handles Enter on an empty keypad buffer. Rings are rate-limited by
// keypad_doorbell_cooldown; the keypad loop is never blocked.
func (ctx *AppContext) ringDoorbell(keypadRole string) {
	ctx.configMu.RLock()
	cfg := ctx.Config
	ctx.configMu.RUnlock()
	if !cfg.KeypadDoorbellEnabled {
		return
	}
	now := time.Now()
	last := ctx.lastDoorbell.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < cfg.KeypadDoorbellCooldown {
		debugf("Doorbell: ignored (within keypad_doorbell_cooldown %s).", cfg.KeypadDoorbellCooldown)
		return
	}
	if !ctx.lastDoorbell.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	log.Printf("INFO: Doorbell rung (%s keypad).", keypadLogTag(keypadRole))
	detail := map[string]any{"keypad_role": keypadRole}
	fireEventWebhook(ctx, "doorbell", detail)
	go mqttPublishEvent(ctx, "doorbell", detail)
	lcdEnqueueFull(ctx, lcdLinesDoorbell(), lcdAutoIdleAfter)
	ctx.playFeedbackSound(keypadRole, cfg, cfg.SoundDoorbell, cfg.SoundDoorbellEnabled, cfg.SoundDoorbellBlocking)
}

// doorLatchState tracks the keypad door latch (function code 2): door relay held on until
// the next latch toggle, keypad_fn_latch_max, or fireman's service.
type doorLatchState struct {
	mu    sync.Mutex
	on    bool
	gen   uint64
	timer *time.Timer
}

// DoorLatched reports whether the door relay is latched on by the keypad.
func (ctx *AppContext) DoorLatched() bool {
	ctx.doorLatch.mu.Lock()
	defer ctx.doorLatch.mu.Unlock()
	return ctx.doorLatch.on
}

// latchDoor energizes the door relay and arms the auto-release timer.
func (ctx *AppContext) latchDoor(maxHold time.Duration) error {
	if ctx.GPIO != nil && ctx.GPIO.HasOutput("door") {
		if err := ctx.GPIO.actionOnErr("door"); err != nil {
			return err
		}
	} else {
		log.Println("WARNING: GPIO unavailable; door latch is software state only.")
	}
	ctx.doorLatch.mu.Lock()
	defer ctx.doorLatch.mu.Unlock()
	ctx.doorLatch.on = true
	ctx.doorLatch.gen++
	gen := ctx.doorLatch.gen
	if ctx.doorLatch.timer != nil {
		ctx.doorLatch.timer.Stop()
	}
	ctx.doorLatch.timer = nil
	if maxHold > 0 {
		ctx.doorLatch.timer = time.AfterFunc(maxHold, func() {
			ctx.doorLatch.mu.Lock()
			current := ctx.doorLatch.gen == gen
			ctx.doorLatch.mu.Unlock()
			if current {
				ctx.releaseDoorLatch("keypad_fn_latch_max", "")
			}
		})
	}
	return nil
}

// releaseDoorLatch drops the latch (if set), de-energizes the door relay unless the fire-alarm
// interface is holding it, and fires door_unlatched. It reports whether a latch was released.
func (ctx *AppContext) releaseDoorLatch(reason, keypadRole string) bool {
	ctx.doorLatch.mu.Lock()
	if !ctx.doorLatch.on {
		ctx.doorLatch.mu.Unlock()
		return false
	}
	ctx.doorLatch.on = false
	ctx.doorLatch.gen++
	if ctx.doorLatch.timer != nil {
		ctx.doorLatch.timer.Stop()
		ctx.doorLatch.timer = nil
	}
	ctx.doorLatch.mu.Unlock()
	if ctx.GPIO != nil && ctx.GPIO.HasOutput("door") && !ctx.FireAlarmInterfaceActive() {
		ctx.GPIO.ActionOff("door")
	}
	log.Printf("INFO: Door latch released (reason=%s).", reason)
	fireEventWebhook(ctx, "door_unlatched", map[string]any{"reason": reason, "keypad_role": keypadRole})
	return true
}

// grantDoorLatchToggle handles an accepted PIN with function code 2 in the door modes.
func (ctx *AppContext) grantDoorLatchToggle(pin string, cfg DeviceConfig, mode, keypadRole, credLabel string, feedbackDelay time.Duration) {
	kTag := keypadLogTag(keypadRole)
	wh := map[string]any{"operation_mode": mode, "keypad_role": keypadRole, "credential_label": credLabel, "fn_code": keypadFnLatch}
	if ctx.FiremansServiceActive() {
		log.Printf("INFO: Door latch refused (fireman's service active; %s keypad).", kTag)
		lcdEnqueueFullSync(ctx, lcdLinesDenied("Latch Unavailable", "", ""), lcdAutoIdleAfter)
		fireEventWebhook(ctx, "pin_rejected", map[string]any{"reason": "latch_unavailable", "keypad_role": keypadRole, "credential_label": credLabel})
		ctx.playRejectSound(keypadRole, cfg)
		return
	}
	if ctx.releaseDoorLatch("keypad", keypadRole) {
		wh["latch"] = "off"
		lcdEnqueueFullSync(ctx, lcdLinesLatch(false), lcdAutoIdleAfter)
		fireEventWebhook(ctx, "pin_accepted", wh)
		ctx.playOKSound(keypadRole, cfg)
		ctx.credentialRecordSuccessfulUse(pin, mode, keypadRole)
		ctx.holdInput(keypadRole, feedbackDelay)
		return
	}
	if err := ctx.latchDoor(cfg.KeypadFnLatchMax); err != nil {
		log.Printf("ERROR: Door latch relay actuation failed (hardware_actuation_failed): %v", err)
		fireEventWebhook(ctx, "hardware_actuation_failed", map[string]any{"keypad_role": keypadRole, "error": err.Error(), "reason": "door_latch"})
		lcdShowCommFail(ctx)
		ctx.playRejectSound(keypadRole, cfg)
		ctx.holdInput(keypadRole, feedbackDelay)
		return
	}
	log.Printf("INFO: Door latched open (%s keypad; credential=%s; auto-release after %s).", kTag, credLabel, cfg.KeypadFnLatchMax)
	wh["latch"] = "on"
	lcdEnqueueFullSync(ctx, lcdLinesLatch(true), lcdAutoIdleAfter)
	fireEventWebhook(ctx, "pin_accepted", wh)
	fireEventWebhook(ctx, "door_latched", map[string]any{"keypad_role": keypadRole, "credential_label": credLabel, "latch_max": cfg.KeypadFnLatchMax.String()})
	ctx.playOKSound(keypadRole, cfg)
	ctx.credentialRecordSuccessfulUse(pin, mode, keypadRole)
	ctx.holdInput(keypadRole, feedbackDelay)
}
