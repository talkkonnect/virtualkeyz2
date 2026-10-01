package app

import (
	"log"
	"maps"
	"slices"
)

// FiremansServiceActive reports whether emergency bypass / fireman's service is engaged (runtime state).
func (ctx *AppContext) FiremansServiceActive() bool {
	ctx.firemansMu.RLock()
	defer ctx.firemansMu.RUnlock()
	return ctx.firemansActive
}

// FireAlarmInterfaceActive reports whether the fire-alarm interface input is latched active (fail-unlock / door held).
func (ctx *AppContext) FireAlarmInterfaceActive() bool {
	ctx.fireAlarmMu.RLock()
	defer ctx.fireAlarmMu.RUnlock()
	return ctx.fireAlarmActive
}

func (ctx *AppContext) applyFireAlarmInterfaceTransition(wantActive bool, reason string) {
	ctx.fireAlarmMu.Lock()
	prev := ctx.fireAlarmActive
	if prev == wantActive {
		ctx.fireAlarmMu.Unlock()
		return
	}
	ctx.fireAlarmActive = wantActive
	ctx.fireAlarmMu.Unlock()

	if wantActive {
		log.Printf("INFO: Fire alarm interface ACTIVE (fail-unlock; reason=%q): holding door relay energized.", reason)
		debugf("Fire alarm interface: GPIO latched active — door strike / operator held open until input clears.")
		lcdShowFireAlarm(ctx, true)
		if ctx.GPIO != nil && ctx.GPIO.HasOutput("door") {
			ctx.GPIO.ActionOn("door")
		} else if ctx.GPIO == nil {
			debugf("Fire alarm interface: no GPIO manager; software state only.")
		}
		return
	}
	log.Printf("INFO: Fire alarm interface CLEARED (reason=%q): releasing door relay hold.", reason)
	debugf("Fire alarm interface: GPIO inactive — returning door relay to software control.")
	lcdShowFireAlarm(ctx, false)
	if ctx.GPIO != nil && ctx.GPIO.HasOutput("door") {
		ctx.GPIO.ActionOff("door")
	}
}

// syncFireAlarmFromHardwareReason samples the fire-alarm interface BCM input (if configured) and updates runtime state.
func (ctx *AppContext) syncFireAlarmFromHardwareReason(reason string) {
	ctx.configMu.RLock()
	pin := ctx.GPIOSettings.FireAlarmInterfacePin
	activeLow := ctx.GPIOSettings.FireAlarmInterfaceActiveLow
	ctx.configMu.RUnlock()
	if pin == 0 || ctx.GPIO == nil {
		return
	}
	inp, ok := ctx.GPIO.Inputs["fire_alarm_interface"]
	if !ok || inp.Line == nil {
		return
	}
	isLow, err := gpioLineIsPhysicalLow(inp.Line)
	if err != nil {
		return
	}
	want := isLow == activeLow
	debugf("Fire alarm interface: GPIO sample BCM=%d read_low=%v active_low_wiring=%v => alarm_active=%v (%s).",
		pin, isLow, activeLow, want, reason)
	ctx.applyFireAlarmInterfaceTransition(want, "gpio:"+reason)
}

func (ctx *AppContext) syncFireAlarmAfterConfigReload() {
	ctx.configMu.RLock()
	pin := ctx.GPIOSettings.FireAlarmInterfacePin
	ctx.configMu.RUnlock()
	if pin == 0 && ctx.FireAlarmInterfaceActive() {
		ctx.applyFireAlarmInterfaceTransition(false, "config_reload_pin_disabled")
		return
	}
	if pin != 0 {
		ctx.syncFireAlarmFromHardwareReason("config_reload")
	}
}

func setupFireAlarmInterfaceGPIOInput(ctx *AppContext, gpio *GPIOManager) {
	ctx.configMu.RLock()
	pin := ctx.GPIOSettings.FireAlarmInterfacePin
	pullUp := ctx.GPIOSettings.FireAlarmInterfaceActiveLow
	ctx.configMu.RUnlock()
	if pin == 0 {
		return
	}
	gpio.AddInputAnyEdge("fire_alarm_interface", pin, pullUp, func() {
		ctx.syncFireAlarmFromHardwareReason("edge")
	})
	log.Printf("INFO: Fire alarm interface input: BCM %d (active_low_wiring=%v, pull_up=%v).", pin, pullUp, pullUp)
}

