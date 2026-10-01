package app

import (
	"fmt"
	"log"
	"maps"
	"strings"
	"sync"
	"time"
)

// pinRejectWithStreak plays reject sound, increments wrong-PIN streak, fires webhook, optional buzzer/lockout.
func (ctx *AppContext) pinRejectWithStreak(cfg DeviceConfig, keypadRole string, buzzerBCM uint8, webhookReason string, extra map[string]any) {
	lcdRejectFromWebhookReason(ctx, webhookReason, keypadRole)
	ctx.pinFailMu.Lock()
	ctx.pinFailSeq++
	failCount := ctx.pinFailSeq
	ctx.pinFailMu.Unlock()
	wh := map[string]any{"reason": webhookReason, "wrong_pin_streak": failCount, "keypad_role": keypadRole}
	maps.Copy(wh, extra)
	// Fire the webhook BEFORE the (blocking) reject sound so remote displays
	// (fb-virtualkeyz2) show ACCESS DENIED in sync with the LCD, not after the
	// sound has finished playing.
	fireEventWebhook(ctx, "pin_rejected", wh)
	ctx.playRejectSound(keypadRole, cfg)
	ctx.configMu.RLock()
	buzzTh := ctx.Config.PinRejectBuzzerAfterAttempts
	buzzDur := ctx.Config.BuzzerRelayPulseDuration
	lockN := ctx.Config.PinLockoutAfterAttempts
	lockDur := ctx.Config.PinLockoutDuration
	lockOn := ctx.Config.PinLockoutEnabled
	ctx.configMu.RUnlock()
	buzzFire := buzzTh > 0 && failCount >= buzzTh && failCount%buzzTh == 0
	lockFire := lockOn && lockN > 0 && failCount >= lockN
	if buzzFire && ctx.FiremansServiceActive() {
		debugf("Fireman's service: wrong-PIN buzzer suppressed (buzzer relay held off).")
		buzzFire = false
	}
	if buzzFire {
		log.Printf("INFO: Wrong PIN count %d; pulsing buzzer relay (GPIO %d).", failCount, buzzerBCM)
		fireEventWebhook(ctx, "wrong_pin_buzzer", map[string]any{"wrong_pin_streak": failCount, "buzzer_relay_gpio": int(buzzerBCM)})
		if ctx.GPIO != nil {
			ctx.GPIO.ActionPulse("buzzer", buzzDur)
		} else {
			log.Println("WARNING: GPIO unavailable; buzzer relay pulse skipped.")
		}
	}
	if lockFire {
		ctx.keypadArmLockout(lockDur)
		ctx.ResetWrongPINCount()
		log.Printf("WARNING: Keypad lockout for %s (configured pin_lockout_duration) after %d failed PIN attempts.", lockDur, failCount)
		fireEventWebhook(ctx, "keypad_lockout_activated", map[string]any{
			"duration":        lockDur.String(),
			"failed_attempts": failCount,
			"lockout_enabled": lockOn,
		})
	}
	// No feedback-delay input hold: re-entry after a rejection is allowed immediately. The
	// displays hold ACCESS DENIED on their own (LCD auto-idle + fb result hold).
}

