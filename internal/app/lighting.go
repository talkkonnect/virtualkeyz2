package app

import (
	"strings"
	"time"
)

func lightingManualControlReady(ctx *AppContext) bool {
	if ctx.GPIO == nil {
		return false
	}
	ctx.configMu.RLock()
	btn := ctx.GPIOSettings.LightingButtonPin
	relay := ctx.GPIOSettings.LightingRelayPin
	ctx.configMu.RUnlock()
	return btn != 0 && relay != 0 && ctx.GPIO.HasOutput("lighting")
}

// lightingRelayOutputReady is true when a lighting relay output exists (non-zero pin and GPIO registered).
func lightingRelayOutputReady(ctx *AppContext) bool {
	if ctx.GPIO == nil || !ctx.GPIO.HasOutput("lighting") {
		return false
	}
	ctx.configMu.RLock()
	relay := ctx.GPIOSettings.LightingRelayPin
	ctx.configMu.RUnlock()
	return relay != 0
}

// lightingEnergizeAndArmTimer turns the lighting relay on (if not already held by an active auto-off timer) and (re)starts the auto-off timer. Reloading the timer while it is running does not pulse the relay off; the relay is only de-energized when the timer fires.
func (ctx *AppContext) lightingEnergizeAndArmTimer(reason string) {
	if strings.TrimSpace(reason) == "" {
		reason = "timer_reset"
	}
	if ctx.FiremansServiceActive() {
		debugf("Fireman's service: lighting timer arm skipped (%s); emergency mode holds lighting relay on without auto-off.", reason)
		return
	}
	if !lightingRelayOutputReady(ctx) {
		debugf("Lighting: skip arm (%s): relay output not ready (GPIO or lighting_relay_pin).", reason)
		return
	}
	ctx.configMu.RLock()
	timeout := ctx.Config.LightingTimeout
	setPath := strings.TrimSpace(ctx.Config.SoundLightingTimerSet)
	soundCard := ctx.Config.SoundCardName
	setEn := ctx.Config.SoundLightingTimerSetEnabled
	setBlocking := ctx.Config.SoundLightingTimerSetBlocking
	ctx.configMu.RUnlock()

	ctx.lightingMu.Lock()
	hadRunningTimer := ctx.lightingOffTimer != nil
	if ctx.lightingOffTimer != nil {
		debugf("Lighting: stopping previous auto-off timer before re-arm (%s).", reason)
		ctx.lightingOffTimer.Stop()
		ctx.lightingOffTimer = nil
	}
	ctx.lightingTimerGen++
	gen := ctx.lightingTimerGen
	if hadRunningTimer {
		debugf("Lighting: timer reload to full duration %s (%s); relay left ON (gen=%d).", timeout, reason, gen)
	} else {
		debugf("Lighting: energizing relay and starting auto-off %s (%s; gen=%d).", timeout, reason, gen)
		ctx.GPIO.ActionOn("lighting")
	}
	ctx.lightingOffTimer = time.AfterFunc(timeout, func() {
		ctx.lightingTimerExpired(gen)
	})
	ctx.lightingMu.Unlock()

	// Notify remote displays (fb-virtualkeyz2): lights are on and the auto-off
	// timer was (re)armed to the full duration. timeout_seconds lets the display
	// show a countdown until the light turns off. Sent on both energize and
	// reload so the countdown resets whenever a button/PIN extends the timer.
	lightsWh := map[string]any{"reason": reason, "timeout_seconds": int(timeout.Seconds())}
	if hadRunningTimer {
		// Timer extended while already on — refresh the remote countdown only,
		// without an audit-log row (the relay did not change state).
		postEventWebhook(ctx, "lights_on", lightsWh)
	} else {
		// Genuine OFF -> ON transition.
		fireEventWebhook(ctx, "lights_on", lightsWh)
	}
	debugf("Lighting: auto-off timer armed for %s [%s] gen=%d.", timeout, reason, gen)
	playSoundEnabled(DeviceConfig{SoundCardName: soundCard}, setPath, setEn, setBlocking)
}

func (ctx *AppContext) lightingTimerExpired(gen uint64) {
	debugf("Lighting: auto-off callback fired (gen=%d).", gen)
	ctx.lightingMu.Lock()
	if gen != ctx.lightingTimerGen {
		debugf("Lighting: auto-off ignored (stale gen=%d, current=%d); relay unchanged.", gen, ctx.lightingTimerGen)
		ctx.lightingMu.Unlock()
		return
	}
	ctx.lightingOffTimer = nil
	if ctx.GPIO != nil {
		debugf("Lighting: de-energizing relay (timer expired, gen=%d).", gen)
		ctx.GPIO.ActionOff("lighting")
	}
	ctx.lightingMu.Unlock()

	// Relay de-energized. Notify remote displays (fb-virtualkeyz2) so the bulb
	// icon turns off.
	fireEventWebhook(ctx, "lights_off", map[string]any{"reason": "timer_expired"})
	debugf("Lighting: relay OFF after lighting_timeout (gen=%d).", gen)
	ctx.configMu.RLock()
	expPath := strings.TrimSpace(ctx.Config.SoundLightingTimerExpired)
	soundCard := ctx.Config.SoundCardName
	expEn := ctx.Config.SoundLightingTimerExpiredEnabled
	expBlocking := ctx.Config.SoundLightingTimerExpiredBlocking
	ctx.configMu.RUnlock()
	playSoundEnabled(DeviceConfig{SoundCardName: soundCard}, expPath, expEn, expBlocking)
}

func (ctx *AppContext) lightingManualButtonPressed() {
	if !lightingManualControlReady(ctx) {
		return
	}
	ctx.lightingEnergizeAndArmTimer("lighting_button")
}
