// Package xl9535 drives an XL9535 16-bit I2C GPIO expander (common on some relay boards).
// Pin index 0–7 is port 0, 8–15 is port 1 (bit pin−8). Register map matches PCA9535-style devices.
package xl9535

import (
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

const i2cSlave = 0x0703

const (
	regInputPort0    = 0x00
	regInputPort1    = 0x01
	regOutputPort0   = 0x02
	regOutputPort1   = 0x03
	regConfigPort0   = 0x06
	regConfigPort1   = 0x07
)

// Dev is an open I2C handle to one XL9535 at a 7-bit address (often 0x20–0x27 per A0–A2).
type Dev struct {
	f  *os.File
	mu sync.Mutex
	a7 uint8 // 7-bit I2C address
	s0 uint8 // output shadow port 0
	s1 uint8 // output shadow port 1
}

// Open opens /dev/i2c-<bus>, claims the slave, configures all 16 pins as outputs,
// and drives both ports high (typical idle for active-low relay inputs).
func Open(bus int, addr7 uint8) (*Dev, error) {
	if bus < 0 || bus > 255 {
		return nil, fmt.Errorf("xl9535: invalid I2C bus %d", bus)
	}
	if addr7 < 0x08 || addr7 > 0x77 {
		return nil, fmt.Errorf("xl9535: invalid 7-bit I2C address 0x%02x", addr7)
	}
	path := fmt.Sprintf("/dev/i2c-%d", bus)
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("xl9535 open %q: %w", path, err)
	}
	d := &Dev{f: f, a7: addr7, s0: 0xff, s1: 0xff}
	if err := d.setSlave(); err != nil {
		_ = f.Close()
		return nil, err
	}
	// CONFIG: 0 = output, 1 = input — all outputs.
	if err := d.writeRegUnlocked(regConfigPort0, 0x00); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("xl9535 CONFIG_PORT_0: %w", err)
	}
	if err := d.writeRegUnlocked(regConfigPort1, 0x00); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("xl9535 CONFIG_PORT_1: %w", err)
	}
	if err := d.writeRegUnlocked(regOutputPort0, d.s0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("xl9535 OUTPUT_PORT_0: %w", err)
	}
	if err := d.writeRegUnlocked(regOutputPort1, d.s1); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("xl9535 OUTPUT_PORT_1: %w", err)
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
		return fmt.Errorf("xl9535 I2C_SLAVE 0x%02x: %w", d.a7, errno)
	}
	return nil
}

func (d *Dev) writeRegUnlocked(reg, val uint8) error {
	if err := d.setSlave(); err != nil {
		return err
	}
	_, err := d.f.Write([]byte{reg, val})
	return err
}

// SetPin drives expander pin pin (0–15); high true means logic high on the pin.
func (d *Dev) SetPin(pin uint8, high bool) error {
	if pin > 15 {
		return fmt.Errorf("xl9535: pin %d out of range 0–15", pin)
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if pin < 8 {
		mask := uint8(1 << pin)
		if high {
			d.s0 |= mask
		} else {
			d.s0 &^= mask
		}
		return d.writeRegUnlocked(regOutputPort0, d.s0)
	}
	mask := uint8(1 << (pin - 8))
	if high {
		d.s1 |= mask
	} else {
		d.s1 &^= mask
	}
	return d.writeRegUnlocked(regOutputPort1, d.s1)
}
