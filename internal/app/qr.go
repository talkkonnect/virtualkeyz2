package app

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"strings"
	"time"
	"unicode"

	evdev "github.com/gvalkov/golang-evdev"
)

// evdevASCII maps US-layout evdev key codes to {unshifted, shifted} ASCII for QR/HID scanners.
var evdevASCII = map[uint16][2]string{
	evdev.KEY_A: {"a", "A"},
	evdev.KEY_B: {"b", "B"},
	evdev.KEY_C: {"c", "C"},
	evdev.KEY_D: {"d", "D"},
	evdev.KEY_E: {"e", "E"},
	evdev.KEY_F: {"f", "F"},
	evdev.KEY_G: {"g", "G"},
	evdev.KEY_H: {"h", "H"},
	evdev.KEY_I: {"i", "I"},
	evdev.KEY_J: {"j", "J"},
	evdev.KEY_K: {"k", "K"},
	evdev.KEY_L: {"l", "L"},
	evdev.KEY_M: {"m", "M"},
	evdev.KEY_N: {"n", "N"},
	evdev.KEY_O: {"o", "O"},
	evdev.KEY_P: {"p", "P"},
	evdev.KEY_Q: {"q", "Q"},
	evdev.KEY_R: {"r", "R"},
	evdev.KEY_S: {"s", "S"},
	evdev.KEY_T: {"t", "T"},
	evdev.KEY_U: {"u", "U"},
	evdev.KEY_V: {"v", "V"},
	evdev.KEY_W: {"w", "W"},
	evdev.KEY_X: {"x", "X"},
	evdev.KEY_Y: {"y", "Y"},
	evdev.KEY_Z: {"z", "Z"},
	evdev.KEY_1: {"1", "!"}, evdev.KEY_2: {"2", "@"}, evdev.KEY_3: {"3", "#"}, evdev.KEY_4: {"4", "$"},
	evdev.KEY_5: {"5", "%"}, evdev.KEY_6: {"6", "^"}, evdev.KEY_7: {"7", "&"}, evdev.KEY_8: {"8", "*"},
	evdev.KEY_9: {"9", "("}, evdev.KEY_0: {"0", ")"},
	evdev.KEY_MINUS: {"-", "_"}, evdev.KEY_EQUAL: {"=", "+"},
	evdev.KEY_LEFTBRACE: {"[", "{"}, evdev.KEY_RIGHTBRACE: {"]", "}"},
	evdev.KEY_SEMICOLON: {";", ":"}, evdev.KEY_APOSTROPHE: {"'", "\""}, evdev.KEY_GRAVE: {"`", "~"},
	evdev.KEY_BACKSLASH: {"\\", "|"}, evdev.KEY_COMMA: {",", "<"}, evdev.KEY_DOT: {".", ">"},
	evdev.KEY_SLASH: {"/", "?"}, evdev.KEY_SPACE: {" ", " "},
	// Keypad keys ignore shift.
	evdev.KEY_KP0: {"0", "0"}, evdev.KEY_KP1: {"1", "1"}, evdev.KEY_KP2: {"2", "2"}, evdev.KEY_KP3: {"3", "3"},
	evdev.KEY_KP4: {"4", "4"}, evdev.KEY_KP5: {"5", "5"}, evdev.KEY_KP6: {"6", "6"}, evdev.KEY_KP7: {"7", "7"},
	evdev.KEY_KP8: {"8", "8"}, evdev.KEY_KP9: {"9", "9"},
	evdev.KEY_KPDOT: {".", "."}, evdev.KEY_KPPLUS: {"+", "+"}, evdev.KEY_KPMINUS: {"-", "-"},
	evdev.KEY_KPSLASH: {"/", "/"}, evdev.KEY_KPASTERISK: {"*", "*"},
}

func evdevKeyToASCII(scancode uint16, shift bool) (string, bool) {
	pair, ok := evdevASCII[scancode]
	if !ok {
		return "", false
	}
	if shift {
		return pair[1], true
	}
	return pair[0], true
}

