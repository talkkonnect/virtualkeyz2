package app

import (
	"log"
	"strings"
	"time"
	"unicode"

	evdev "github.com/gvalkov/golang-evdev"
)

// pinDisplayDigitsBuffer is the capacity of PinDisplayDigits; notifyPinDisplay drops updates when full (non-blocking send).
const pinDisplayDigitsBuffer = 12

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}

func pinDigitCount(pin string) int {
	n := 0
	for _, r := range pin {
		if unicode.IsDigit(r) {
			n++
		}
	}
	return n
}

func notifyPinDisplay(ctx *AppContext, pin string) {
	n := pinDigitCount(pin)

	// Local 20x4 LCD: non-blocking push of the masked digit count.
	if ctx.PinDisplayDigits != nil {
		select {
		case ctx.PinDisplayDigits <- n:
		default:
			// Channel full or consumer gone; never block keypad on the display path.
		}
	}

	// Remote framebuffer display (fb-virtualkeyz2): mirror the masked digit count
	// so its PIN-entry screen fills in lockstep with the LCD. Async + allowlist-
	// gated + no audit row, so this never blocks keypad input.
	if n > 0 {
		ctx.configMu.RLock()
		pinLen := ctx.Config.PinLength
		ctx.configMu.RUnlock()
		if pinLen <= 0 {
			pinLen = 6
		}
		postEventWebhook(ctx, "pin_progress", map[string]any{"entered": n, "length": pinLen})
	} else {
		// Buffer emptied — backspace-to-empty, clear, inter-digit/session timeout,
		// or the post-submit reset. keypad_session_end returns the remote display
		// to idle ONLY if it is currently on the PIN screen (the fb client guards
		// this), so it clears a cleared entry but never repaints over an ACCESS
		// GRANTED / DENIED result.
		postEventWebhook(ctx, "keypad_session_end", map[string]any{})
	}
}

// notifyKeypadEntry mirrors a keypad buffer on the displays: only the PIN digits after a
// "<code> " function-code prefix are counted. While the PIN part is still empty after the
// separator nothing is sent, so the displays stay on the PIN screen (a 0 count ends the session).
func notifyKeypadEntry(ctx *AppContext, buf string) {
	pin := keypadEntryPINPart(buf)
	if buf != "" && pin == "" {
		return
	}
	notifyPinDisplay(ctx, pin)
}

// isKeypadCancelKey reports whether code is the keypad's Cancel key. Keypads differ in
// what they send for it; check with evtest.
func isKeypadCancelKey(code uint16) bool {
	switch code {
	case evdev.KEY_ESC, evdev.KEY_DELETE, evdev.KEY_CANCEL:
		return true
	}
	return false
}

// Use evtest in linux to test the capabilities of the keypad.
// keypadRole is "entry", "exit", or "" for single-keypad modes (used in logs, webhooks, and dual-keypad setups).
// keypadKeyChars maps PIN keypad key codes to {character, name used in the DEBUG log}.
var keypadKeyChars = map[uint16][2]string{
	evdev.KEY_KP0:     {"0", "digit 0"},
	evdev.KEY_0:       {"0", "digit 0"},
	evdev.KEY_KP1:     {"1", "digit 1"},
	evdev.KEY_1:       {"1", "digit 1"},
	evdev.KEY_KP2:     {"2", "digit 2"},
	evdev.KEY_2:       {"2", "digit 2"},
	evdev.KEY_KP3:     {"3", "digit 3"},
	evdev.KEY_3:       {"3", "digit 3"},
	evdev.KEY_KP4:     {"4", "digit 4"},
	evdev.KEY_4:       {"4", "digit 4"},
	evdev.KEY_KP5:     {"5", "digit 5"},
	evdev.KEY_5:       {"5", "digit 5"},
	evdev.KEY_KP6:     {"6", "digit 6"},
	evdev.KEY_6:       {"6", "digit 6"},
	evdev.KEY_KP7:     {"7", "digit 7"},
	evdev.KEY_7:       {"7", "digit 7"},
	evdev.KEY_KP8:     {"8", "digit 8"},
	evdev.KEY_8:       {"8", "digit 8"},
	evdev.KEY_KP9:     {"9", "digit 9"},
	evdev.KEY_9:       {"9", "digit 9"},
	evdev.KEY_KPSLASH: {"/", "slash"},
	evdev.KEY_SLASH:   {"/", "slash"},
	evdev.KEY_KPMINUS: {"-", "minus"},
	evdev.KEY_KPPLUS:  {"+", "plus"},
	evdev.KEY_KPDOT:   {".", "dot"},
}

