package app

import (
	"testing"
	"time"
)

func TestParseKeypadEntry(t *testing.T) {
	for _, tc := range []struct{ buf, code, pin string }{
		{"123456", "", "123456"},
		{"1 123456", "1", "123456"},
		{"99 123456", "99", "123456"},
		{"2 ", "2", ""},
		{"", "", ""},
	} {
		code, pin := parseKeypadEntry(tc.buf)
		if code != tc.code || pin != tc.pin {
			t.Errorf("parseKeypadEntry(%q) = %q, %q; want %q, %q", tc.buf, code, pin, tc.code, tc.pin)
		}
	}
}

func TestKeypadSpaceAllowed(t *testing.T) {
	for buf, want := range map[string]bool{
		"":     false,
		"1":    true,
		"99":   true,
		"123":  false, // codes are at most 2 digits
		"1 ":   false, // only one separator
		"1 23": false,
		"/":    false,
	} {
		if got := keypadSpaceAllowed(buf); got != want {
			t.Errorf("keypadSpaceAllowed(%q) = %v; want %v", buf, got, want)
		}
	}
}

func TestResolveKeypadEntryDuress(t *testing.T) {
	cfg := DeviceConfig{KeypadFnDuressCode: "99"}
	pin, opts := resolveKeypadEntry(cfg, "99 246810")
	if pin != "246810" || !opts.duress || opts.fnCode != "" {
		t.Fatalf("duress entry: pin=%q opts=%+v", pin, opts)
	}
	pin, opts = resolveKeypadEntry(cfg, "1 246810")
	if pin != "246810" || opts.duress || opts.fnCode != "1" {
		t.Fatalf("function entry: pin=%q opts=%+v", pin, opts)
	}
	if _, opts = resolveKeypadEntry(DeviceConfig{}, "99 246810"); opts.duress {
		t.Fatal("duress must be disabled when keypad_fn_duress_code is empty")
	}
}

func TestKeypadFnCodeValid(t *testing.T) {
	ctx := newDefaultAppContext(t.Context(), nil)
	ctx.elevatorFloorDispatchPins = []uint8{5, 6, 7}
	cfg := DeviceConfig{KeypadFunctionCodesEnabled: true}
	for _, tc := range []struct {
		mode, code string
		latch, want bool
	}{
		{ModeAccessEntry, "", false, true},
		{ModeAccessEntry, "1", false, true},
		{ModeAccessEntry, "2", false, false},
		{ModeAccessEntry, "2", true, true},
		{ModeAccessEntry, "3", true, false},
		{ModeElevatorWaitFloorButtons, "2", false, true},
		{ModeElevatorWaitFloorButtons, "3", false, false}, // only 3 dispatch outputs (0-2)
		{ModeElevatorPredefinedFloor, "1", false, false},
	} {
		c := cfg
		c.KeypadFnLatchEnabled = tc.latch
		if got := ctx.keypadFnCodeValid(c, tc.mode, pinOpts{fnCode: tc.code}); got != tc.want {
			t.Errorf("mode=%s code=%q latch=%v: got %v want %v", tc.mode, tc.code, tc.latch, got, tc.want)
		}
	}
	if ctx.keypadFnCodeValid(DeviceConfig{}, ModeAccessEntry, pinOpts{fnCode: "1"}) {
		t.Error("codes must be rejected when keypad_function_codes_enabled is false")
	}
}

func TestKeypadEntryDuressGrantsNormally(t *testing.T) {
	ctx, rec := newWebhookCtx(t)
	ctx.Config.KeypadOperationMode = ModeAccessEntry
	ctx.Config.KeypadFnDuressCode = "99"
	processKeypadEntry(ctx, "99 246810", "")
	rec.waitFor(t, "duress_alarm")
	e := rec.waitFor(t, "pin_accepted")
	if _, ok := e["fn_code"]; ok {
		t.Fatalf("pin_accepted must not reveal the duress code: %v", e)
	}
}

func TestKeypadEntryExtendedHold(t *testing.T) {
	ctx, rec := newWebhookCtx(t)
	ctx.Config.KeypadOperationMode = ModeAccessEntry
	ctx.Config.KeypadFunctionCodesEnabled = true
	processKeypadEntry(ctx, "1 246810", "")
	e := rec.waitFor(t, "pin_accepted")
	if e["fn_code"] != "1" {
		t.Fatalf("unexpected payload: %v", e)
	}
	ctx.doorAlarmMu.Lock()
	extra := ctx.doorHoldExtraGrace
	ctx.doorAlarmMu.Unlock()
	if extra != ctx.Config.KeypadFnExtendedHoldExtra {
		t.Fatalf("door hold extra = %s; want %s", extra, ctx.Config.KeypadFnExtendedHoldExtra)
	}
}

func TestKeypadEntryDisabledCodeRejected(t *testing.T) {
	ctx, rec := newWebhookCtx(t)
	ctx.Config.KeypadOperationMode = ModeAccessEntry
	processKeypadEntry(ctx, "1 246810", "")
	e := rec.waitFor(t, "pin_rejected")
	if e["reason"] != "invalid_function_code" {
		t.Fatalf("unexpected payload: %v", e)
	}
}

func TestKeypadEntryLatchToggle(t *testing.T) {
	ctx, rec := newWebhookCtx(t)
	ctx.Config.KeypadOperationMode = ModeAccessEntry
	ctx.Config.KeypadFunctionCodesEnabled = true
	ctx.Config.KeypadFnLatchEnabled = true
	ctx.Config.PinEntryFeedbackDelay = 0
	processKeypadEntry(ctx, "2 246810", "")
	rec.waitFor(t, "door_latched")
	if !ctx.DoorLatched() {
		t.Fatal("door should be latched")
	}
	processKeypadEntry(ctx, "2 246810", "")
	rec.waitFor(t, "door_unlatched")
	if ctx.DoorLatched() {
		t.Fatal("second latch code should release the latch")
	}
}

func TestDoorLatchAutoRelease(t *testing.T) {
	ctx, rec := newWebhookCtx(t)
	if err := ctx.latchDoor(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	e := rec.waitFor(t, "door_unlatched")
	if e["reason"] != "keypad_fn_latch_max" || ctx.DoorLatched() {
		t.Fatalf("auto-release: payload=%v latched=%v", e, ctx.DoorLatched())
	}
}

func TestDoorbellCooldown(t *testing.T) {
	ctx, rec := newWebhookCtx(t)
	ctx.Config.KeypadDoorbellEnabled = true
	ctx.Config.KeypadDoorbellCooldown = time.Hour
	ctx.ringDoorbell("")
	ctx.ringDoorbell("")
	rec.waitFor(t, "doorbell")
	time.Sleep(200 * time.Millisecond)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	n := 0
	for _, e := range rec.evts {
		if e["event"] == "doorbell" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("doorbell rang %d times within the cooldown; want 1", n)
	}
}
