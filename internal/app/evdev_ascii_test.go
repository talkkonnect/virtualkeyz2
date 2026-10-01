package app

import (
	"fmt"
	"strings"
	"testing"
)

// TestGoldenEvdevKeyToASCII snapshots the scancode -> ASCII mapping (shift off/on).
func TestGoldenEvdevKeyToASCII(t *testing.T) {
	var out strings.Builder
	for sc := range uint16(768) {
		lo, okLo := evdevKeyToASCII(sc, false)
		hi, okHi := evdevKeyToASCII(sc, true)
		if okLo || okHi {
			fmt.Fprintf(&out, "%d %q %v %q %v\n", sc, lo, okLo, hi, okHi)
		}
	}
	checkGolden(t, "evdev_ascii.golden.txt", []byte(out.String()))
}