// runKeypadListenerLoop keeps a keypad listener running: it reopens the device after it is
// unplugged or fails to open, logging the open failure once per outage.
func runKeypadListenerLoop(ctx *AppContext, devicePath, keypadRole string) {
	failing := false
	for {
		dev, err := evdev.Open(devicePath)
		if err != nil {
			if !failing {
				log.Printf("CRITICAL: Failed to open USB keypad at %s: %v (retrying every 2s)", devicePath, err)
				failing = true
			}
			time.Sleep(2 * time.Second)
			continue
		}
		if failing {
			log.Printf("INFO: USB keypad at %s is available again.", devicePath)
			failing = false
		}
		runKeypadListener(ctx, dev, devicePath, keypadRole)
		log.Printf("WARNING: Keypad listener stopped for %s; reopening in 2s.", devicePath)
		time.Sleep(2 * time.Second)
	}
}

// runKeypadListener handles one opened keypad until its device returns a read error.
func runKeypadListener(ctx *AppContext, dev *evdev.InputDevice, devicePath, keypadRole string) {
	defer dev.File.Close()

	kpLog := keypadLogTag(keypadRole)
	if keypadRole != "" {
		log.Printf("INFO: Keypad role %q: %s @ %s", keypadRole, dev.Name, devicePath)
	} else {
		log.Printf("INFO: Listening to USB Keypad: %s @ %s", dev.Name, devicePath)
	}

	// Block in a goroutine so the main loop can select on inter-digit/session timers and react immediately.
	// The channel is closed on a read error (e.g. keypad unplugged) so the caller can reopen it.
	eventCh := make(chan *evdev.InputEvent, 64)
	go func() {
		defer close(eventCh)
		for {
			ev, rerr := dev.ReadOne()
			if rerr != nil {
				log.Printf("ERROR: Failed to read from keypad %s: %v", devicePath, rerr)
				return
			}
			if ev != nil {
				eventCh <- ev
			}
		}
	}()

	var pinBuffer string
	interTimer := time.NewTimer(time.Hour)
	interTimer.Stop()
	sessionTimer := time.NewTimer(time.Hour)
	sessionTimer.Stop()

	drainTimer := func(t *time.Timer) {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	}

	stopEntryTimers := func() {
		drainTimer(interTimer)
		drainTimer(sessionTimer)
	}

	restartInterDigit := func() {
		ctx.configMu.RLock()
		d := ctx.Config.KeypadInterDigitTimeout
		ctx.configMu.RUnlock()
		if d <= 0 {
			d = 5 * time.Second
		}
		d = clampDuration(d, 3*time.Second, 10*time.Second)
		drainTimer(interTimer)
		interTimer.Reset(d)
	}

	startSessionFromFirstDigit := func() {
		ctx.configMu.RLock()
		d := ctx.Config.KeypadSessionTimeout
		ctx.configMu.RUnlock()
		if d <= 0 {
			d = 30 * time.Second
		}
		d = clampDuration(d, 10*time.Second, 60*time.Second)
		drainTimer(sessionTimer)
		sessionTimer.Reset(d)
	}

	for {
		select {
		case ev, ok := <-eventCh:
			if !ok {
				stopEntryTimers()
				if pinBuffer != "" {
					notifyPinDisplay(ctx, "")
				}
				return
			}
			if ev.Type == evdev.EV_KEY && ev.Value == 1 {
				if ctx.inputHeld(keypadRole) {
					// Inside the post-result feedback window (pin_entry_feedback_delay): drop the key.
					continue
				}
				restartInterDigit()

				ke := evdev.NewKeyEvent(ev)
				char := ""

				if kc, ok := keypadKeyChars[ke.Scancode]; ok {
					char = kc[0]
					debugf("[%s keypad] %s pressed", kpLog, kc[1])
				}
				switch {
				case ke.Scancode == evdev.KEY_BACKSPACE:
					if len(pinBuffer) > 0 {
						pinBuffer = pinBuffer[:len(pinBuffer)-1]
						if len(pinBuffer) == 0 {
							drainTimer(sessionTimer)
						}
					}
					notifyKeypadEntry(ctx, pinBuffer)
				case ke.Scancode == evdev.KEY_SPACE:
					// Function-code separator: "<code> <PIN>" (keypad_fn.go).
					if keypadSpaceAllowed(pinBuffer) {
						debugf("[%s keypad] space pressed (function code separator)", kpLog)
						pinBuffer += " "
					}
				case isKeypadCancelKey(ke.Scancode):
					if pinBuffer != "" {
						log.Printf("INFO: PIN entry cancelled (%s keypad).", kpLog)
						pinBuffer = ""
						stopEntryTimers()
						notifyPinDisplay(ctx, pinBuffer)
						ctx.configMu.RLock()
						cfg := ctx.Config
						ctx.configMu.RUnlock()
						ctx.playFeedbackSound(keypadRole, cfg, cfg.SoundCancel, cfg.SoundCancelEnabled, cfg.SoundCancelBlocking)
					}
				case ke.Scancode == evdev.KEY_KPENTER || ke.Scancode == evdev.KEY_ENTER:
					if pinBuffer == "" {
						ctx.ringDoorbell(keypadRole)
						continue
					}
					log.Printf("INFO: PIN submission initiated (%s keypad).", kpLog)
					processKeypadEntry(ctx, pinBuffer, keypadRole)
					pinBuffer = ""
					stopEntryTimers()
					notifyPinDisplay(ctx, pinBuffer)
				case ke.Scancode == evdev.KEY_KPASTERISK:
					log.Printf("INFO: 'Call for Help' triggered via USB keypad (%s).", kpLog)
					triggerCallForHelp(ctx)
					pinBuffer = ""
					stopEntryTimers()
					notifyPinDisplay(ctx, pinBuffer)
				}

				if char != "" {
					wasEmpty := len(pinBuffer) == 0
					pinBuffer += char
					if wasEmpty {
						startSessionFromFirstDigit()
					}
					notifyKeypadEntry(ctx, pinBuffer)

					ctx.configMu.RLock()
					pinLen := ctx.Config.PinLength
					ctx.configMu.RUnlock()
					// With a "<code> " prefix only the PIN digits count toward pin_length.
					pinPart := keypadEntryPINPart(pinBuffer)
					if pinLen > 0 && len(pinPart) >= pinLen && isAllDigits(pinPart) {
						log.Printf("INFO: PIN auto-submitted after %d digits (%s keypad).", pinLen, kpLog)
						processKeypadEntry(ctx, pinBuffer, keypadRole)
						pinBuffer = ""
						stopEntryTimers()
						notifyPinDisplay(ctx, pinBuffer)
					}
				}
			}

		case <-interTimer.C:
			if len(pinBuffer) > 0 {
				ctx.configMu.RLock()
				lim := ctx.Config.KeypadInterDigitTimeout
				ctx.configMu.RUnlock()
				log.Printf("WARNING: Keypad inter-digit timeout (%s) expired; PIN buffer cleared (%s keypad).", lim, kpLog)
				pinBuffer = ""
				stopEntryTimers()
				notifyPinDisplay(ctx, pinBuffer)
			}

		case <-sessionTimer.C:
			if len(pinBuffer) > 0 {
				ctx.configMu.RLock()
				lim := ctx.Config.KeypadSessionTimeout
				ctx.configMu.RUnlock()
				log.Printf("WARNING: Keypad session timeout (%s) expired; PIN buffer cleared (%s keypad).", lim, kpLog)
				pinBuffer = ""
				stopEntryTimers()
				notifyPinDisplay(ctx, pinBuffer)
			}
		}
	}
}

func startKeypadListeners(ctx *AppContext) {
	ctx.configMu.RLock()
	mode := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
	p1 := strings.TrimSpace(ctx.Config.KeypadEvdevPath)
	p2 := strings.TrimSpace(ctx.Config.KeypadExitEvdevPath)
	ctx.configMu.RUnlock()
	if p1 == "" {
		p1 = "/dev/input/event1"
	}
	if isDualUSBKeypadMode(mode) {
		if p2 == "" || p2 == p1 {
			log.Printf("CRITICAL: access_dual_usb_keypad requires keypad_exit_evdev_path distinct from keypad_evdev_path; using single listener on %q", p1)
			runKeypadListenerLoop(ctx, p1, "")
			return
		}
		go runKeypadListenerLoop(ctx, p1, "entry")
		runKeypadListenerLoop(ctx, p2, "exit")
		return
	}
	runKeypadListenerLoop(ctx, p1, "")
}