// qrPayloadQueue bounds scans waiting for processing; extra scans during a slow grant are dropped
// rather than stalling the evdev reader (which would make the kernel drop events mid-scan).
const qrPayloadQueue = 4

// runQRScannerListener reads one opened scanner until a read error. Decoding runs on a reader
// goroutine so the kernel buffer is always drained; payloads are processed here, in order.
func runQRScannerListener(ctx *AppContext, dev *evdev.InputDevice, devicePath string) {
	defer dev.File.Close()
	payloads := make(chan string, qrPayloadQueue)
	go readQRScannerPayloads(dev, devicePath, payloads)
	for payload := range payloads {
		ctx.processScannedQRCode(payload)
	}
}

// readQRScannerPayloads grabs the scanner, decodes key events into Enter-terminated payloads and
// sends them on out. It closes out when the device returns a read error (e.g. unplugged).
func readQRScannerPayloads(dev *evdev.InputDevice, devicePath string, out chan<- string) {
	defer close(out)
	// Grab() issues EVIOCGRAB so this HID device is consumed exclusively by this process.
	// That prevents QR keystrokes from leaking into local shells/TTY as normal keyboard input.
	if err := dev.Grab(); err != nil {
		log.Printf("WARNING: QR scanner open but EVIOCGRAB failed on %s: %v", devicePath, err)
	} else {
		defer func() { _ = dev.Release() }()
		log.Printf("INFO: QR scanner EVIOCGRAB active: %s @ %s", dev.Name, devicePath)
	}
	// While grabbed, the kernel keyboard handler never sees this device's Caps Lock, so the Caps Lock LED is
	// never updated. Scanners with Caps Lock detection toggle Caps Lock and wait for the LED to change before
	// sending the payload; without LED feedback they repeat Caps Lock forever. Mirror the LED ourselves via a
	// write fd (evdev writes inject into the same handle, so the grab does not block them).
	ledW, lerr := os.OpenFile(devicePath, os.O_WRONLY, 0)
	if lerr != nil {
		log.Printf("WARNING: QR scanner LED write open failed on %s: %v (scanners with Caps Lock detection may not send data)", devicePath, lerr)
	} else {
		defer ledW.Close()
	}
	capsOn := false
	var buf strings.Builder
	buf.Grow(256)
	shiftPressed := false
	for {
		ev, rerr := dev.ReadOne()
		if rerr != nil {
			log.Printf("ERROR: QR scanner read error (%s): %v", devicePath, rerr)
			return
		}
		if ev == nil || ev.Type != evdev.EV_KEY {
			continue
		}
		ke := evdev.NewKeyEvent(ev)
		switch ke.Scancode {
		case evdev.KEY_LEFTSHIFT, evdev.KEY_RIGHTSHIFT:
			if ev.Value == 0 {
				shiftPressed = false
			} else if ev.Value == 1 || ev.Value == 2 {
				shiftPressed = true
			}
			continue
		}
		if ev.Value != 1 {
			continue
		}
		switch ke.Scancode {
		case evdev.KEY_CAPSLOCK:
			capsOn = !capsOn
			if ledW != nil {
				if err := writeEvdevLED(ledW, evdev.LED_CAPSL, capsOn); err != nil {
					debugf("QR scanner Caps Lock LED write failed: %v", err)
				}
			}
			continue
		case evdev.KEY_ENTER, evdev.KEY_KPENTER:
			payload := strings.TrimSpace(buf.String())
			buf.Reset()
			if payload != "" {
				select {
				case out <- payload:
				default:
					log.Printf("WARNING: QR scan dropped (previous scans still being processed).")
				}
			}
			continue
		case evdev.KEY_BACKSPACE:
			s := buf.String()
			if len(s) > 0 {
				buf.Reset()
				buf.WriteString(s[:len(s)-1])
			}
			continue
		case evdev.KEY_ESC:
			buf.Reset()
			continue
		}
		if ch, ok := evdevKeyToASCII(ke.Scancode, shiftPressed); ok {
			if capsOn && len(ch) == 1 && unicode.IsLetter(rune(ch[0])) {
				// Caps Lock inverts letter case only (shift still applies to digits/symbols).
				if r := rune(ch[0]); unicode.IsUpper(r) {
					ch = string(unicode.ToLower(r))
				} else {
					ch = string(unicode.ToUpper(r))
				}
			}
			_, _ = buf.WriteString(ch)
			if buf.Len() > 2048 {
				log.Printf("WARNING: QR payload exceeded 2048 chars; resetting scanner buffer.")
				buf.Reset()
			}
		}
	}
}

