package app

import (
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/warthog618/go-gpiocdev"
)

func manageHardwareHeartbeat(bcm uint8) {
	// Blinks onboard heartbeat LED to indicate software is running
	ln, err := gpiocdev.RequestLine(gpioChipName, int(bcm), gpiocdev.AsOutput(0), gpiocdev.WithConsumer("virtualkeyz2-heartbeat"))
	if err != nil {
		log.Printf("WARNING: heartbeat GPIO BCM %d: %v", bcm, err)
		return
	}
	defer ln.Close()
	v := 0
	for {
		v = 1 - v
		if err := ln.SetValue(v); err != nil {
			log.Printf("WARNING: heartbeat SetValue BCM %d: %v", bcm, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// gpioChipName is the kernel gpiochip device name (e.g. gpiochip0), set by openGPIOController.
var gpioChipName string

func openGPIOController() error {
	var candidates []string
	if v := strings.TrimSpace(os.Getenv("VIRTUALKEYZ2_GPIOCHIP")); v != "" {
		candidates = append(candidates, v)
	} else {
		// Prefer the SoC header GPIO controller only; do not scan every gpiochip
		// (e.g. gpiochip1 on Raspberry Pi is power/LED, not header BCM lines).
		candidates = append(candidates, "gpiochip0", "gpiochip4")
	}
	var lastErr error
	for _, name := range candidates {
		c, err := gpiocdev.NewChip(name)
		if err != nil {
			lastErr = err
			continue
		}
		_ = c.Close()
		gpioChipName = name
		log.Printf("INFO: GPIO chip: %s (set VIRTUALKEYZ2_GPIOCHIP to override)", name)
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("no accessible gpiochip device")
}

// gpioLogicalOutputValue maps relay energized ("on") to gpio-cdev line value (0 = inactive, 1 = active for default active-high mapping).
func gpioLogicalOutputValue(activeLow, energized bool) int {
	if energized == activeLow {
		return 0
	}
	return 1
}

func gpioLineIsPhysicalLow(line *gpiocdev.Line) (bool, error) {
	if line == nil {
		return false, errors.New("nil gpio line")
	}
	v, err := line.Value()
	if err != nil {
		return false, err
	}
	// Default active-high: inactive (0) is physical low.
	return v == 0, nil
}

// i2cRelayRetryDelays are backoff delays before I2C relay write attempts 2–5 within a phase (5 ms, 15 ms, then ×3).
var i2cRelayRetryDelays = []time.Duration{
	5 * time.Millisecond,
	15 * time.Millisecond,
	45 * time.Millisecond,
	135 * time.Millisecond,
}

const i2cRelayAttemptsPerPhase = 5

// bitbangI2CBusRecoverySCL emits up to nine SCL pulses on a BCM line (I2C adapter closed) to clock a stuck slave off SDA.
func bitbangI2CBusRecoverySCL(chip string, sclBCM uint8) {
	if chip == "" || sclBCM == 0 {
		return
	}
	ln, err := gpiocdev.RequestLine(chip, int(sclBCM), gpiocdev.AsOutput(1), gpiocdev.WithConsumer("virtualkeyz2-i2c-recovery"))
	if err != nil {
		log.Printf("WARNING: I2C bus recovery: cannot request SCL GPIO %d: %v", sclBCM, err)
		return
	}
	defer ln.Close()
	toggle := func(high bool) {
		v := 0
		if high {
			v = 1
		}
		_ = ln.SetValue(v)
		time.Sleep(5 * time.Microsecond)
	}
	for i := 0; i < 9; i++ {
		toggle(false)
		toggle(true)
	}
	log.Printf("INFO: I2C bus recovery: completed SCL bit-bang on BCM %d.", sclBCM)
}

// --- Structures ---

// i2cRelayExpander drives relay outputs 0–15 on an MCP23017 or XL9535 (see gpio.relay_output_mode).
type i2cRelayExpander interface {
	SetPin(pin uint8, high bool) error
}

// OutputConfig defines an output pin, like a door relay or buzzer
type OutputConfig struct {
	PinNumber uint8
	ActiveLow bool           // True if the relay triggers on ground/0V (common for opto-relays)
	Line      *gpiocdev.Line // SoC line; nil when UseI2CRelay or request failed
	// UseI2CRelay: when true, PinNumber is expander index 0-15 (MCP23017 or XL9535 per relay_output_mode).
	UseI2CRelay bool
	mu          sync.Mutex // Prevents overlapping pulse routines
}

// InputConfig defines an input pin, like a door sensor or egress button
type InputConfig struct {
	PinNumber    uint8
	PullUp       bool          // Enable internal pull-up resistor
	DebounceTime time.Duration // Time to ignore subsequent triggers
	AnyEdge      bool          // When true, both rising and falling edges are detected (maintained switches).
	Line         *gpiocdev.Line
	Action       func() // The function to call when triggered
	debounceMu   sync.Mutex
	lastTrigger  time.Time // Used for debouncing
}

// elevatorFloorPin holds a BCM input wired to a cab floor button (active low when pressed).
type elevatorFloorPin struct {
	Line *gpiocdev.Line
	BCM  uint8
}

// GPIOManager holds the state of all physical IO
type GPIOManager struct {
	Outputs map[string]*OutputConfig
	Inputs  map[string]*InputConfig

	i2cRelay             i2cRelayExpander
	i2cRelayCloser       io.Closer
	i2cOpsMu             sync.Mutex // serializes expander I/O, bus recovery, and adapter reinit
	i2cReopenFn          func() (i2cRelayExpander, error)
	i2cBusRecoverySCLBCM uint8

	// doorHoldWhile, if set, is consulted at the end of ActionPulse("door", ...): when true the door relay
	// stays energized (fire alarm interface fail-unlock) instead of returning to off.
	doorHoldWhile func() bool

	doorSensorLine  *gpiocdev.Line
	doorSensorReady bool

	elevatorFloorPins []elevatorFloorPin
}

// --- Initialization ---

func NewGPIOManager() *GPIOManager {
	return &GPIOManager{
		Outputs: make(map[string]*OutputConfig),
		Inputs:  make(map[string]*InputConfig),
	}
}

// SetDoorHoldOpenWhile sets a callback used by ActionPulse on output "door" to leave the relay on when true.
func (m *GPIOManager) SetDoorHoldOpenWhile(f func() bool) {
	m.doorHoldWhile = f
}

// SetI2CRelayExpander attaches an MCP23017 or XL9535 for outputs registered with UseI2CRelay true.
func (m *GPIOManager) SetI2CRelayExpander(d i2cRelayExpander) {
	m.i2cOpsMu.Lock()
	defer m.i2cOpsMu.Unlock()
	m.setI2CRelayExpanderLocked(d)
}

func (m *GPIOManager) setI2CRelayExpanderLocked(d i2cRelayExpander) {
	m.i2cRelay = d
	if c, ok := d.(io.Closer); ok {
		m.i2cRelayCloser = c
	} else {
		m.i2cRelayCloser = nil
	}
}

// SetI2CRelayBusRecovery registers reopen after closing /dev/i2c-* and optional SCL bit-bang (BCM). Used only in I2C relay modes.
func (m *GPIOManager) SetI2CRelayBusRecovery(reopen func() (i2cRelayExpander, error), sclRecoveryBCM uint8) {
	m.i2cReopenFn = reopen
	m.i2cBusRecoverySCLBCM = sclRecoveryBCM
}

// CloseI2CRelayForShutdown closes the active I2C expander handle (call from process exit; safe if no I2C relays).
func (m *GPIOManager) CloseI2CRelayForShutdown() {
	if m == nil {
		return
	}
	m.i2cOpsMu.Lock()
	defer m.i2cOpsMu.Unlock()
	if m.i2cRelayCloser != nil {
		_ = m.i2cRelayCloser.Close()
	}
	m.i2cRelayCloser = nil
	m.i2cRelay = nil
}

func (m *GPIOManager) recoverI2CRelayAdapter() error {
	m.i2cOpsMu.Lock()
	defer m.i2cOpsMu.Unlock()
	if m.i2cReopenFn == nil {
		return errors.New("i2c bus recovery: reopen function not configured")
	}
	if m.i2cRelayCloser != nil {
		_ = m.i2cRelayCloser.Close()
	}
	m.i2cRelayCloser = nil
	m.i2cRelay = nil
	if m.i2cBusRecoverySCLBCM != 0 {
		if gpioChipName != "" {
			bitbangI2CBusRecoverySCL(gpioChipName, m.i2cBusRecoverySCLBCM)
		} else {
			log.Println("WARNING: I2C bus recovery: GPIO chip not initialized; skipping SCL bit-bang.")
		}
	}
	time.Sleep(2 * time.Millisecond)
	dev, err := m.i2cReopenFn()
	if err != nil {
		return fmt.Errorf("i2c bus recovery: reopen expander: %w", err)
	}
	m.setI2CRelayExpanderLocked(dev)
	log.Println("INFO: I2C bus recovery: expander reinitialized after adapter close.")
	return nil
}

// i2cSetPinReliable performs several write attempts with exponential backoff, then optional bus recovery and a second attempt batch.
func (m *GPIOManager) i2cSetPinReliable(pin uint8, high bool) error {
	tryBatch := func() error {
		m.i2cOpsMu.Lock()
		defer m.i2cOpsMu.Unlock()
		rel := m.i2cRelay
		if rel == nil {
			return errors.New("i2c relay expander not initialized")
		}
		var lastErr error
		for attempt := 0; attempt < i2cRelayAttemptsPerPhase; attempt++ {
			if attempt > 0 {
				time.Sleep(i2cRelayRetryDelays[attempt-1])
			}
			err := rel.SetPin(pin, high)
			if err == nil {
				return nil
			}
			lastErr = err
			log.Printf("WARNING: I2C relay SetPin pin=%d high=%v attempt %d/%d: %v", pin, high, attempt+1, i2cRelayAttemptsPerPhase, err)
		}
		return lastErr
	}
	if err := tryBatch(); err == nil {
		return nil
	}
	log.Printf("WARNING: I2C relay SetPin pin=%d high=%v: exhausted %d attempts; running bus recovery.", pin, high, i2cRelayAttemptsPerPhase)
	if err := m.recoverI2CRelayAdapter(); err != nil {
		return err
	}
	if err := tryBatch(); err != nil {
		return fmt.Errorf("i2c relay SetPin after bus recovery pin=%d high=%v: %w", pin, high, err)
	}
	return nil
}

func (m *GPIOManager) actionOnErr(name string) error {
	out, exists := m.Outputs[name]
	if !exists {
		return fmt.Errorf("output %q not found", name)
	}
	if out.UseI2CRelay {
		logicHigh := !out.ActiveLow
		return m.i2cSetPinReliable(out.PinNumber, logicHigh)
	}
	if out.Line == nil {
		return fmt.Errorf("output %q has no SoC GPIO line", name)
	}
	v := gpioLogicalOutputValue(out.ActiveLow, true)
	if err := out.Line.SetValue(v); err != nil {
		return fmt.Errorf("output %q SetValue: %w", name, err)
	}
	return nil
}

func (m *GPIOManager) actionOffErr(name string) error {
	out, exists := m.Outputs[name]
	if !exists {
		return fmt.Errorf("output %q not found", name)
	}
	if out.UseI2CRelay {
		logicHigh := out.ActiveLow
		return m.i2cSetPinReliable(out.PinNumber, logicHigh)
	}
	if out.Line == nil {
		return fmt.Errorf("output %q has no SoC GPIO line", name)
	}
	v := gpioLogicalOutputValue(out.ActiveLow, false)
	if err := out.Line.SetValue(v); err != nil {
		return fmt.Errorf("output %q SetValue: %w", name, err)
	}
	return nil
}

// ConfigureDoorSensor sets up a BCM GPIO as digital input with pull-up for a door contact (call once after openGPIOController).
// Typical for DoorSensorClosedIsLow wiring; if your "closed" state is active-high only, adjust hardware pull resistors as needed.
func (m *GPIOManager) ConfigureDoorSensor(bcm uint8) {
	if bcm == 0 {
		return
	}
	ln, err := gpiocdev.RequestLine(gpioChipName, int(bcm), gpiocdev.AsInput, gpiocdev.WithPullUp, gpiocdev.WithConsumer("virtualkeyz2-door-sensor"))
	if err != nil {
		log.Printf("WARNING: door sensor GPIO BCM %d: %v", bcm, err)
		return
	}
	m.doorSensorLine = ln
	m.doorSensorReady = true
}

// DoorSensorConfigured reports whether ConfigureDoorSensor was called.
func (m *GPIOManager) DoorSensorConfigured() bool {
	return m.doorSensorReady
}

// DoorIsOpen returns true when the door is open. closedIsLow matches DeviceConfig.DoorSensorClosedIsLow.
func (m *GPIOManager) DoorIsOpen(closedIsLow bool) bool {
	if !m.doorSensorReady {
		return false
	}
	isLow, err := gpioLineIsPhysicalLow(m.doorSensorLine)
	if err != nil {
		return false
	}
	if closedIsLow {
		return !isLow
	}
	return isLow
}

// ConfigureElevatorFloorPins sets up BCM inputs (pull-up, active low when pressed) for elevator cab floor buttons.
func (m *GPIOManager) ConfigureElevatorFloorPins(bcms []uint8) {
	m.elevatorFloorPins = nil
	for _, bcm := range bcms {
		if bcm == 0 {
			continue
		}
		ln, err := gpiocdev.RequestLine(gpioChipName, int(bcm), gpiocdev.AsInput, gpiocdev.WithPullUp, gpiocdev.WithConsumer("virtualkeyz2-elev-floor"))
		if err != nil {
			log.Printf("WARNING: elevator floor sense BCM %d: %v", bcm, err)
			continue
		}
		m.elevatorFloorPins = append(m.elevatorFloorPins, elevatorFloorPin{Line: ln, BCM: bcm})
	}
	if len(m.elevatorFloorPins) > 0 {
		log.Printf("INFO: Elevator floor sense GPIOs: %v", bcms)
	}
}

// HasElevatorFloorPins reports whether any floor sense inputs are configured.
func (m *GPIOManager) HasElevatorFloorPins() bool {
	return len(m.elevatorFloorPins) > 0
}

// AnyElevatorFloorPressed returns true if any configured floor input reads low (pressed).
func (m *GPIOManager) AnyElevatorFloorPressed() bool {
	return len(m.ElevatorCabFloorsPressed()) > 0
}

// ElevatorCabFloorsPressed returns zero-based indices of cab floor inputs that read low (pressed), in pin order.
func (m *GPIOManager) ElevatorCabFloorsPressed() []int {
	var r []int
	for i, fp := range m.elevatorFloorPins {
		low, err := gpioLineIsPhysicalLow(fp.Line)
		if err == nil && low {
			r = append(r, i)
		}
	}
	return r
}

// HasOutput returns true if a named relay/output was registered.
func (m *GPIOManager) HasOutput(name string) bool {
	_, ok := m.Outputs[name]
	return ok
}

// AddOutput registers a new output pin. useI2CRelay selects expander pin PinNumber (0-15); otherwise BCM GPIO.
func (m *GPIOManager) AddOutput(name string, pin uint8, activeLow bool, useI2CRelay bool) {
	cfg := &OutputConfig{
		PinNumber:   pin,
		ActiveLow:   activeLow,
		UseI2CRelay: useI2CRelay,
	}
	if !useI2CRelay {
		initV := gpioLogicalOutputValue(activeLow, false)
		ln, err := gpiocdev.RequestLine(gpioChipName, int(pin), gpiocdev.AsOutput(initV), gpiocdev.WithConsumer("virtualkeyz2-out-"+name))
		if err != nil {
			log.Printf("WARNING: output %q BCM %d: %v", name, pin, err)
		} else {
			cfg.Line = ln
		}
	}
	m.Outputs[name] = cfg

	// Ensure it starts in the "Off" state
	m.ActionOff(name)
}

// AddInput registers a new input pin and its callback function (single edge: fall if pull-up, rise if pull-down).
func (m *GPIOManager) AddInput(name string, pin uint8, pullUp bool, action func()) {
	m.addInputEdge(name, pin, pullUp, false, action)
}

// AddInputAnyEdge registers an input that fires on both edges (debounced); use for maintained contacts.
func (m *GPIOManager) AddInputAnyEdge(name string, pin uint8, pullUp bool, action func()) {
	m.addInputEdge(name, pin, pullUp, true, action)
}

func (m *GPIOManager) inputEdgeFired(inputName string) {
	in := m.Inputs[inputName]
	if in == nil || in.Action == nil {
		return
	}
	in.debounceMu.Lock()
	if time.Since(in.lastTrigger) <= in.DebounceTime {
		in.debounceMu.Unlock()
		return
	}
	in.lastTrigger = time.Now()
	in.debounceMu.Unlock()
	debugf("GPIO Input '%s' triggered", inputName)
	go in.Action()
}

func (m *GPIOManager) addInputEdge(name string, pin uint8, pullUp bool, anyEdge bool, action func()) {
	if pin == 0 {
		log.Printf("WARNING: GPIO input %q skipped (BCM 0)", name)
		return
	}
	var edgeOpt gpiocdev.LineReqOption
	if pullUp {
		if anyEdge {
			edgeOpt = gpiocdev.WithBothEdges
		} else {
			edgeOpt = gpiocdev.WithFallingEdge
		}
	} else {
		if anyEdge {
			edgeOpt = gpiocdev.WithBothEdges
		} else {
			edgeOpt = gpiocdev.WithRisingEdge
		}
	}
	var bias gpiocdev.LineReqOption = gpiocdev.WithPullUp
	if !pullUp {
		bias = gpiocdev.WithPullDown
	}
	inName := name
	ln, err := gpiocdev.RequestLine(gpioChipName, int(pin),
		bias,
		edgeOpt,
		gpiocdev.WithEventHandler(func(gpiocdev.LineEvent) { m.inputEdgeFired(inName) }),
		gpiocdev.WithConsumer("virtualkeyz2-in-"+name),
	)
	if err != nil {
		log.Printf("WARNING: input %q BCM %d: %v", name, pin, err)
		return
	}
	m.Inputs[name] = &InputConfig{
		PinNumber:    pin,
		PullUp:       pullUp,
		DebounceTime: 300 * time.Millisecond,
		AnyEdge:      anyEdge,
		Line:         ln,
		Action:       action,
		lastTrigger:  time.Now(),
	}
}

// --- Output Actions ---

// ActionOn turns the output on continuously
func (m *GPIOManager) ActionOn(name string) {
	if err := m.actionOnErr(name); err != nil {
		log.Printf("ERROR: ActionOn %q: %v", name, err)
	}
}

// ActionOff turns the output off continuously
func (m *GPIOManager) ActionOff(name string) {
	if err := m.actionOffErr(name); err != nil {
		log.Printf("ERROR: ActionOff %q: %v", name, err)
	}
}

// ActionPulse turns the output on for a duration, then off.
// Runs concurrently so it doesn't block the main thread.
func (m *GPIOManager) ActionPulse(name string, duration time.Duration) {
	out, exists := m.Outputs[name]
	if !exists {
		return
	}

	go func() {
		// Lock prevents two quick pulses from stepping on each other
		out.mu.Lock()
		defer out.mu.Unlock()

		if err := m.actionOnErr(name); err != nil {
			log.Printf("ERROR: ActionPulse %q on-phase: %v", name, err)
			return
		}
		time.Sleep(duration)
		if name == "door" && m.doorHoldWhile != nil && m.doorHoldWhile() {
			debugf("Fire alarm interface: door relay hold-open active — skipping off phase after pulse.")
			return
		}
		if err := m.actionOffErr(name); err != nil {
			log.Printf("ERROR: ActionPulse %q off-phase: %v", name, err)
		}
	}()
}

// ActionPulseChecked energizes the output synchronously and returns any on-phase error, so callers
// can report a failed actuation. The off phase runs in the background after duration (the caller
// is not blocked for the pulse); an off-phase error is passed to onOffErr when non-nil. Pulses on
// the same output stay serialized via out.mu, which the background goroutine releases.
func (m *GPIOManager) ActionPulseChecked(name string, duration time.Duration, onOffErr func(error)) error {
	out, exists := m.Outputs[name]
	if !exists {
		return fmt.Errorf("ActionPulseChecked: output %q not found", name)
	}
	out.mu.Lock()
	if err := m.actionOnErr(name); err != nil {
		out.mu.Unlock()
		return fmt.Errorf("pulse %q on-phase: %w", name, err)
	}
	go func() {
		defer out.mu.Unlock()
		time.Sleep(duration)
		if name == "door" && m.doorHoldWhile != nil && m.doorHoldWhile() {
			return
		}
		if err := m.actionOffErr(name); err != nil {
			err = fmt.Errorf("pulse %q off-phase: %w", name, err)
			log.Printf("ERROR: %v", err)
			if onOffErr != nil {
				onOffErr(err)
			}
		}
	}()
	return nil
}

// doorPulseOffErrReporter returns an off-phase callback that raises hardware_actuation_failed
// (the relay may be stuck energized) with the given webhook context.
func (ctx *AppContext) doorPulseOffErrReporter(detail map[string]any) func(error) {
	return func(err error) {
		wh := maps.Clone(detail)
		if wh == nil {
			wh = map[string]any{}
		}
		wh["error"] = err.Error()
		wh["phase"] = "off"
		fireEventWebhook(ctx, "hardware_actuation_failed", wh)
	}
}

// parseBCMPinList parses comma-separated BCM numbers (e.g. "17,27,22") for elevator floor sense inputs.
func parseBCMPinList(s string) ([]uint8, error) {
	return parseCommaList(s, func(p string) (uint8, error) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, fmt.Errorf("invalid BCM %q: %w", p, err)
		}
		return bcmUint8("elevator_floor_input", n)
	})
}

func setupOperationModeGPIOInputs(ctx *AppContext, gpio *GPIOManager) {
	ctx.configMu.RLock()
	ex := ctx.GPIOSettings.ExitButtonPin
	exLow := ctx.GPIOSettings.ExitButtonActiveLow
	en := ctx.GPIOSettings.EntryButtonPin
	enLow := ctx.GPIOSettings.EntryButtonActiveLow
	lb := ctx.GPIOSettings.LightingButtonPin
	lbLow := ctx.GPIOSettings.LightingButtonActiveLow
	ctx.configMu.RUnlock()

	// Register pins whenever configured so runtime keypad_operation_mode changes take effect
	// without restart. Callbacks gate relay pulses on the current mode.
	if ex != 0 {
		gpio.AddInput("exit_button", ex, exLow, func() {
			ctx.configMu.RLock()
			cfg := ctx.Config
			m := NormalizeKeypadOperationMode(cfg.KeypadOperationMode)
			feedbackDelay := cfg.PinEntryFeedbackDelay
			doorBCM := ctx.GPIOSettings.DoorRelayPin
			ctx.configMu.RUnlock()
			if !modeUsesExitGPIOButton(m) {
				return
			}
			kTag := keypadLogTag("")
			log.Printf("INFO: PIN accepted (mode=%s %s keypad; credential=%s); door relay GPIO %d.", m, kTag, "exit_button", doorBCM)
			ctx.grantDefaultModeDoorUnlockLikePIN("", cfg, m, "", "exit_button", doorBCM, feedbackDelay, 0, nil, false)
		})
	}
	if en != 0 {
		gpio.AddInput("entry_button", en, enLow, func() {
			ctx.configMu.RLock()
			cfg := ctx.Config
			m := NormalizeKeypadOperationMode(cfg.KeypadOperationMode)
			feedbackDelay := cfg.PinEntryFeedbackDelay
			doorBCM := ctx.GPIOSettings.DoorRelayPin
			ctx.configMu.RUnlock()
			if !modeUsesEntryGPIOButton(m) {
				return
			}
			kTag := keypadLogTag("")
			log.Printf("INFO: PIN accepted (mode=%s %s keypad; credential=%s); door relay GPIO %d.", m, kTag, "entry_button", doorBCM)
			ctx.grantDefaultModeDoorUnlockLikePIN("", cfg, m, "", "entry_button", doorBCM, feedbackDelay, 0, nil, false)
		})
	}
	if lb != 0 {
		gpio.AddInput("lighting_button", lb, lbLow, func() {
			ctx.lightingManualButtonPressed()
		})
	}
}
