package app

import (
	"testing"
	"time"
)

func TestInputHoldPerRole(t *testing.T) {
	ctx := &AppContext{}
	if ctx.inputHeld("") {
		t.Fatal("no hold armed")
	}
	ctx.holdInput("entry", time.Hour)
	if !ctx.inputHeld("entry") {
		t.Fatal("entry should be held")
	}
	if ctx.inputHeld("exit") || ctx.inputHeld("qr") {
		t.Fatal("hold must be per input source")
	}
	// A shorter hold never shortens an existing one.
	ctx.holdInput("entry", time.Millisecond)
	if !ctx.inputHeld("entry") {
		t.Fatal("shorter hold shortened the window")
	}
	ctx.holdInput("qr", time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if ctx.inputHeld("qr") {
		t.Fatal("expired hold still active")
	}
	ctx.holdInput("qr", 0)
	if ctx.inputHeld("qr") {
		t.Fatal("zero hold must be a no-op")
	}
}

func TestPlayFeedbackSoundMissingFileDoesNotHold(t *testing.T) {
	quietLogs(t)
	ctx := &AppContext{}
	ctx.playFeedbackSound("", DeviceConfig{}, "/nonexistent/virtualkeyz2-test.wav", true, true)
	if ctx.inputHeld("") {
		t.Fatal("missing sound file must not hold input")
	}
}