func setupTamperSwitchGPIOInput(ctx *AppContext, gpio *GPIOManager) {
	ctx.configMu.RLock()
	pin := ctx.GPIOSettings.TamperSwitchPin
	activeLow := ctx.GPIOSettings.TamperSwitchActiveLow
	ctx.configMu.RUnlock()
	if pin == 0 {
		return
	}
	gpio.AddInputAnyEdge("tamper_switch", pin, activeLow, func() {
		inp, ok := gpio.Inputs["tamper_switch"]
		if !ok || inp.Line == nil {
			return
		}
		isLow, err := gpioLineIsPhysicalLow(inp.Line)
		if err != nil {
			return
		}
		secure := isLow == activeLow
		debugf("Tamper switch: edge on BCM %d read_low=%v active_low_wiring=%v => enclosure_secure=%v.",
			pin, isLow, activeLow, secure)
		if secure {
			lcdShowIdle(ctx)
		} else {
			lcdShowTamperAlert(ctx)
		}
	})
	log.Printf("INFO: Tamper switch input: BCM %d (active_low_wiring=%v, pull_up=%v).", pin, activeLow, activeLow)
}

func setupMotionSensorGPIOInput(ctx *AppContext, gpio *GPIOManager) {
	ctx.configMu.RLock()
	pin := ctx.GPIOSettings.MotionSensorPin
	activeLow := ctx.GPIOSettings.MotionSensorActiveLow
	ctx.configMu.RUnlock()
	if pin == 0 {
		return
	}
	gpio.AddInput("motion_sensor", pin, activeLow, func() {
		debugf("Motion sensor: presence / approach detected on BCM %d (active_low_wiring=%v).", pin, activeLow)
	})
	log.Printf("INFO: Motion sensor input: BCM %d (active_low_wiring=%v, pull_up=%v; edge=asserted).", pin, activeLow, activeLow)
}

// pulseAuthorizedAccessAuxRelays pulses automatic door operator and intercom/camera trigger relays after a normal access grant.
func (ctx *AppContext) pulseAuthorizedAccessAuxRelays(cfg DeviceConfig) {
	if ctx.GPIO == nil || ctx.FiremansServiceActive() {
		return
	}
	ado := cfg.AutomaticDoorOperatorPulseDuration
	if ado <= 0 {
		ado = cfg.RelayPulseDuration
	}
	ict := cfg.IntercomCameraTriggerPulseDuration
	if ctx.GPIO.HasOutput("automatic_door_operator") {
		debugf("Automatic door operator relay: pulse %s (authorized access).", ado)
		ctx.GPIO.ActionPulse("automatic_door_operator", ado)
	}
	if ctx.GPIO.HasOutput("intercom_camera_trigger") {
		debugf("Intercom/camera trigger relay: pulse %s (authorized access).", ict)
		ctx.GPIO.ActionPulse("intercom_camera_trigger", ict)
	}
}

func deenergizeAllRelayOutputs(gpio *GPIOManager, holdDoorOpenForFireAlarm bool) {
	if gpio == nil {
		return
	}
	for _, n := range slices.Sorted(maps.Keys(gpio.Outputs)) {
		if holdDoorOpenForFireAlarm && n == "door" {
			debugf("Fire alarm interface: skipping door relay in bulk de-energize (fail-unlock hold).")
			continue
		}
		gpio.ActionOff(n)
	}
}

func (ctx *AppContext) firemansStopLightingAutoOffTimer() {
	ctx.lightingMu.Lock()
	if ctx.lightingOffTimer != nil {
		ctx.lightingOffTimer.Stop()
		ctx.lightingOffTimer = nil
	}
	ctx.lightingTimerGen++
	ctx.lightingMu.Unlock()
}

func (ctx *AppContext) syncFiremansServiceAfterConfigReload() {
	ctx.configMu.RLock()
	en := ctx.Config.FiremansServiceEnabled
	ctx.configMu.RUnlock()
	if !en && ctx.FiremansServiceActive() {
		ctx.applyFiremansServiceTransition(false, "config_reload_disabled")
		return
	}
	if en {
		ctx.syncFiremansServiceFromHardwareReason("config_reload")
	}
}

// syncFiremansServiceFromHardwareReason reads the fireman's GPIO (if configured) and aligns runtime state.
func (ctx *AppContext) syncFiremansServiceFromHardwareReason(reason string) {
	ctx.configMu.RLock()
	en := ctx.Config.FiremansServiceEnabled
	pin := ctx.GPIOSettings.FiremansServiceInputPin
	activeLow := ctx.GPIOSettings.FiremansServiceActiveLow
	ctx.configMu.RUnlock()
	if !en || pin == 0 || ctx.GPIO == nil {
		return
	}
	inp, ok := ctx.GPIO.Inputs["firemans_service"]
	if !ok || inp.Line == nil {
		return
	}
	isLow, err := gpioLineIsPhysicalLow(inp.Line)
	if err != nil {
		return
	}
	want := isLow == activeLow
	debugf("Fireman's service: GPIO sample BCM=%d read_low=%v active_low_wiring=%v => emergency_active=%v (%s).",
		pin, isLow, activeLow, want, reason)
	ctx.applyFiremansServiceTransition(want, "gpio:"+reason)
}