// grantDefaultModeDoorUnlockLikePIN performs the same unlock side effects as processPIN's default branch
// (non-elevator modes): door relay pulse (blocking when GPIO relays are used), welcome sound, optional pair-peer MQTT, pin_accepted webhook,
// then pin_entry_feedback_delay. whExtra is merged into the webhook payload (e.g. dual-keypad occupancy).
// pin is the credential (empty for physical entry/exit buttons); temporary credentials consume a use_count only after a successful door actuation.
// dualKeypadZoneBookkeepingChanged: when true and the door relay fails, dual-keypad occupancy updated before this call is reverted.
// doorHoldExtra extends the configured door_open_warning_after for the next open period (accessibility); use 0 when not from access_pins.
// Returns false when the door relay could not be actuated (after I2C retries and bus recovery); true on software-only grants and successful pulses.
func (ctx *AppContext) grantDefaultModeDoorUnlockLikePIN(pin string, cfg DeviceConfig, mode, keypadRole, credLabel string, doorBCM uint8, feedbackDelay time.Duration, doorHoldExtra time.Duration, whExtra map[string]any, dualKeypadZoneBookkeepingChanged bool) bool {
	ctx.noteDoorHoldExtraGrace(doorHoldExtra)
	okSound := cfg.SoundPinOK
	okEn := cfg.SoundPinOKEnabled
	okBlocking := cfg.SoundPinOKBlocking
	if credLabel == "exit_button" || credLabel == "entry_button" {
		okSound = cfg.SoundAccessGranted
		okEn = cfg.SoundAccessGrantedEnabled
		okBlocking = cfg.SoundAccessGrantedBlocking
	}
	// Exit/entry buttons run on their own goroutine and keep the old pause-and-block behaviour;
	// keypad/QR sources must never block (see input_hold.go).
	isButton := credLabel == "exit_button" || credLabel == "entry_button"
	pause := func() {
		if isButton {
			time.Sleep(feedbackDelay)
		} else {
			ctx.holdInput(keypadRole, feedbackDelay)
		}
	}
	playSound := func(path string, en, blocking bool) {
		if isButton {
			playSoundEnabled(cfg, path, en, blocking)
		} else {
			ctx.playFeedbackSound(keypadRole, cfg, path, en, blocking)
		}
	}
	relPulsed := false
	if ctx.FiremansServiceActive() {
		debugf("Fireman's service: grant path — door relay pulse suppressed; PIN lighting timer skipped (emergency lighting policy).")
	} else if ctx.GPIO != nil {
		offErr := ctx.doorPulseOffErrReporter(map[string]any{
			"operation_mode": mode, "keypad_role": keypadRole, "credential_label": credLabel, "door_relay_gpio": int(doorBCM),
		})
		if err := ctx.GPIO.ActionPulseChecked("door", cfg.RelayPulseDuration, offErr); err != nil {
			ctx.clearDoorHoldExtraGrace()
			if dualKeypadZoneBookkeepingChanged {
				ctx.revertDualKeypadZoneAfterFailedActuation(pin, keypadRole)
			}
			failWh := map[string]any{
				"operation_mode":   mode,
				"keypad_role":      keypadRole,
				"credential_label": credLabel,
				"door_relay_gpio":  int(doorBCM),
				"error":            err.Error(),
			}
			maps.Copy(failWh, whExtra)
			fireEventWebhook(ctx, "hardware_actuation_failed", failWh)
			log.Printf("ERROR: Door relay actuation failed (hardware_actuation_failed): %v", err)
			lcdShowCommFail(ctx)
			playSound(cfg.SoundPinReject, cfg.SoundPinRejectEnabled, cfg.SoundPinRejectBlocking)
			pause()
			return false
		}
		relPulsed = true
		ctx.pulseAuthorizedAccessAuxRelays(cfg)
	} else {
		log.Println("WARNING: GPIO unavailable; relay pulse skipped.")
	}
	lcdShowGranted(ctx, credLabel)
	// Fire the accepted webhook BEFORE the (blocking) welcome sound so remote
	// displays (fb-virtualkeyz2) show ACCESS GRANTED in sync with the LCD, not
	// after the sound has finished playing.
	wh := map[string]any{
		"operation_mode":   mode,
		"keypad_role":      keypadRole,
		"credential_label": credLabel,
		"door_relay_gpio":  int(doorBCM),
		"relay_pulsed":     relPulsed,
	}
	if ctx.FiremansServiceActive() {
		wh["firemans_service"] = true
	}
	maps.Copy(wh, whExtra)
	fireEventWebhook(ctx, "pin_accepted", wh)
	playSound(okSound, okEn, okBlocking)
	if strings.TrimSpace(pin) != "" && !ctx.FiremansServiceActive() {
		ctx.lightingEnergizeAndArmTimer(fmt.Sprintf("pin_accepted_%s", strings.TrimSpace(keypadRole)))
	}
	if pairedEntryPublishesToPeer(mode, cfg.PairPeerRole) {
		publishMQTTPairPeerPulse(ctx, cfg)
	}
	ctx.credentialRecordSuccessfulUse(pin, mode, keypadRole)
	pause()
	return true
}