// writeEvdevLED sets an LED (e.g. evdev.LED_CAPSL) on an input device opened for writing.
func writeEvdevLED(f *os.File, led uint16, on bool) error {
	var val int32
	if on {
		val = 1
	}
	evs := []evdev.InputEvent{
		{Type: evdev.EV_LED, Code: led, Value: val},
		{Type: evdev.EV_SYN, Code: evdev.SYN_REPORT},
	}
	return binary.Write(f, binary.NativeEndian, evs)
}

func startQRScannerListener(ctx *AppContext) {
	failing := false
	for {
		ctx.configMu.RLock()
		enabled := ctx.Config.ScannerEnabled
		path := strings.TrimSpace(ctx.Config.ScannerDevicePath)
		ctx.configMu.RUnlock()
		if !enabled {
			log.Printf("INFO: QR scanner disabled via config (scanner_enabled=false); listener stopping.")
			return
		}
		if path == "" {
			path = "/dev/input/by-id/usb-YUREN_Yuren_HID_FS_Keyboard_SN_20190000-event-kbd"
		}
		dev, err := evdev.Open(path)
		if err != nil {
			if !failing {
				log.Printf("WARNING: Failed to open QR scanner at %s: %v (retrying every 2s)", path, err)
				failing = true
			}
			time.Sleep(2 * time.Second)
			continue
		}
		if failing {
			log.Printf("INFO: QR scanner at %s is available again.", path)
			failing = false
		}
		runQRScannerListener(ctx, dev, path)
		log.Printf("WARNING: QR scanner listener stopped for %s; reopening in 2s.", path)
		time.Sleep(2 * time.Second)
	}
}

type scannedQRCodePayload struct {
	// device_uuid identifies the phone/device pre-registered for a PIN.
	DeviceUUID string `json:"device_uuid"`
	// datetime_stamp is RFC3339/RFC3339Nano timestamp generated by the app.
	DatetimeStamp string `json:"datetime_stamp"`
	// timestamp_unix is optional (seconds since Unix epoch) fallback for app variants.
	TimestampUnix int64 `json:"timestamp_unix,omitempty"`
}

// parseScannedQRCodePayload accepts JSON payloads like:
// {"device_uuid":"<uuid>","datetime_stamp":"2026-04-26T03:12:45Z"}
// and fallback text "<device_uuid>|<RFC3339 timestamp>".
func parseScannedQRCodePayload(raw string) (deviceUUID string, stamp time.Time, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", time.Time{}, errors.New("empty QR payload")
	}
	var p scannedQRCodePayload
	if jerr := json.Unmarshal([]byte(raw), &p); jerr == nil {
		deviceUUID = normalizeDeviceUUID(p.DeviceUUID)
		if deviceUUID == "" {
			return "", time.Time{}, errors.New("missing device_uuid")
		}
		ts := strings.TrimSpace(p.DatetimeStamp)
		if ts != "" {
			t, terr := time.Parse(time.RFC3339Nano, ts)
			if terr != nil {
				return "", time.Time{}, fmt.Errorf("invalid datetime_stamp: %w", terr)
			}
			return deviceUUID, t, nil
		}
		if p.TimestampUnix > 0 {
			return deviceUUID, time.Unix(p.TimestampUnix, 0).UTC(), nil
		}
		return "", time.Time{}, errors.New("missing datetime_stamp")
	}
	parts := strings.SplitN(raw, "|", 2)
	if len(parts) != 2 {
		return "", time.Time{}, errors.New("unsupported QR payload format")
	}
	deviceUUID = normalizeDeviceUUID(parts[0])
	if deviceUUID == "" {
		return "", time.Time{}, errors.New("missing device_uuid")
	}
	t, terr := time.Parse(time.RFC3339Nano, strings.TrimSpace(parts[1]))
	if terr != nil {
		return "", time.Time{}, fmt.Errorf("invalid datetime_stamp: %w", terr)
	}
	return deviceUUID, t, nil
}

