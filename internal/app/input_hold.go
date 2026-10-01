package app

import (
	"sync"
	"time"
)

// Input hold: after a PIN/QR result the submitting input source ignores new keys/scans for
// pin_entry_feedback_delay (and while a *_blocking feedback sound is still playing) instead of
// sleeping on the listener goroutine. Sources are keyed by keypad role ("", "entry", "exit", "qr"),
// matching the old behaviour where each listener goroutine paused independently.

type inputHoldState struct {
	until  time.Time
	sounds int // blocking feedback sounds still playing for this source
}

type inputHolds struct {
	mu sync.Mutex
	m  map[string]*inputHoldState
}

func (h *inputHolds) get(role string) *inputHoldState {
	if h.m == nil {
		h.m = make(map[string]*inputHoldState)
	}
	s, ok := h.m[role]
	if !ok {
		s = &inputHoldState{}
		h.m[role] = s
	}
	return s
}

// holdInput makes role ignore input for d from now (never shortens an existing hold).
func (ctx *AppContext) holdInput(role string, d time.Duration) {
	if d <= 0 {
		return
	}
	ctx.inputHold.mu.Lock()
	defer ctx.inputHold.mu.Unlock()
	s := ctx.inputHold.get(role)
	if t := time.Now().Add(d); t.After(s.until) {
		s.until = t
	}
}

// inputHeld reports whether role is inside its post-result feedback window.
func (ctx *AppContext) inputHeld(role string) bool {
	ctx.inputHold.mu.Lock()
	defer ctx.inputHold.mu.Unlock()
	s, ok := ctx.inputHold.m[role]
	if !ok {
		return false
	}
	return s.sounds > 0 || time.Now().Before(s.until)
}

// playFeedbackSound queues a feedback sound without blocking the caller. When blocking is set
// (sound_<name>_blocking), role's input stays held until the sound has finished.
func (ctx *AppContext) playFeedbackSound(role string, cfg DeviceConfig, path string, enabled, blocking bool) {
	if !enabled || path == "" {
		return
	}
	if !blocking {
		playSoundAsync(cfg, path)
		return
	}
	done := playSoundQueued(cfg, path)
	if done == nil {
		return
	}
	ctx.inputHold.mu.Lock()
	ctx.inputHold.get(role).sounds++
	ctx.inputHold.mu.Unlock()
	go func() {
		<-done
		ctx.inputHold.mu.Lock()
		ctx.inputHold.get(role).sounds--
		ctx.inputHold.mu.Unlock()
	}()
}

func (ctx *AppContext) playOKSound(role string, cfg DeviceConfig) {
	ctx.playFeedbackSound(role, cfg, cfg.SoundPinOK, cfg.SoundPinOKEnabled, cfg.SoundPinOKBlocking)
}

func (ctx *AppContext) playRejectSound(role string, cfg DeviceConfig) {
	ctx.playFeedbackSound(role, cfg, cfg.SoundPinReject, cfg.SoundPinRejectEnabled, cfg.SoundPinRejectBlocking)
}