func processPIN(ctx *AppContext, pin string, keypadRole string) {
	// Query local SQLite for permissions
	// Validate PIN against door constraints
	// Trigger Relay, Sound Event, MQTT Update, and Logging
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return
	}
	if keypadRole != "qr" {
		// QR path already showed "Scan Card / Then Enter PIN" just before we entered processPIN.
		lcdShowProcessing(ctx)
	}
	debugf("Processing PIN from %s keypad (length %d).", keypadLogTag(keypadRole), len(pin))
	ctx.configMu.RLock()
	cfg := ctx.Config
	doorBCM := ctx.GPIOSettings.DoorRelayPin
	buzzerBCM := ctx.GPIOSettings.BuzzerRelayPin
	feedbackDelay := cfg.PinEntryFeedbackDelay
	ctx.configMu.RUnlock()

	override := strings.TrimSpace(cfg.PinLockoutOverridePin)
	if override != "" && pin == override {
		ctx.keypadClearLockout()
		ctx.ResetWrongPINCount()
		log.Printf("INFO: Keypad lockout cleared by override PIN (%s keypad).", keypadLogTag(keypadRole))
		// The override/master PIN also opens the door, unless fireman's service
		// is holding the relays off.
		relPulsed := false
		if ctx.FiremansServiceActive() {
			debugf("Fireman's service active: override PIN door pulse suppressed.")
		} else if ctx.GPIO != nil {
			offErr := ctx.doorPulseOffErrReporter(map[string]any{"keypad_role": keypadRole, "door_relay_gpio": int(doorBCM), "reason": "override_pin"})
			if err := ctx.GPIO.ActionPulseChecked("door", cfg.RelayPulseDuration, offErr); err != nil {
				log.Printf("ERROR: Override PIN door relay actuation failed: %v", err)
				fireEventWebhook(ctx, "hardware_actuation_failed", map[string]any{
					"keypad_role": keypadRole, "door_relay_gpio": int(doorBCM), "error": err.Error(), "reason": "override_pin",
				})
			} else {
				relPulsed = true
				ctx.pulseAuthorizedAccessAuxRelays(cfg)
			}
		} else {
			log.Println("WARNING: GPIO unavailable; override PIN door pulse skipped.")
		}
		fireEventWebhook(ctx, "keypad_lockout_override", map[string]any{
			"keypad_role":     keypadRole,
			"relay_pulsed":    relPulsed,
			"door_relay_gpio": int(doorBCM),
		})
		lcdEnqueueFullSync(ctx, lcdLinesIdle(), 0)
		ctx.playOKSound(keypadRole, cfg)
		// No feedback-delay input hold: keep the keypad responsive (the display holds
		// the override result on its own).
		return
	}

	if ctx.keypadLockoutActive() && !ctx.FiremansServiceActive() {
		log.Printf("INFO: PIN rejected (keypad lockout; %s keypad).", keypadLogTag(keypadRole))
		lcdShowKeypadLockout(ctx)
		fireEventWebhook(ctx, "pin_rejected", map[string]any{"reason": "keypad_lockout", "keypad_role": keypadRole})
		ctx.playRejectSound(keypadRole, cfg)
		ctx.holdInput(keypadRole, feedbackDelay)
		return
	}

	cred := ctx.accessCredentialForPIN(pin)
	if cred.LifecycleReason != "" {
		log.Printf("INFO: PIN rejected (credential lifecycle: %s; %s keypad).", cred.LifecycleReason, keypadLogTag(keypadRole))
		ex := map[string]any{"lifecycle_reason": cred.LifecycleReason}
		if cred.Label != "" {
			ex["credential_label"] = cred.Label
		}
		ctx.pinRejectCredentialLifecycle(cfg, keypadRole, "credential_lifecycle", ex)
		return
	}
	pinOK := cred.OK
	credLabel := cred.Label
	modePre := NormalizeKeypadOperationMode(cfg.KeypadOperationMode)
	if pinOK && modePre == ModeAccessDualUSBKeypad && keypadRole == "exit" && cfg.DualKeypadRejectExitWithoutEntry && !ctx.FiremansServiceActive() && ctx.dualKeypadExitWouldMismatch(pin) {
		log.Printf("INFO: PIN rejected (exit keypad; no recorded entry for this credential; door not opened).")
		ex := map[string]any{}
		if credLabel != "" {
			ex["credential_label"] = credLabel
		}
		ctx.pinRejectWithStreak(cfg, keypadRole, buzzerBCM, "exit_without_recorded_entry", ex)
		return
	}

	if pinOK {
		doorID := ctx.effectiveAccessDoorID()
		if ok, schedReason := ctx.accessScheduleAllows(pin, doorID, time.Now(), cred.ViaFallback); !ok {
			log.Printf("INFO: PIN rejected (access schedule: %s; door=%q).", schedReason, doorID)
			ex := map[string]any{"schedule_reason": schedReason, "access_control_door_id": doorID}
			if credLabel != "" {
				ex["credential_label"] = credLabel
			}
			ctx.pinRejectWithStreak(cfg, keypadRole, buzzerBCM, "access_schedule", ex)
			return
		}
		if isElevatorKeypadMode(modePre) {
			elevID := ctx.effectiveAccessElevatorID()
			if ok, schedReason := ctx.accessScheduleAllowsElevator(pin, elevID, time.Now(), cred.ViaFallback); !ok {
				log.Printf("INFO: PIN rejected (access schedule: %s; elevator=%q).", schedReason, elevID)
				ex := map[string]any{"schedule_reason": schedReason, "access_control_elevator_id": elevID}
				if credLabel != "" {
					ex["credential_label"] = credLabel
				}
				ctx.pinRejectWithStreak(cfg, keypadRole, buzzerBCM, "access_schedule", ex)
				return
			}
		}

		ctx.ResetWrongPINCount()
		ctx.keypadClearLockout()

		mode := modePre
		kTag := keypadLogTag(keypadRole)
		credTag := credLabel
		if credTag == "" {
			credTag = "legacy_or_unlabeled"
		}

		if ctx.grantElevatorMode(cfg, mode, doorBCM, feedbackDelay, grantRequest{
			pin: pin, keypadRole: keypadRole, credLabel: credLabel, viaFallback: cred.ViaFallback,
		}) {
			return
		}
		var areaTotal, insideThis int
		var occMismatch string
		var zoneBookkeepingChanged bool
		if mode == ModeAccessDualUSBKeypad && (keypadRole == "entry" || keypadRole == "exit") {
			areaTotal, insideThis, occMismatch, zoneBookkeepingChanged = ctx.adjustDualKeypadOccupancy(pin, keypadRole)
			log.Printf("INFO: PIN accepted (dual USB %s keypad; credential=%s; people_in_area_total=%d; this_credential_inside=%d); door relay GPIO %d.",
				keypadRole, credTag, areaTotal, insideThis, doorBCM)
			if occMismatch != "" {
				log.Printf("WARNING: Dual keypad occupancy: %s (%s keypad; credential=%s) — door still opened.", occMismatch, keypadRole, credTag)
			}
		} else {
			if mode == ModeAccessDualUSBKeypad && keypadRole == "" {
				log.Printf("WARNING: access_dual_usb_keypad but keypad role unknown; occupancy not updated. Use distinct keypad_exit_evdev_path.")
			}
			log.Printf("INFO: PIN accepted (mode=%s %s keypad; credential=%s); door relay GPIO %d.", mode, kTag, credTag, doorBCM)
		}
		var whExtra map[string]any
		if mode == ModeAccessDualUSBKeypad && (keypadRole == "entry" || keypadRole == "exit") {
			whExtra = map[string]any{
				"access_area_occupancy_total": areaTotal,
				"credential_inside_count":     insideThis,
			}
			if occMismatch != "" {
				whExtra["occupancy_mismatch"] = occMismatch
			}
		}
		ctx.grantDefaultModeDoorUnlockLikePIN(pin, cfg, mode, keypadRole, credLabel, doorBCM, feedbackDelay, cred.DoorHoldExtra, whExtra, zoneBookkeepingChanged)
		return
	}

	log.Printf("INFO: PIN rejected (%s keypad).", keypadLogTag(keypadRole))
	ctx.pinRejectWithStreak(cfg, keypadRole, buzzerBCM, "invalid_pin", nil)
}