// qrRejectRepeatQuietPeriod: a rejected QR payload read again before this much time has passed since its
// previous read is treated as the same scan (continuous-mode scanners re-read a code held in view) and gets no
// reject feedback. Once the scanner has not seen it for this long, the next read is a new scan.
const qrRejectRepeatQuietPeriod = 3 * time.Second

// qrSuppressRepeatReject reports whether raw repeats the last rejected payload without a quiet gap,
// refreshing the last-seen time so a code held in view stays suppressed.
func (ctx *AppContext) qrSuppressRepeatReject(raw string) bool {
	ctx.qrRejectMu.Lock()
	defer ctx.qrRejectMu.Unlock()
	if raw != ctx.qrRejectPayload {
		return false
	}
	now := time.Now()
	repeat := now.Sub(ctx.qrRejectLastSeen) < qrRejectRepeatQuietPeriod
	ctx.qrRejectLastSeen = now
	return repeat
}

func (ctx *AppContext) qrRememberReject(raw string) {
	ctx.qrRejectMu.Lock()
	ctx.qrRejectPayload = raw
	ctx.qrRejectLastSeen = time.Now()
	ctx.qrRejectMu.Unlock()
}

func (ctx *AppContext) qrClearReject() {
	ctx.qrRejectMu.Lock()
	ctx.qrRejectPayload = ""
	ctx.qrRejectLastSeen = time.Time{}
	ctx.qrRejectMu.Unlock()
}

func (ctx *AppContext) qrRejectWithAccessDenied(cfg DeviceConfig, raw, reason string, extra map[string]any) {
	ctx.qrRememberReject(raw)
	ctx.configMu.RLock()
	buzzerBCM := ctx.GPIOSettings.BuzzerRelayPin
	ctx.configMu.RUnlock()
	wh := map[string]any{"auth_method": "qr"}
	maps.Copy(wh, extra)
	ctx.pinRejectWithStreak(cfg, "qr", buzzerBCM, reason, wh)
}

// grantStaticTestQRAccess performs the same door/elevator side effects as a successful credential without
// consulting access_pins, schedules, or keypad lockout. Fireman's service still suppresses relays per existing paths.
func (ctx *AppContext) grantStaticTestQRAccess(cfg DeviceConfig) {
	ctx.configMu.RLock()
	doorBCM := ctx.GPIOSettings.DoorRelayPin
	ctx.configMu.RUnlock()
	feedbackDelay := cfg.PinEntryFeedbackDelay
	mode := NormalizeKeypadOperationMode(cfg.KeypadOperationMode)
	keypadRole := "qr"
	credLabel := "static_test_qr"
	kTag := keypadLogTag(keypadRole)
	credTag := credLabel
	whStatic := map[string]any{"auth_method": "qr", "static_test_qr": true}

	ctx.ResetWrongPINCount()
	ctx.keypadClearLockout()

	if ctx.grantElevatorMode(cfg, mode, doorBCM, feedbackDelay, grantRequest{
		pin: credLabel, keypadRole: keypadRole, credLabel: credLabel, staticTest: true, whExtra: whStatic,
	}) {
		return
	}
	log.Printf("INFO: Static test QR accepted (mode=%s %s; credential=%s); door relay GPIO %d.", mode, kTag, credTag, doorBCM)
	ctx.grantDefaultModeDoorUnlockLikePIN("", cfg, mode, keypadRole, credLabel, doorBCM, feedbackDelay, 0, whStatic, false)
}

