package app

import (
	"log"
	"slices"
	"strings"
	"time"
	"virtualkeyz2/internal/config"
	"virtualkeyz2/internal/outputnames"
)

func clearElevatorGrantState(ctx *AppContext) {
	ctx.elevatorMu.Lock()
	ctx.elevatorGrantUntil = time.Time{}
	ctx.elevatorGrantStartedAt = time.Time{}
	ctx.elevatorCabFloorDebounceHeld = nil
	ctx.elevatorCabFloorDebounceTick = 0
	ctx.elevatorGrantPIN = ""
	ctx.elevatorGrantViaFallback = false
	ctx.elevatorStaticTestFloorACLBypass.Store(false)
	ctx.elevatorMu.Unlock()
	if ctx.GPIO == nil {
		return
	}
	for i := range ctx.elevatorWaitFloorEnablePins {
		name := outputnames.ElevatorWaitFloorEnable(i)
		if ctx.GPIO.HasOutput(name) {
			ctx.GPIO.ActionOff(name)
		}
	}
	if ctx.GPIO.HasOutput("elevator_enable") {
		ctx.GPIO.ActionOff("elevator_enable")
	}
}

func startElevatorFloorWaitGrant(ctx *AppContext, cfg DeviceConfig) {
	if ctx.FiremansServiceActive() {
		debugf("Fireman's service: startElevatorFloorWaitGrant skipped (enable relays stay de-energized).")
		clearElevatorGrantState(ctx)
		return
	}
	ctx.elevatorMu.Lock()
	pin := ctx.elevatorGrantPIN
	via := ctx.elevatorGrantViaFallback
	ctx.elevatorMu.Unlock()

	elevID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
	now := time.Now()

	ctx.elevatorMu.Lock()
	ctx.elevatorGrantUntil = now.Add(cfg.ElevatorFloorWaitTimeout)
	ctx.elevatorGrantStartedAt = now
	ctx.elevatorCabFloorDebounceHeld = nil
	ctx.elevatorCabFloorDebounceTick = 0
	ctx.elevatorMu.Unlock()
	if ctx.GPIO == nil {
		return
	}
	// Hold ground-return / enable relays for the full wait window (until clearElevatorGrantState on
	// floor press or timeout). elevator_enable_pulse_duration does not apply here—only elevator_predefined_floor uses it.
	if len(ctx.elevatorWaitFloorEnablePins) > 0 {
		for i := range ctx.elevatorWaitFloorEnablePins {
			if !ctx.elevatorFloorChannelAllowed(pin, elevID, i, via, now) {
				continue
			}
			name := outputnames.ElevatorWaitFloorEnable(i)
			if ctx.GPIO.HasOutput(name) {
				ctx.GPIO.ActionOn(name)
			}
		}
		return
	}
	// Legacy single shared enable relay: hardware cannot isolate per floor; PIN/time rules are enforced when a floor is selected.
	if ctx.GPIO.HasOutput("elevator_enable") {
		ctx.GPIO.ActionOn("elevator_enable")
	}
}

func pulseElevatorOrDoorOutput(ctx *AppContext, cfg DeviceConfig) bool {
	if ctx.GPIO == nil {
		return false
	}
	dur := cfg.ElevatorDispatchPulseDuration
	if ctx.GPIO.HasOutput("elevator_dispatch") {
		ctx.GPIO.ActionPulse("elevator_dispatch", dur)
		return true
	}
	ctx.GPIO.ActionPulse("door", dur)
	return true
}

// pulseElevatorFloorSelections pulses per-floor dispatch outputs for each cab index, or one legacy dispatch/door pulse.
func pulseElevatorFloorSelections(ctx *AppContext, cfg DeviceConfig, floorIndices []int) bool {
	if ctx.GPIO == nil || len(floorIndices) == 0 {
		return false
	}
	perFloor := false
	for _, idx := range floorIndices {
		if ctx.GPIO.HasOutput(outputnames.ElevatorFloorDispatch(idx)) {
			perFloor = true
			break
		}
	}
	if !perFloor {
		return pulseElevatorOrDoorOutput(ctx, cfg)
	}
	for _, idx := range floorIndices {
		name := outputnames.ElevatorFloorDispatch(idx)
		if ctx.GPIO.HasOutput(name) {
			ctx.GPIO.ActionPulse(name, dispatchPulseDurationForFloor(cfg, idx))
		}
	}
	return true
}

