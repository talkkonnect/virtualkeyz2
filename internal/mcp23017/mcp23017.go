// Package mcp23017 drives an MCP23017 16-bit I2C GPIO expander (relays as outputs).
// Pin index 0–7 is port A (GPA0–GPA7), 8–15 is port B (GPB0–GPB7).
package mcp23017

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Linux /dev/i2c-* ioctl: set 7-bit slave address for subsequent read/write.
const i2cSlave = 0x0703

const (
	regIODIRA = 0x00
	regIODIRB = 0x01
	regOLATA  = 0x14
	regOLATB  = 0x15
)

// Dev is an open I2C handle to one MCP23017 at a 7-bit address (typically 0x20–0x27).
type Dev struct {
	f   *os.File
	mu  sync.Mutex
	a7  uint8 // 7-bit I2C address
	sA  uint8 // output latch shadow port A
	sB  uint8 // output latch shadow port B
}

// Open opens /dev/i2c-<bus>, claims the slave, configures all 16 pins as outputs,
// and drives both ports high (typical idle for active-low relay inputs).
func Open(bus int, addr7 uint8) (*Dev, error) {
	if bus < 0 || bus > 255 {
		return nil, fmt.Errorf("mcp23017: invalid I2C bus %d", bus)
	}
	if addr7 < 0x08 || addr7 > 0x77 {
		return nil, fmt.Errorf("mcp23017: invalid 7-bit I2C address 0x%02x", addr7)
	}
	path := fmt.Sprintf("/dev/i2c-%d", bus)
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mcp23017 open %q: %w", path, err)
	}
	d := &Dev{f: f, a7: addr7, sA: 0xff, sB: 0xff}
	if err := d.setSlave(); err != nil {
		_ = f.Close()
		return nil, err
	}
	// All pins output.
	if err := d.writeRegUnlocked(regIODIRA, 0x00); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mcp23017 IODIRA: %w", err)
	}
	if err := d.writeRegUnlocked(regIODIRB, 0x00); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mcp23017 IODIRB: %w", err)
	}
	if err := d.writeRegUnlocked(regOLATA, d.sA); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mcp23017 OLATA: %w", err)
	}
	if err := d.writeRegUnlocked(regOLATB, d.sB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mcp23017 OLATB: %w", err)
	}
	return d, nil
}

// Close releases the I2C handle.
func (d *Dev) Close() error {
	if d == nil || d.f == nil {
		return nil
	}
	err := d.f.Close()
	d.f = nil
	return err
}

func (d *Dev) setSlave() error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), uintptr(i2cSlave), uintptr(d.a7))
	if errno != 0 {
		return fmt.Errorf("mcp23017 I2C_SLAVE 0x%02x: %w", d.a7, errno)
	}
	return nil
}

func (d *Dev) writeRegUnlocked(reg, val uint8) error {
	if err := d.setSlave(); err != nil {
		return err
	}
	_, err := d.f.Write([]byte{reg, val})
	if err != nil {
		return err
	}
	return nil
}

// SetPin drives expander pin pin (0–15); high true means logic high on the pin.
func (d *Dev) SetPin(pin uint8, high bool) error {
	if pin > 15 {
		return fmt.Errorf("mcp23017: pin %d out of range 0–15", pin)
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	var mask uint8
	if pin < 8 {
		mask = 1 << pin
		if high {
			d.sA |= mask
		} else {
			d.sA &^= mask
		}
		return d.writeRegUnlocked(regOLATA, d.sA)
	}
	mask = 1 << (pin - 8)
	if high {
		d.sB |= mask
	} else {
		d.sB &^= mask
	}
	return d.writeRegUnlocked(regOLATB, d.sB)
}