func (ctx *AppContext) processScannedQRCode(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	// "DEBUG:" lines are filtered to log_level=debug|all|"" by shouldEmitLogLine.
	debugf("QR code read: %q (len=%d)", raw, len(raw))
	if ctx.qrSuppressRepeatReject(raw) {
		debugf("QR repeat of rejected payload within %s; reject feedback suppressed.", qrRejectRepeatQuietPeriod)
		return
	}
	if ctx.inputHeld("qr") {
		debugf("QR scan ignored (inside pin_entry_feedback_delay after the previous result).")
		return
	}
	ctx.configMu.RLock()
	cfg := ctx.Config
	ctx.configMu.RUnlock()

	if cfg.StaticTestQRCodeEnabled {
		want := strings.TrimSpace(cfg.StaticTestQRCode)
		if want == "" {
			log.Println("WARNING: static_test_qr_code_enabled is true but static_test_qr_code is empty; rejecting QR.")
			ctx.qrRejectWithAccessDenied(cfg, raw, "qr_static_test_not_configured", map[string]any{"qr_payload_len": len(raw)})
			return
		}
		if raw == want {
			log.Printf("INFO: Static test QR matched; granting without credential, schedule, or keypad-lockout checks.")
			ctx.qrClearReject()
			ctx.grantStaticTestQRAccess(cfg)
			return
		}
		log.Printf("INFO: QR rejected (static_test_qr_code_enabled: payload does not match static_test_qr_code).")
		ctx.qrRejectWithAccessDenied(cfg, raw, "qr_static_test_mismatch", map[string]any{"qr_payload_len": len(raw)})
		return
	}

	if ctx.keypadLockoutActive() && !ctx.FiremansServiceActive() {
		ctx.qrRejectWithAccessDenied(cfg, raw, "keypad_lockout", map[string]any{"qr_payload_len": len(raw)})
		return
	}
	deviceUUID, stamp, err := parseScannedQRCodePayload(raw)
	if err != nil {
		log.Printf("INFO: QR rejected (payload parse failed): %v", err)
		ctx.qrRejectWithAccessDenied(cfg, raw, "qr_parse_failed", map[string]any{"error": err.Error()})
		return
	}
	pin, err := ctx.resolvePINForMobileUUID(deviceUUID)
	if err != nil {
		reason := "qr_unknown_device_uuid"
		if strings.HasPrefix(err.Error(), "credential_lifecycle:") {
			reason = "credential_lifecycle"
		}
		log.Printf("INFO: QR rejected (uuid=%s): %v", deviceUUID, err)
		ctx.qrRejectWithAccessDenied(cfg, raw, reason, map[string]any{"device_uuid": deviceUUID})
		return
	}
	window := time.Duration(cfg.QRTimeWindowSeconds) * time.Second
	drift := time.Since(stamp)
	if drift < 0 {
		drift = -drift
	}
	if drift > window {
		log.Printf("INFO: QR rejected (uuid=%s; drift=%s > window=%s).", deviceUUID, drift, window)
		ctx.qrRejectWithAccessDenied(cfg, raw, "qr_timestamp_outside_window", map[string]any{
			"device_uuid":  deviceUUID,
			"drift":        drift.String(),
			"max_drift":    window.String(),
			"datetime_utc": stamp.UTC().Format(time.RFC3339Nano),
		})
		return
	}
	log.Printf("INFO: QR accepted (uuid=%s); routing to PIN grant path.", deviceUUID)
	ctx.qrClearReject()
	lcdEnqueueFull(ctx, lcdLinesCardPINPrompt(), 0)
	processPIN(ctx, pin, "qr")
}