func pulseElevatorPredefinedDispatchAtIndex(ctx *AppContext, cfg DeviceConfig, idx int) (outName string, pin int, ok bool) {
	if ctx.GPIO == nil {
		return "", 0, false
	}
	n := len(ctx.elevatorFloorDispatchPins)
	if n == 0 {
		ok = pulseElevatorOrDoorOutput(ctx, cfg)
		if ctx.GPIO.HasOutput("elevator_dispatch") {
			return "elevator_dispatch", int(ctx.GPIOSettings.ElevatorDispatchRelayPin), ok
		}
		return "door", int(ctx.GPIOSettings.DoorRelayPin), ok
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	name := outputnames.ElevatorFloorDispatch(idx)
	if ctx.GPIO.HasOutput(name) {
		ctx.GPIO.ActionPulse(name, dispatchPulseDurationForFloor(cfg, idx))
		return name, int(ctx.elevatorFloorDispatchPins[idx]), true
	}
	ok = pulseElevatorOrDoorOutput(ctx, cfg)
	if ctx.GPIO.HasOutput("elevator_dispatch") {
		return "elevator_dispatch", int(ctx.GPIOSettings.ElevatorDispatchRelayPin), ok
	}
	return "door", int(ctx.GPIOSettings.DoorRelayPin), ok
}

// activateElevatorPredefinedFloor pulses the enable relay (if configured) and dispatch for the selected predefined floor; returns webhook detail fields.
// The second return is false when elevatorFloorChannelAllowed denies (PIN floor list / floor groups, or timed lock).
func activateElevatorPredefinedFloor(ctx *AppContext, cfg DeviceConfig, kTag, credTag, pin string, viaFallback bool) (map[string]any, bool) {
	extra := map[string]any{}
	if ctx.FiremansServiceActive() {
		aclIdx := ctx.elevatorPredefinedDispatchIndexForACL(cfg)
		elevID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
		log.Printf("INFO: PIN accepted (elevator predefined; fireman's service; %s keypad; credential=%s); relay pulses skipped (%s).",
			kTag, credTag, elevatorFloorLogLabel(ctx.DB, elevID, aclIdx))
		debugf("Fireman's service: elevator_predefined_floor — enable/dispatch outputs not pulsed.")
		extra["firemans_service"] = true
		extra["elevator_floor_index"] = aclIdx
		if elevID != "" {
			extra["access_control_elevator_id"] = elevID
		}
		return extra, true
	}
	aclIdx := ctx.elevatorPredefinedDispatchIndexForACL(cfg)
	elevID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
	if !ctx.elevatorFloorChannelAllowed(pin, elevID, aclIdx, viaFallback, time.Now()) {
		log.Printf("INFO: Elevator predefined floor denied (%s not permitted for credential or schedule; %s keypad; credential=%s).", elevatorFloorLogLabel(ctx.DB, elevID, aclIdx), kTag, credTag)
		return nil, false
	}
	if ctx.GPIO == nil {
		extra["gpio"] = "unavailable"
		log.Printf("INFO: PIN accepted (elevator predefined; %s keypad; credential=%s); GPIO unavailable.", kTag, credTag)
		return extra, true
	}
	nf := len(cfg.ElevatorPredefinedFloors)
	if nf == 0 {
		idx := cfg.ElevatorPredefinedFloor
		if len(ctx.elevatorFloorDispatchPins) > 0 {
			if idx < 0 {
				idx = 0
			}
			if idx >= len(ctx.elevatorFloorDispatchPins) {
				log.Printf("WARNING: elevator_predefined_floor %d out of range for %d dispatch relay(s); using index %d.", cfg.ElevatorPredefinedFloor, len(ctx.elevatorFloorDispatchPins), len(ctx.elevatorFloorDispatchPins)-1)
				idx = len(ctx.elevatorFloorDispatchPins) - 1
			}
		}
		dOut, dPin, dOK := pulseElevatorPredefinedDispatchAtIndex(ctx, cfg, idx)
		log.Printf("INFO: PIN accepted (elevator predefined legacy; %s keypad; credential=%s); configured_predefined_floors=[] logical_floor_label=%d dispatch_output=%q dispatch_relay_pin=%d dispatch_pulsed=%v",
			kTag, credTag, cfg.ElevatorPredefinedFloor, dOut, dPin, dOK)
		extra["elevator_predefined_logical_floor"] = cfg.ElevatorPredefinedFloor
		extra["dispatch_output"] = dOut
		extra["dispatch_relay_pin"] = dPin
		extra["dispatch_pulsed"] = dOK
		return extra, true
	}
	idx := cfg.ElevatorPredefinedFloor
	if idx < 0 {
		idx = 0
	}
	if idx >= nf {
		log.Printf("WARNING: elevator_predefined_floor index %d out of range for %d configured floors; using index %d.", cfg.ElevatorPredefinedFloor, nf, nf-1)
		idx = nf - 1
	}
	logical := cfg.ElevatorPredefinedFloors[idx]
	enOut, enPin := "", 0
	enName := outputnames.ElevatorPredefinedEnable(idx)
	if ctx.GPIO.HasOutput(enName) {
		enOut = enName
		if idx < len(ctx.elevatorPredefinedEnablePins) {
			enPin = int(ctx.elevatorPredefinedEnablePins[idx])
		}
		enDur := cfg.ElevatorEnablePulseDuration
		if enDur <= 0 {
			enDur = dispatchPulseDurationForFloor(cfg, idx)
			if enDur <= 0 {
				enDur = cfg.ElevatorDispatchPulseDuration
			}
		}
		ctx.GPIO.ActionPulse(enName, enDur)
	}
	dOut, dPin, dOK := pulseElevatorPredefinedDispatchAtIndex(ctx, cfg, idx)
	log.Printf("INFO: PIN accepted (elevator predefined; %s keypad; credential=%s); configured_logical_floors=%v selected_index=%d activated_logical_floor=%d enable_output=%q enable_relay_pin=%d dispatch_output=%q dispatch_relay_pin=%d dispatch_pulsed=%v",
		kTag, credTag, cfg.ElevatorPredefinedFloors, idx, logical, enOut, enPin, dOut, dPin, dOK)
	extra["elevator_predefined_floors_configured"] = cfg.ElevatorPredefinedFloors
	extra["elevator_predefined_selected_index"] = idx
	extra["elevator_predefined_logical_floor"] = logical
	if enOut != "" {
		extra["elevator_enable_output"] = enOut
		extra["elevator_enable_relay_pin"] = enPin
	}
	extra["dispatch_output"] = dOut
	extra["dispatch_relay_pin"] = dPin
	extra["dispatch_pulsed"] = dOK
	return extra, true
}

// Elevator monitor poll rates: fast only while a wait-floor grant is active (cab button debounce
// counts these ticks); slower while waiting for a grant (well under ElevatorCabSenseArmDelay plus
// the debounce); slowest when another operation mode is configured (mode can change via cfg set).
const (
	elevatorPollActive = 50 * time.Millisecond
	elevatorPollIdle   = 200 * time.Millisecond
	elevatorPollOff    = time.Second
)

func monitorElevatorFloorSelection(ctx *AppContext) {
	tick := time.NewTicker(elevatorPollOff)
	defer tick.Stop()
	interval := elevatorPollOff
	setInterval := func(d time.Duration) {
		if d != interval {
			interval = d
			tick.Reset(d)
		}
	}
	for range tick.C {
		ctx.configMu.RLock()
		mode := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
		senseCab := elevatorWaitFloorSenseCabInputs(ctx.Config)
		ctx.configMu.RUnlock()
		if mode != ModeElevatorWaitFloorButtons {
			setInterval(elevatorPollOff)
			continue
		}
		if ctx.FiremansServiceActive() {
			ctx.elevatorMu.Lock()
			untilFS := ctx.elevatorGrantUntil
			ctx.elevatorMu.Unlock()
			if !untilFS.IsZero() {
				debugf("Fireman's service: clearing elevator wait-floor grant (enable relays must remain off).")
				clearElevatorGrantState(ctx)
			}
			continue
		}
		ctx.elevatorMu.Lock()
		until := ctx.elevatorGrantUntil
		ctx.elevatorMu.Unlock()
		if until.IsZero() {
			setInterval(elevatorPollIdle)
			continue
		}
		setInterval(elevatorPollActive)
		if time.Now().After(until) {
			clearElevatorGrantState(ctx)
			if senseCab {
				log.Println("WARNING: Elevator floor-button wait window expired (cab sense enabled).")
				fireEventWebhook(ctx, "elevator_floor_timeout", map[string]any{"operation_mode": mode, "elevator_wait_floor_cab_sense": ElevatorWaitFloorCabSenseSense})
			} else {
				log.Println("INFO: Elevator wait-floor enable window ended (cab sense disabled; no floor GPIO).")
				fireEventWebhook(ctx, "elevator_floor_timeout", map[string]any{"operation_mode": mode, "elevator_wait_floor_cab_sense": ElevatorWaitFloorCabSenseIgnore})
			}
			continue
		}
		if !senseCab {
			continue
		}
		if ctx.GPIO == nil || !ctx.GPIO.HasElevatorFloorPins() {
			continue
		}
		ctx.elevatorMu.Lock()
		if ctx.elevatorGrantUntil.IsZero() || time.Now().After(ctx.elevatorGrantUntil) {
			ctx.elevatorMu.Unlock()
			continue
		}
		if ctx.elevatorGrantStartedAt.IsZero() || time.Since(ctx.elevatorGrantStartedAt) < config.ElevatorCabSenseArmDelay {
			ctx.elevatorMu.Unlock()
			continue
		}
		ctx.elevatorMu.Unlock()

		pressed := ctx.GPIO.ElevatorCabFloorsPressed()
		var toDispatch []int
		ctx.elevatorMu.Lock()
		if ctx.elevatorGrantUntil.IsZero() || time.Now().After(ctx.elevatorGrantUntil) {
			ctx.elevatorCabFloorDebounceHeld = nil
			ctx.elevatorCabFloorDebounceTick = 0
			ctx.elevatorMu.Unlock()
			continue
		}
		if len(pressed) == 0 {
			ctx.elevatorCabFloorDebounceHeld = nil
			ctx.elevatorCabFloorDebounceTick = 0
			ctx.elevatorMu.Unlock()
			continue
		}
		if slices.Equal(ctx.elevatorCabFloorDebounceHeld, pressed) {
			ctx.elevatorCabFloorDebounceTick++
		} else {
			ctx.elevatorCabFloorDebounceHeld = slices.Clone(pressed)
			ctx.elevatorCabFloorDebounceTick = 1
		}
		floorDenied := false
		var deniedIndices []int
		var denyElevLifecycle string
		var grantPin string
		if ctx.elevatorCabFloorDebounceTick >= config.ElevatorCabSenseStableTicks {
			held := slices.Clone(ctx.elevatorCabFloorDebounceHeld)
			pin := strings.TrimSpace(ctx.elevatorGrantPIN)
			via := ctx.elevatorGrantViaFallback
			grantGen := ctx.elevatorGrantStartedAt
			// Run the credential / floor ACL SQLite checks without holding elevatorMu, so processPIN
			// starting a new grant is never stalled behind them; re-validate the grant afterwards.
			ctx.elevatorMu.Unlock()
			elevID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
			if pin != "" {
				live := ctx.accessCredentialForPIN(pin)
				if !live.OK {
					floorDenied = true
					deniedIndices = held
					denyElevLifecycle = live.LifecycleReason
					if denyElevLifecycle == "" {
						denyElevLifecycle = "credential_invalid"
					}
				}
			}
			if !floorDenied {
				for _, fi := range held {
					if !ctx.elevatorFloorChannelAllowed(pin, elevID, fi, via, time.Now()) {
						floorDenied = true
						deniedIndices = held
						break
					}
				}
			}
			if !floorDenied {
				toDispatch = held
			}
			ctx.elevatorMu.Lock()
			if !ctx.elevatorGrantStartedAt.Equal(grantGen) || ctx.elevatorGrantUntil.IsZero() || time.Now().After(ctx.elevatorGrantUntil) {
				// Grant replaced, cleared or expired while checking; act on the current state next tick.
				ctx.elevatorMu.Unlock()
				continue
			}
			grantPin = pin
		}
		ctx.elevatorMu.Unlock()
		if floorDenied {
			clearElevatorGrantState(ctx)
			ctx.configMu.RLock()
			cfg := ctx.Config
			ctx.configMu.RUnlock()
			if denyElevLifecycle != "" {
				log.Printf("INFO: Elevator cab floor rejected (%s; floors=%v).", denyElevLifecycle, deniedIndices)
			} else {
				log.Printf("INFO: Elevator cab floor input(s) rejected (not permitted for credential or schedule): %v", deniedIndices)
			}
			lcdShowElevatorFloorDeny(ctx)
			playSoundEnabled(cfg, cfg.SoundPinReject, cfg.SoundPinRejectEnabled, cfg.SoundPinRejectBlocking)
			elevID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
			denyEx := map[string]any{
				"operation_mode":             mode,
				"elevator_floor_indices":     deniedIndices,
				"access_control_elevator_id": elevID,
			}
			if denyElevLifecycle != "" {
				denyEx["lifecycle_reason"] = denyElevLifecycle
			}
			if ctx.DB != nil && elevID != "" && len(deniedIndices) > 0 {
				denyEx["elevator_floor_labels"] = elevatorFloorLogLabels(ctx.DB, elevID, deniedIndices)
			}
			fireEventWebhook(ctx, "elevator_floor_denied", denyEx)
			continue
		}
		if len(toDispatch) == 0 {
			continue
		}
		clearElevatorGrantState(ctx)
		ctx.configMu.RLock()
		cfg := ctx.Config
		ctx.configMu.RUnlock()
		pulseElevatorFloorSelections(ctx, cfg, toDispatch)
		ctx.credentialRecordSuccessfulUse(grantPin, ModeElevatorWaitFloorButtons, "elevator_cab_floor")
		log.Printf("INFO: Elevator cab floor input(s) active %v; dispatch pulse sent.", toDispatch)
		fireEventWebhook(ctx, "elevator_floor_selected", map[string]any{"operation_mode": mode, "elevator_floor_indices": toDispatch})
	}
}

// grantElevatorKeypadFloor handles an accepted "<floor> <PIN>" keypad entry in
// elevator_wait_floor_buttons: the floor ACL is checked and the floor is dispatched directly,
// with no cab-button wait window. accepted fires pin_accepted and holds input.
func (ctx *AppContext) grantElevatorKeypadFloor(cfg DeviceConfig, mode string, feedbackDelay time.Duration, g grantRequest, accepted func(map[string]any)) {
	idx := g.keypadFloor
	elevID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
	credTag := g.credLabel
	if credTag == "" {
		credTag = "legacy_or_unlabeled"
	}
	floorLabel := func() string { return elevatorFloorLogLabel(ctx.DB, elevID, idx) }
	if !ctx.elevatorFloorChannelAllowed(g.pin, elevID, idx, g.viaFallback, time.Now()) {
		log.Printf("INFO: Elevator keypad floor %s rejected (not permitted for credential=%s or schedule).", floorLabel(), credTag)
		denyEx := map[string]any{
			"operation_mode":             mode,
			"keypad_role":                g.keypadRole,
			"elevator_floor_indices":     []int{idx},
			"access_control_elevator_id": elevID,
			"elevator_floor_labels":      []string{floorLabel()},
		}
		if g.credLabel != "" {
			denyEx["credential_label"] = g.credLabel
		}
		lcdShowElevatorFloorDeny(ctx)
		fireEventWebhook(ctx, "elevator_floor_denied", denyEx)
		ctx.playRejectSound(g.keypadRole, cfg)
		ctx.holdInput(g.keypadRole, feedbackDelay)
		return
	}
	// A direct selection replaces any cab-button wait window still open from an earlier PIN.
	clearElevatorGrantState(ctx)
	pulsed := pulseElevatorFloorSelections(ctx, cfg, []int{idx})
	log.Printf("INFO: PIN accepted (elevator wait-floor; keypad floor %s; credential=%s); dispatch pulse sent=%v.", floorLabel(), credTag, pulsed)
	lcdShowGranted(ctx, credTag)
	ctx.playOKSound(g.keypadRole, cfg)
	accepted(map[string]any{
		"elevator_phase":       "keypad_floor_select",
		"elevator_floor_index": idx,
		"relay_pulsed":         pulsed,
	})
	fireEventWebhook(ctx, "elevator_floor_selected", map[string]any{"operation_mode": mode, "elevator_floor_indices": []int{idx}, "keypad_role": g.keypadRole})
	ctx.credentialRecordSuccessfulUse(g.pin, mode, g.keypadRole)
}
