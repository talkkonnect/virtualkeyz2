package app

import (
	"testing"
	"time"
)

func TestQRSuppressRepeatReject(t *testing.T) {
	ctx := &AppContext{}
	if ctx.qrSuppressRepeatReject("bad") {
		t.Fatal("first read of a payload must not be suppressed")
	}
	ctx.qrRememberReject("bad")
	if !ctx.qrSuppressRepeatReject("bad") {
		t.Fatal("immediate repeat of rejected payload should be suppressed")
	}
	if ctx.qrSuppressRepeatReject("other") {
		t.Fatal("a different payload must not be suppressed")
	}
	// Simulate the code leaving the scanner's view for longer than the quiet period.
	ctx.qrRejectLastSeen = time.Now().Add(-qrRejectRepeatQuietPeriod - time.Second)
	if ctx.qrSuppressRepeatReject("bad") {
		t.Fatal("rescan after quiet period must not be suppressed")
	}
	ctx.qrClearReject()
	if ctx.qrSuppressRepeatReject("bad") {
		t.Fatal("cleared state must not suppress")
	}
}
