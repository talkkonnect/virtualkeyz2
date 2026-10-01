package app

import (
	"log"
	"strings"
	"time"
)

func (ctx *AppContext) noteDoorHoldExtraGrace(d time.Duration) {
	if d < 0 {
		d = 0
	}
	ctx.doorAlarmMu.Lock()
	ctx.doorHoldExtraGrace = d
	ctx.doorAlarmMu.Unlock()
}

func (ctx *AppContext) clearDoorHoldExtraGrace() {
	ctx.doorAlarmMu.Lock()
	ctx.doorHoldExtraGrace = 0
	ctx.doorAlarmMu.Unlock()
}

func monitorDoorSensors(ctx *AppContext) {
	if ctx.GPIO == nil {
		log.Println("INFO: Door sensor monitor disabled (GPIO not available).")
		return
	}
	if !ctx.GPIO.DoorSensorConfigured() {
		log.Println("INFO: Door sensor monitor disabled (no door sensor pin configured).")
		return
	}

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	sensorGPIO := int(ctx.GPIOSettings.DoorSensorPin)

	var openSince time.Time
	var wasOpen bool
	first := true

	var inAlarmPhase bool
	var lastAlarmAt time.Time
	var warningCount int
	var doorForcedLatched bool
	var doorSeenCloseAfterForced bool

	for range tick.C {
		ctx.configMu.RLock()
		warnAfter := ctx.Config.DoorOpenWarningAfter
		alarmInterval := ctx.Config.DoorOpenAlarmInterval
		maxCount := ctx.Config.DoorOpenAlarmMaxCount
		forcedAfter := ctx.Config.DoorForcedAfterWarnings
		closedLow := ctx.Config.DoorSensorClosedIsLow
		doorOpenPath := strings.TrimSpace(ctx.Config.SoundDoorOpen)
		doorOpenSoundEn := ctx.Config.SoundDoorOpenEnabled
		doorOpenBlocking := ctx.Config.SoundDoorOpenBlocking
		doorOpenCard := ctx.Config.SoundCardName
		ctx.configMu.RUnlock()
		if warnAfter <= 0 {
			warnAfter = 10 * time.Second
		}
		if alarmInterval <= 0 {
			alarmInterval = 30 * time.Second
		}

		open := ctx.GPIO.DoorIsOpen(closedLow)

		if first {
			first = false
			wasOpen = open
			if open {
				log.Printf("INFO: Door status: OPEN (sensor GPIO %d).", ctx.GPIOSettings.DoorSensorPin)
				openSince = time.Now()
			} else {
				log.Printf("INFO: Door status: CLOSED (sensor GPIO %d).", ctx.GPIOSettings.DoorSensorPin)
				openSince = time.Time{}
			}
			inAlarmPhase = false
			lastAlarmAt = time.Time{}
			warningCount = 0
			doorForcedLatched = false
			doorSeenCloseAfterForced = false
			continue
		} else if open != wasOpen {
			wasOpen = open
			if open {
				log.Printf("INFO: Door is now OPEN (sensor GPIO %d).", ctx.GPIOSettings.DoorSensorPin)
				openSince = time.Now()
				inAlarmPhase = false
				lastAlarmAt = time.Time{}
				warningCount = 0
				if doorSeenCloseAfterForced {
					doorForcedLatched = false
					doorSeenCloseAfterForced = false
				}
				fireEventWebhook(ctx, "door_opened", map[string]any{"door_sensor_gpio": sensorGPIO})
			} else {
				log.Printf("INFO: Door is now CLOSED (sensor GPIO %d).", ctx.GPIOSettings.DoorSensorPin)
				openSince = time.Time{}
				inAlarmPhase = false
				lastAlarmAt = time.Time{}
				warningCount = 0
				if doorForcedLatched {
					doorSeenCloseAfterForced = true
				}
				ctx.clearDoorHoldExtraGrace()
				fireEventWebhook(ctx, "door_closed", map[string]any{"door_sensor_gpio": sensorGPIO})
				lcdShowDoorSecured(ctx)
			}
			continue
		}

		if !open {
			continue
		}
		if openSince.IsZero() {
			openSince = time.Now()
		}
		if ctx.DoorLatched() {
			// Keypad latch (function code 2): the door is meant to stand open; restart the
			// held-open clock so warnings only begin after the latch is released.
			openSince = time.Now()
			inAlarmPhase = false
			warningCount = 0
			continue
		}

		ctx.doorAlarmMu.Lock()
		holdExtra := ctx.doorHoldExtraGrace
		ctx.doorAlarmMu.Unlock()
		effectiveWarn := warnAfter + holdExtra

		if doorForcedLatched && !doorSeenCloseAfterForced {
			continue
		}

		now := time.Now()
		elapsed := now.Sub(openSince)

		if !inAlarmPhase {
			if elapsed < effectiveWarn {
				continue
			}
			inAlarmPhase = true
			warningCount = 1
			lastAlarmAt = now
			log.Printf("WARNING: Door open longer than %v (effective threshold %v; door sensor GPIO %d).", warnAfter, effectiveWarn, ctx.GPIOSettings.DoorSensorPin)
			detail := map[string]any{
				"door_sensor_gpio":         sensorGPIO,
				"threshold":                warnAfter.String(),
				"threshold_effective":      effectiveWarn.String(),
				"door_hold_extra":          holdExtra.String(),
				"warning_sequence":         warningCount,
				"door_open_alarm_interval": alarmInterval.String(),
			}
			fireEventWebhook(ctx, "door_open_timeout", detail)
			playSoundEnabled(DeviceConfig{SoundCardName: doorOpenCard}, doorOpenPath, doorOpenSoundEn, doorOpenBlocking)
			if forcedAfter > 0 && warningCount >= forcedAfter {
				fireEventWebhook(ctx, "door_forced", map[string]any{
					"door_sensor_gpio":      sensorGPIO,
					"warning_sequence":      warningCount,
					"forced_after_warnings": forcedAfter,
				})
				doorForcedLatched = true
				lcdShowDoorForced(ctx)
			} else {
				lcdShowDoorHeld(ctx)
			}
			continue
		}

		if maxCount > 0 && warningCount >= maxCount {
			continue
		}
		if now.Sub(lastAlarmAt) < alarmInterval {
			continue
		}

		warningCount++
		lastAlarmAt = now
		if maxCount > 0 && warningCount > maxCount {
			continue
		}

		log.Printf("WARNING: Door still open (alarm repeat %d; door sensor GPIO %d).", warningCount, ctx.GPIOSettings.DoorSensorPin)
		detail := map[string]any{
			"door_sensor_gpio":         sensorGPIO,
			"threshold":                warnAfter.String(),
			"threshold_effective":      effectiveWarn.String(),
			"door_hold_extra":          holdExtra.String(),
			"warning_sequence":         warningCount,
			"door_open_alarm_interval": alarmInterval.String(),
		}
		fireEventWebhook(ctx, "door_open_timeout", detail)
		playSoundEnabled(DeviceConfig{SoundCardName: doorOpenCard}, doorOpenPath, doorOpenSoundEn, doorOpenBlocking)
		if forcedAfter > 0 && warningCount >= forcedAfter && !doorForcedLatched {
			fireEventWebhook(ctx, "door_forced", map[string]any{
				"door_sensor_gpio":      sensorGPIO,
				"warning_sequence":      warningCount,
				"forced_after_warnings": forcedAfter,
			})
			doorForcedLatched = true
			lcdShowDoorForced(ctx)
		} else {
			lcdShowDoorHeld(ctx)
		}
	}
}