// applyFiremansServiceTransition sets emergency bypass on or off (idempotent). Respects device.firemans_service_enabled for activate only.
func (ctx *AppContext) applyFiremansServiceTransition(wantActive bool, reason string) {
	ctx.configMu.RLock()
	en := ctx.Config.FiremansServiceEnabled
	ctx.configMu.RUnlock()
	if wantActive && !en {
		debugf("Fireman's service: ignoring activate (reason=%q); firemans_service_enabled is false.", reason)
		return
	}

	ctx.firemansMu.Lock()
	prev := ctx.firemansActive
	if prev == wantActive {
		ctx.firemansMu.Unlock()
		return
	}
	ctx.firemansActive = wantActive
	ctx.firemansMu.Unlock()

	if wantActive {
		ctx.runFiremansServiceEnter(reason)
	} else {
		ctx.runFiremansServiceExit(reason)
	}
}

func (ctx *AppContext) runFiremansServiceEnter(reason string) {
	log.Printf("INFO: Fireman's service ACTIVATED (emergency bypass; reason=%q): de-energizing all relay outputs; lighting on; access schedules and elevator floor rules bypassed for valid credentials.", reason)
	debugf("Fireman's service: enter — clearing elevator grant state and lighting auto-off timer.")
	ctx.firemansStopLightingAutoOffTimer()
	clearElevatorGrantState(ctx)
	if ctx.GPIO != nil {
		deenergizeAllRelayOutputs(ctx.GPIO, ctx.FireAlarmInterfaceActive())
		if ctx.FireAlarmInterfaceActive() && ctx.GPIO.HasOutput("door") {
			ctx.GPIO.ActionOn("door")
			debugf("Fire alarm interface: door relay re-energized after fireman's service bulk off (fail-unlock).")
		}
		if ctx.GPIO.HasOutput("lighting") {
			ctx.GPIO.ActionOn("lighting")
			debugf("Fireman's service: lighting relay held ON (emergency illumination).")
		} else {
			debugf("Fireman's service: no lighting_relay_pin configured; skip lighting hold.")
		}
	} else {
		debugf("Fireman's service: GPIO unavailable; software bypass only (no relay or lighting control).")
	}

	ctx.configMu.RLock()
	cfg := ctx.Config
	ctx.configMu.RUnlock()
	playSoundEnabled(cfg, cfg.SoundFiremansActivated, cfg.SoundFiremansActivatedEnabled, cfg.SoundFiremansActivatedBlocking)
	fireEventWebhook(ctx, "firemans_service_activated", map[string]any{"reason": reason})
	lcdShowFiremans(ctx, true)
}

func (ctx *AppContext) runFiremansServiceExit(reason string) {
	log.Printf("INFO: Fireman's service DEACTIVATED (reason=%q): returning to normal software relay control; lighting relay off if configured.", reason)
	debugf("Fireman's service: exit — stopping lighting auto-off timer; clearing elevator grant.")
	ctx.firemansStopLightingAutoOffTimer()
	clearElevatorGrantState(ctx)
	if ctx.GPIO != nil && ctx.GPIO.HasOutput("lighting") {
		ctx.GPIO.ActionOff("lighting")
		debugf("Fireman's service: lighting relay OFF after emergency mode end.")
	}

	ctx.configMu.RLock()
	cfg := ctx.Config
	ctx.configMu.RUnlock()
	playSoundEnabled(cfg, cfg.SoundFiremansDeactivated, cfg.SoundFiremansDeactivatedEnabled, cfg.SoundFiremansDeactivatedBlocking)
	fireEventWebhook(ctx, "firemans_service_deactivated", map[string]any{"reason": reason})
	lcdShowFiremans(ctx, false)
}

func setupFiremansServiceGPIOInput(ctx *AppContext, gpio *GPIOManager) {
	ctx.configMu.RLock()
	en := ctx.Config.FiremansServiceEnabled
	pin := ctx.GPIOSettings.FiremansServiceInputPin
	pullUp := ctx.GPIOSettings.FiremansServiceActiveLow
	ctx.configMu.RUnlock()
	if !en || pin == 0 {
		return
	}
	gpio.AddInputAnyEdge("firemans_service", pin, pullUp, func() {
		ctx.syncFiremansServiceFromHardwareReason("edge")
	})
	log.Printf("INFO: Fireman's service input: BCM %d (active_low_wiring=%v, pull_up=%v).", pin, pullUp, pullUp)
}
