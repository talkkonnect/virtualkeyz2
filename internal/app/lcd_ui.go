package app

import (
	"log"
	"strings"
	"sync"
	"time"

	hd44780 "github.com/d2r2/go-hd44780"
	i2c "github.com/d2r2/go-i2c"
	d2logger "github.com/d2r2/go-logger"
	logging "github.com/op/go-logging"
)

func init() {
	// go-i2c defaults its package logger to Debug; keep I2C chatter off until JSON sets i2c_debug_enabled.
	syncLCDTransportLibraryDebug(false)
}

// syncLCDTransportLibraryDebug sets verbosity for d2r2/go-i2c (go-logger) and d2r2/go-hd44780 (op/go-logging).
// Independent of VirtualKeyz2 log_level / shouldEmitLogLine.
func syncLCDTransportLibraryDebug(enabled bool) {
	if enabled {
		logging.SetLevel(logging.DEBUG, "hd44780")
		_ = d2logger.ChangePackageLogLevel("i2c", d2logger.DebugLevel)
	} else {
		logging.SetLevel(logging.INFO, "hd44780")
		_ = d2logger.ChangePackageLogLevel("i2c", d2logger.InfoLevel)
	}
}

// HD44780 20x4 over I2C (PCF8574 backpack). All hardware access and formatting run only in
// displayController so keypad, door polling, and MQTT handlers stay non-blocking (they only
// enqueue lcdCmd / PinDisplayDigits with bounded, non-blocking sends).

const (
	lcdCols           = 20
	lcdCmdBuffer      = 32
	lcdAutoIdleAfter  = 4 * time.Second
	lcdMinBacklight   = 5 * time.Second
	lcdMaxBacklight   = 60 * time.Minute
)

// lcdCmd is a full-screen update for the display goroutine (PIN masking uses PinDisplayDigits only).
type lcdCmd struct {
	lines             [4]string
	returnToIdleAfter time.Duration // if >0, show idle after this duration (unless user starts PIN entry)
	// renderDone: if non-nil, send one value after this frame is applied (or skipped); used so access sounds run after the physical LCD update.
	renderDone chan struct{}
}

func lcdFitRow(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) >= lcdCols {
		return string(r[:lcdCols-1]) + "~"
	}
	return string(r) + strings.Repeat(" ", lcdCols-len(r))
}

func lcdCenterRow(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) >= lcdCols {
		return string(r[:lcdCols-1]) + "~"
	}
	pad := lcdCols - len(r)
	left := pad / 2
	return strings.Repeat(" ", left) + string(r) + strings.Repeat(" ", pad-left)
}

func lcdPinMaskRow(n int) string {
	if n < 0 {
		n = 0
	}
	stars := strings.Repeat("*", n)
	s := "Enter PIN: " + stars
	return lcdFitRow(s)
}

func lcdLinesIdle() [4]string {
	return [4]string{
		lcdCenterRow("Welcome to MeSpace"),
		lcdCenterRow("Scan Phone or PIN"),
		lcdCenterRow("to Open Door"),
		strings.Repeat(" ", lcdCols),
		}
}