func triggerCallForHelp(ctx *AppContext) {
	// Signal the centralized operations center via API/MQTT
	// to call the user on the IP-based intercom
}

// grantRequest describes an accepted credential for grantElevatorMode.
type grantRequest struct {
	pin         string // credential PIN ("static_test_qr" for the static test code)
	keypadRole  string
	credLabel   string
	viaFallback bool
	staticTest  bool           // static test QR: skip floor ACL and use_count
	whExtra     map[string]any // merged into every webhook (e.g. auth_method)
}

// grantElevatorMode performs the grant side effects for the elevator operation modes and reports
// whether mode was one of them (otherwise the caller runs the door grant path).
func (ctx *AppContext) grantElevatorMode(cfg DeviceConfig, mode string, doorBCM uint8, feedbackDelay time.Duration, g grantRequest) bool {
	if mode != ModeElevatorWaitFloorButtons && mode != ModeElevatorPredefinedFloor {
		return false
	}
	kTag := keypadLogTag(g.keypadRole)
	what, src := "PIN accepted", kTag+" keypad"
	if g.staticTest {
		what, src = "Static test QR accepted", kTag
	}
	credTag := g.credLabel
	if credTag == "" {
		credTag = "legacy_or_unlabeled"
	}
	accepted := func(fields map[string]any) {
		wh := map[string]any{
			"operation_mode":   mode,
			"keypad_role":      g.keypadRole,
			"credential_label": g.credLabel,
			"door_relay_gpio":  int(doorBCM),
		}
		maps.Copy(wh, fields)
		maps.Copy(wh, g.whExtra)
		fireEventWebhook(ctx, "pin_accepted", wh)
		ctx.holdInput(g.keypadRole, feedbackDelay)
	}

	if mode == ModeElevatorWaitFloorButtons {
		cabSense := normalizeElevatorWaitFloorCabSense(cfg.ElevatorWaitFloorCabSense)
		if ctx.FiremansServiceActive() {
			log.Printf("INFO: %s (elevator wait-floor; fireman's service; %s; credential=%s); software enable/dispatch relays not used (cab_sense=%s).", what, src, credTag, cabSense)
			debugf("Fireman's service: elevator_wait_floor_buttons — skipping enable relay window and floor selection handler state.")
			lcdShowGranted(ctx, credTag)
			ctx.playOKSound(g.keypadRole, cfg)
			accepted(map[string]any{
				"elevator_phase":                "firemans_bypass_no_relay",
				"elevator_wait_floor_cab_sense": cabSense,
				"firemans_service":              true,
			})
			return true
		}
		if g.staticTest {
			ctx.elevatorStaticTestFloorACLBypass.Store(true)
		}
		ctx.elevatorMu.Lock()
		ctx.elevatorGrantPIN = g.pin
		ctx.elevatorGrantViaFallback = g.viaFallback
		ctx.elevatorMu.Unlock()
		log.Printf("INFO: %s (elevator wait-floor; %s; credential=%s); enable window started (cab_sense=%s).", what, src, credTag, cabSense)
		lcdShowGranted(ctx, credTag)
		ctx.playOKSound(g.keypadRole, cfg)
		startElevatorFloorWaitGrant(ctx, cfg)
		accepted(map[string]any{
			"elevator_phase":                "wait_floor_input",
			"elevator_wait_floor_cab_sense": cabSense,
			"floor_wait_until":              cfg.ElevatorFloorWaitTimeout.String(),
		})
		return true
	}

	// ModeElevatorPredefinedFloor
	if g.staticTest {
		ctx.elevatorStaticTestFloorACLBypass.Store(true)
	}
	ex, okElev := activateElevatorPredefinedFloor(ctx, cfg, kTag, credTag, g.pin, g.viaFallback)
	if g.staticTest {
		ctx.elevatorStaticTestFloorACLBypass.Store(false)
	}
	if !okElev {
		aclIdx := ctx.elevatorPredefinedDispatchIndexForACL(cfg)
		elevDenyID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
		denyEx := map[string]any{
			"keypad_role":                g.keypadRole,
			"elevator_floor_index":       aclIdx,
			"access_control_elevator_id": elevDenyID,
		}
		if g.credLabel != "" {
			denyEx["credential_label"] = g.credLabel
		}
		maps.Copy(denyEx, g.whExtra)
		if ctx.DB != nil && elevDenyID != "" {
			denyEx["elevator_floor_label"] = elevatorFloorLogLabel(ctx.DB, elevDenyID, aclIdx)
		}
		lcdShowElevatorFloorDeny(ctx)
		ctx.playRejectSound(g.keypadRole, cfg)
		fireEventWebhook(ctx, "elevator_floor_denied", denyEx)
		ctx.holdInput(g.keypadRole, feedbackDelay)
		return true
	}
	lcdShowGranted(ctx, credTag)
	ctx.playOKSound(g.keypadRole, cfg)
	accepted(ex)
	if !g.staticTest {
		ctx.credentialRecordSuccessfulUse(g.pin, ModeElevatorPredefinedFloor, g.keypadRole)
	}
	return true
}

