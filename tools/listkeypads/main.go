// listkeypads lists stable Linux input symlinks (/dev/input/by-id, /dev/input/by-path)
// resolved to /dev/input/eventN, with names and USB Phys lines for keypad_evdev_path wiring.
//
// Usage (from repo root):
//
//	go run ./tools/listkeypads
//	go run ./tools/listkeypads -usb
//	go build -o listkeypads ./tools/listkeypads && ./listkeypads -usb
package main

import (
	"flag"
	"fmt"
	"os"

	"virtualkeyz2/internal/keypadlist"
)

func main() {
	usbOnly := flag.Bool("usb", false, "only list USB devices (Phys starts with usb-)")
	flag.Parse()
	if err := keypadlist.Fprint(os.Stdout, *usbOnly); err != nil {
		fmt.Fprintf(os.Stderr, "listkeypads: %v\n", err)
		os.Exit(1)
	}
}