func lcdLinesProcessing() [4]string {
	return [4]string{
		lcdCenterRow("Checking..."),
		lcdCenterRow("Please Wait..."),
		strings.Repeat(" ", lcdCols),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesGranted(welcome string) [4]string {
	welcome = strings.TrimSpace(welcome)
	if welcome == "" {
		welcome = "To MeSpace"
	}
	return [4]string{
		lcdCenterRow("Access Granted"),
		lcdFitRow("Welcome, " + welcome),
		lcdCenterRow("Door Unlocked"),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesDenied(line2, line3, line4 string) [4]string {
	return [4]string{
		lcdCenterRow("Access Denied"),
		lcdFitRow(line2),
		lcdFitRow(line3),
		lcdFitRow(line4),
	}
}

func lcdLinesDoorHeld() [4]string {
	return [4]string{
		lcdCenterRow("Door Held Open"),
		lcdCenterRow("Please Close Door"),
		strings.Repeat(" ", lcdCols),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesDoorForced() [4]string {
	return [4]string{
		lcdCenterRow("!!! ALERT !!!"),
		lcdCenterRow("Forced Open"),
		lcdFitRow("Secure Door Now"),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesDoorSecured() [4]string {
	return [4]string{
		lcdCenterRow("Door Secured"),
		lcdCenterRow("Ready"),
		strings.Repeat(" ", lcdCols),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesMQTTOffline() [4]string {
	return [4]string{
		lcdCenterRow("Error: Comm Fail"),
		lcdCenterRow("Offline Mode"),
		lcdFitRow("MQTT Disconnected"),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesTamper() [4]string {
	return [4]string{
		lcdCenterRow("!!! ALERT !!!"),
		lcdCenterRow("Tamper Detected"),
		lcdFitRow("Check Enclosure"),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesFiremans(active bool) [4]string {
	if active {
		return [4]string{
			lcdCenterRow("Emergency Bypass"),
			lcdCenterRow("Fireman's Service"),
			lcdFitRow("Access Rules Relaxed"),
			strings.Repeat(" ", lcdCols),
		}
	}
	return [4]string{
		lcdCenterRow("Fireman's Service"),
		lcdCenterRow("Ended"),
		lcdCenterRow("Normal Operation"),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesFireAlarm(active bool) [4]string {
	if active {
		return [4]string{
			lcdCenterRow("!!! FIRE ALARM !!!"),
			lcdCenterRow("Fail-Unlock Active"),
			lcdFitRow("Door Held Open"),
			strings.Repeat(" ", lcdCols),
		}
	}
	return lcdLinesDoorSecured()
}

func lcdLinesConfigReload() [4]string {
	return [4]string{
		lcdCenterRow("System Updating"),
		lcdCenterRow("Please Wait..."),
		strings.Repeat(" ", lcdCols),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesCommFail() [4]string {
	return [4]string{
		lcdCenterRow("Error: Comm Fail"),
		lcdCenterRow("Door Relay Fault"),
		lcdFitRow("Try Again / Service"),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesWrongPassword() [4]string {
	return [4]string{
		lcdCenterRow("Wrong Password"),
		lcdCenterRow("Try Again"),
		strings.Repeat(" ", lcdCols),
		strings.Repeat(" ", lcdCols),
	}
}

func lcdLinesCardPINPrompt() [4]string {
	return [4]string{
		lcdCenterRow("Then Enter PIN"),
		strings.Repeat(" ", lcdCols),
		strings.Repeat(" ", lcdCols),
	}
}

// lcdEnqueueFull enqueues a full-screen update. Drops when the buffer is full so callers never block.
func lcdEnqueueFull(ctx *AppContext, lines [4]string, returnToIdleAfter time.Duration) {
	if ctx == nil || ctx.lcdUI == nil {
		return
	}
	select {
	case ctx.lcdUI <- lcdCmd{lines: lines, returnToIdleAfter: returnToIdleAfter}:
	default:
	}
}

// lcdEnqueueFullSync enqueues one frame and blocks until the display goroutine finishes drawing it
// (or skips it), or until lcdSyncRenderTimeout. Use before access granted/denied sounds.
func lcdEnqueueFullSync(ctx *AppContext, lines [4]string, returnToIdleAfter time.Duration) {
	if ctx == nil || ctx.lcdUI == nil {
		return
	}
	done := make(chan struct{}, 1)
	select {
	case ctx.lcdUI <- lcdCmd{lines: lines, returnToIdleAfter: returnToIdleAfter, renderDone: done}:
		select {
		case <-done:
		case <-time.After(lcdSyncRenderTimeout):
		}
	default:
	}
}

const lcdSyncRenderTimeout = 3 * time.Second

func lcdShowIdle(ctx *AppContext) {
	lcdEnqueueFull(ctx, lcdLinesIdle(), 0)
}

func lcdShowProcessing(ctx *AppContext) {
	lcdEnqueueFull(ctx, lcdLinesProcessing(), 0)
}

func lcdShowGranted(ctx *AppContext, credentialLabel string) {
	lcdEnqueueFullSync(ctx, lcdLinesGranted(credentialLabel), lcdAutoIdleAfter)
}

func lcdShowDeniedReason(ctx *AppContext, mid, bottom string) {
	lcdEnqueueFull(ctx, lcdLinesDenied(mid, bottom, ""), lcdAutoIdleAfter)
}

func lcdShowScheduleDeny(ctx *AppContext) {
	lcdEnqueueFull(ctx, lcdLinesDenied("Outside Schedule", "", ""), lcdAutoIdleAfter)
}

func lcdShowInvalidCard(ctx *AppContext) {
	lcdEnqueueFullSync(ctx, lcdLinesDenied("Invalid Card", "", ""), lcdAutoIdleAfter)
}

func lcdShowNoPermission(ctx *AppContext) {
	lcdEnqueueFull(ctx, lcdLinesDenied("No Permission", "", ""), lcdAutoIdleAfter)
}

func lcdShowKeypadLockout(ctx *AppContext) {
	lcdEnqueueFullSync(ctx, lcdLinesDenied("Keypad Locked", "Try Later", ""), lcdAutoIdleAfter)
}

func lcdShowWrongPIN(ctx *AppContext) {
	lcdEnqueueFull(ctx, lcdLinesWrongPassword(), lcdAutoIdleAfter)
}

func lcdShowElevatorFloorDeny(ctx *AppContext) {
	lcdEnqueueFullSync(ctx, lcdLinesDenied("Elevator", "Access Denied", ""), lcdAutoIdleAfter)
}

func lcdShowCommFail(ctx *AppContext) {
	lcdEnqueueFullSync(ctx, lcdLinesCommFail(), lcdAutoIdleAfter)
}

func lcdShowDoorHeld(ctx *AppContext) {
	if ctx == nil {
		return
	}
	// After this duration, return to the normal welcome/idle screen (lcdLinesIdle — e.g. "Welcome to MeSpace").
	// Uses device.door_open_alarm_interval when set; repeats of door_open_timeout refresh this timer.
	ctx.configMu.RLock()
	d := ctx.Config.DoorOpenAlarmInterval
	ctx.configMu.RUnlock()
	if d <= 0 {
		d = 30 * time.Second
	}
	if d < 15*time.Second {
		d = 15 * time.Second
	}
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	lcdEnqueueFull(ctx, lcdLinesDoorHeld(), d)
}

func lcdShowDoorForced(ctx *AppContext) {
	lcdEnqueueFull(ctx, lcdLinesDoorForced(), 0)
}

func lcdShowDoorSecured(ctx *AppContext) {
	lcdEnqueueFull(ctx, lcdLinesDoorSecured(), lcdAutoIdleAfter)
}

func lcdShowMQTTOffline(ctx *AppContext) {
	lcdEnqueueFull(ctx, lcdLinesMQTTOffline(), 0)
}

func lcdShowMQTTRecovered(ctx *AppContext) {
	lcdShowIdle(ctx)
}

func lcdShowTamperAlert(ctx *AppContext) {
	lcdEnqueueFull(ctx, lcdLinesTamper(), 0)
}

func lcdShowFiremans(ctx *AppContext, active bool) {
	lcdEnqueueFull(ctx, lcdLinesFiremans(active), lcdAutoIdleAfter)
}

func lcdShowFireAlarm(ctx *AppContext, active bool) {
	lcdEnqueueFull(ctx, lcdLinesFireAlarm(active), 0)
}

func lcdShowConfigReload(ctx *AppContext) {
	lcdEnqueueFull(ctx, lcdLinesConfigReload(), 2*time.Second)
}

func lcdRejectFromWebhookReason(ctx *AppContext, reason, keypadRole string) {
	// Sync so deny WAV plays only after the LCD has been updated.
	switch reason {
	case "access_schedule":
		lcdEnqueueFullSync(ctx, lcdLinesDenied("Outside Schedule", "", ""), lcdAutoIdleAfter)
	case "keypad_lockout":
		lcdShowKeypadLockout(ctx)
	case "exit_without_recorded_entry":
		lcdEnqueueFullSync(ctx, lcdLinesDenied("No Permission", "", ""), lcdAutoIdleAfter)
	case "invalid_pin":
		lcdEnqueueFullSync(ctx, lcdLinesWrongPassword(), lcdAutoIdleAfter)
	case "credential_lifecycle", "qr_unknown_device_uuid", "qr_parse_failed",
		"qr_static_test_mismatch", "qr_static_test_not_configured", "qr_timestamp_outside_window":
		lcdShowInvalidCard(ctx)
	default:
		if strings.HasPrefix(reason, "qr_") {
			lcdShowInvalidCard(ctx)
			return
		}
		lcdEnqueueFullSync(ctx, lcdLinesDenied("See Logs", reason, ""), lcdAutoIdleAfter)
	}
}

func normalizeLCDDisplaySettings(lc *LCDDisplaySettings) {
	if lc.I2CBus <= 0 {
		lc.I2CBus = 1
	}
	if lc.I2CAddr == 0 {
		lc.I2CAddr = 0x27
	}
	if lc.BacklightTimeout < 0 {
		lc.BacklightTimeout = 0
	} else if lc.BacklightTimeout > 0 && lc.BacklightTimeout < lcdMinBacklight {
		lc.BacklightTimeout = lcdMinBacklight
	} else if lc.BacklightTimeout > lcdMaxBacklight {
		lc.BacklightTimeout = lcdMaxBacklight
	}
	syncLCDTransportLibraryDebug(lc.I2CDebugEnabled)
}

// displayController owns the I2C HD44780 device: one goroutine performs Open/Clear/ShowMessage so
// no other code waits on the bus. PinDisplayDigits and lcdUI are merged here with a select.
func displayController(ctx *AppContext) {
	var dev *hd44780.Lcd
	var bus *i2c.I2C
	var busMu sync.Mutex
	pinEntryMode := false
	var lastPinLines [4]string

	drainTimer := func(t *time.Timer) {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	}

	blTimer := time.NewTimer(time.Hour)
	drainTimer(blTimer)
	idleTimer := time.NewTimer(time.Hour)
	drainTimer(idleTimer)

	closeHW := func() {
		busMu.Lock()
		defer busMu.Unlock()
		if dev != nil {
			_ = dev.BacklightOff()
			dev = nil
		}
		if bus != nil {
			_ = bus.Close()
			bus = nil
		}
	}

	// openLCDUnsafe opens the I2C LCD; caller must hold busMu and must ensure dev == nil.
	openLCDUnsafe := func(busN int, addr uint8) error {
		ic, err := i2c.NewI2C(addr, busN)
		if err != nil {
			return err
		}
		lcd, err := hd44780.NewLcd(ic, hd44780.LCD_20x4)
		if err != nil {
			_ = ic.Close()
			return err
		}
		bus = ic
		dev = lcd
		_ = dev.BacklightOn()
		log.Printf("INFO: LCD HD44780 20x4 opened on /dev/i2c-%d address 0x%02x.", busN, addr)
		return nil
	}

	renderFull := func(lines [4]string) error {
		busMu.Lock()
		defer busMu.Unlock()
		if dev == nil {
			return nil
		}
		if err := dev.Clear(); err != nil {
			return err
		}
		for row := 0; row < 4; row++ {
			if err := dev.SetPosition(row, 0); err != nil {
				return err
			}
			if _, err := dev.Write([]byte(lcdFitRow(lines[row]))); err != nil {
				return err
			}
		}
		return nil
	}

	renderPin := func(lines [4]string) error {
		busMu.Lock()
		defer busMu.Unlock()
		if dev == nil {
			return nil
		}
		if err := dev.SetPosition(2, 0); err != nil {
			return err
		}
		if _, err := dev.Write([]byte(lcdFitRow(lines[2]))); err != nil {
			return err
		}
		return nil
	}

	ensureLCD := func() bool {
		ctx.configMu.RLock()
		cfg := ctx.Config.LCDDisplay
		ctx.configMu.RUnlock()
		if !cfg.Enabled {
			closeHW()
			return false
		}
		busMu.Lock()
		defer busMu.Unlock()
		if dev != nil {
			return true
		}
		if err := openLCDUnsafe(cfg.I2CBus, cfg.I2CAddr); err != nil {
			log.Printf("WARNING: LCD open failed (bus=%d addr=0x%02x): %v — display disabled until next message.", cfg.I2CBus, cfg.I2CAddr, err)
			if bus != nil {
				_ = bus.Close()
				bus = nil
			}
			dev = nil
			return false
		}
		return true
	}

	backlightArm := func() {
		ctx.configMu.RLock()
		d := ctx.Config.LCDDisplay.BacklightTimeout
		ctx.configMu.RUnlock()
		drainTimer(blTimer)
		if d <= 0 {
			busMu.Lock()
			if dev != nil {
				_ = dev.BacklightOn()
			}
			busMu.Unlock()
			return
		}
		busMu.Lock()
		if dev != nil {
			_ = dev.BacklightOn()
		}
		busMu.Unlock()
		blTimer.Reset(d)
	}

	backlightMaybeOff := func() {
		ctx.configMu.RLock()
		d := ctx.Config.LCDDisplay.BacklightTimeout
		en := ctx.Config.LCDDisplay.Enabled
		ctx.configMu.RUnlock()
		if !en || d <= 0 {
			return
		}
		busMu.Lock()
		if dev != nil {
			_ = dev.BacklightOff()
		}
		busMu.Unlock()
	}

	armIdleReturn := func(d time.Duration) {
		drainTimer(idleTimer)
		if d <= 0 {
			return
		}
		idleTimer.Reset(d)
	}

	for {
		ctx.configMu.RLock()
		enabled := ctx.Config.LCDDisplay.Enabled
		ctx.configMu.RUnlock()
		if !enabled {
			closeHW()
		}

		select {
		case cmd := <-ctx.lcdUI:
			signalRender := func() {
				if cmd.renderDone != nil {
					select {
					case cmd.renderDone <- struct{}{}:
					default:
					}
				}
			}
			if !enabled {
				signalRender()
				continue
			}
			if !ensureLCD() {
				signalRender()
				continue
			}
			backlightArm()
			pinEntryMode = false
			lastPinLines = cmd.lines
			if err := renderFull(cmd.lines); err != nil {
				log.Printf("WARNING: LCD render: %v", err)
				closeHW()
				signalRender()
				continue
			}
			armIdleReturn(cmd.returnToIdleAfter)
			signalRender()

		case n := <-ctx.PinDisplayDigits:
			if !enabled {
				continue
			}
			if !ensureLCD() {
				continue
			}
			backlightArm()
			if n <= 0 {
				pinEntryMode = false
				drainTimer(idleTimer)
				if err := renderFull(lcdLinesIdle()); err != nil {
					log.Printf("WARNING: LCD render: %v", err)
					closeHW()
				}
				continue
			}
			drainTimer(idleTimer)
			if !pinEntryMode {
				pinEntryMode = true
				lastPinLines = [4]string{
					lcdCenterRow("PIN Mode"),
					strings.Repeat(" ", lcdCols),
					lcdPinMaskRow(n),
					strings.Repeat(" ", lcdCols),
				}
				if err := renderFull(lastPinLines); err != nil {
					log.Printf("WARNING: LCD render: %v", err)
					closeHW()
				}
			} else {
				lastPinLines[2] = lcdPinMaskRow(n)
				if err := renderPin(lastPinLines); err != nil {
					log.Printf("WARNING: LCD render: %v", err)
					closeHW()
				}
			}

		case <-idleTimer.C:
			if !enabled {
				continue
			}
			if !ensureLCD() {
				continue
			}
			backlightArm()
			pinEntryMode = false
			if err := renderFull(lcdLinesIdle()); err != nil {
				log.Printf("WARNING: LCD render: %v", err)
				closeHW()
			}

		case <-blTimer.C:
			backlightMaybeOff()
		}
	}
}