func (ctx *AppContext) keypadLockoutActive() bool {
	ctx.configMu.RLock()
	enabled := ctx.Config.PinLockoutEnabled
	ctx.configMu.RUnlock()
	if !enabled {
		ctx.keypadClearLockout()
		return false
	}
	ctx.keypadLockoutMu.Lock()
	defer ctx.keypadLockoutMu.Unlock()
	if ctx.keypadLockoutUntil.IsZero() {
		return false
	}
	if time.Now().Before(ctx.keypadLockoutUntil) {
		return true
	}
	// Lockout period ended (first check after deadline, e.g. next keypress).
	if ctx.keypadLockoutEndTimer != nil {
		ctx.keypadLockoutEndTimer.Stop()
		ctx.keypadLockoutEndTimer = nil
	}
	if ctx.keypadLockoutEndLogOnce != nil {
		ctx.keypadLockoutEndLogOnce.Do(func() {
			log.Println("WARNING: Keypad lockout period ended; keypad accepting input again.")
		})
	}
	ctx.keypadLockoutUntil = time.Time{}
	ctx.keypadLockoutEndLogOnce = nil
	return false
}

func (ctx *AppContext) keypadArmLockout(d time.Duration) {
	if d <= 0 {
		return
	}
	ctx.configMu.RLock()
	on := ctx.Config.PinLockoutEnabled
	ctx.configMu.RUnlock()
	if !on {
		return
	}
	ctx.keypadLockoutMu.Lock()
	defer ctx.keypadLockoutMu.Unlock()
	if ctx.keypadLockoutEndTimer != nil {
		ctx.keypadLockoutEndTimer.Stop()
		ctx.keypadLockoutEndTimer = nil
	}
	ctx.keypadLockoutEndLogOnce = new(sync.Once)
	onceRef := ctx.keypadLockoutEndLogOnce
	ctx.keypadLockoutUntil = time.Now().Add(d)
	ctx.keypadLockoutEndTimer = time.AfterFunc(d, func() {
		ctx.keypadLockoutMu.Lock()
		defer ctx.keypadLockoutMu.Unlock()
		ctx.keypadLockoutEndTimer = nil
		if onceRef != nil {
			onceRef.Do(func() {
				log.Println("WARNING: Keypad lockout period ended; keypad accepting input again.")
			})
		}
		ctx.keypadLockoutUntil = time.Time{}
		ctx.keypadLockoutEndLogOnce = nil
	})
}

func (ctx *AppContext) keypadClearLockout() {
	ctx.keypadLockoutMu.Lock()
	defer ctx.keypadLockoutMu.Unlock()
	if ctx.keypadLockoutEndTimer != nil {
		ctx.keypadLockoutEndTimer.Stop()
		ctx.keypadLockoutEndTimer = nil
	}
	ctx.keypadLockoutEndLogOnce = nil
	ctx.keypadLockoutUntil = time.Time{}
}

func (ctx *AppContext) keypadLockoutRemaining() time.Duration {
	ctx.configMu.RLock()
	on := ctx.Config.PinLockoutEnabled
	ctx.configMu.RUnlock()
	if !on {
		return 0
	}
	ctx.keypadLockoutMu.Lock()
	defer ctx.keypadLockoutMu.Unlock()
	if ctx.keypadLockoutUntil.IsZero() || !time.Now().Before(ctx.keypadLockoutUntil) {
		return 0
	}
	return time.Until(ctx.keypadLockoutUntil)
}
