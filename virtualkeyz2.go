package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unsafe"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gin-gonic/gin"
	evdev "github.com/gvalkov/golang-evdev"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stianeikeland/go-rpio/v4"
	"golang.org/x/term"

	"virtualkeyz2/internal/keypadlist"
	"virtualkeyz2/internal/mcp23017"
	"virtualkeyz2/internal/xl9535"
)

// Software build metadata — updated by ./tools/bump-version.sh after each documented revision.
const (
	SoftwareVersion    = "0.06"
	SoftwareReleaseUTC = "2026-04-14T05:53:47Z"
)

// Keypad / access operation modes (device.keypad_operation_mode in JSON).
const (
	ModeAccessEntry               = "access_entry"
	ModeAccessExit                = "access_exit"
	ModeAccessEntryWithExitButton = "access_entry_with_exit_button"
	ModeAccessExitWithEntryButton = "access_exit_with_entry_button"
	ModeAccessDualUSBKeypad       = "access_dual_usb_keypad"
	ModeAccessPairedRemoteExit    = "access_paired_remote_exit"
	// ModeElevatorWaitFloorButtons: cab floor buttons are isolated from ground until allowed; after a valid PIN, enable relays hold for elevator_floor_wait_timeout. Sub-mode device.elevator_wait_floor_cab_sense: sense (default) reads elevator_floor_input_pins, logs floor, pulses matching dispatch; ignore skips cab GPIO entirely (leave elevator_floor_input_pins empty).
	ModeElevatorWaitFloorButtons = "elevator_wait_floor_buttons"
	// ModeElevatorPredefinedFloor: in-cab floor buttons are not used; one relay output pulses to complete the call circuit for a single predefined floor (no cab wait). At most one floor in elevator_predefined_floors / one predefined enable pin.
	ModeElevatorPredefinedFloor = "elevator_predefined_floor"
)

// Relay output backend (gpio.relay_output_mode). Sensor, buttons, and LED pins stay on SoC BCM GPIO.
const (
	RelayOutputGPIO     = "gpio"
	RelayOutputMCP23017 = "mcp23017"
	RelayOutputXL9535   = "xl9535"
)

// PairPeerRoleEntry / PairPeerExit used with ModeAccessPairedRemoteExit (MQTT to sibling controller).
const (
	PairPeerRoleNone  = "none"
	PairPeerRoleEntry = "entry"
	PairPeerRoleExit  = "exit"
)

// NormalizeKeypadOperationMode returns a canonical mode string or access_entry if unknown.
func NormalizeKeypadOperationMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", ModeAccessEntry:
		return ModeAccessEntry
	case ModeAccessExit:
		return ModeAccessExit
	case ModeAccessEntryWithExitButton:
		return ModeAccessEntryWithExitButton
	case ModeAccessExitWithEntryButton:
		return ModeAccessExitWithEntryButton
	case ModeAccessDualUSBKeypad:
		return ModeAccessDualUSBKeypad
	case ModeAccessPairedRemoteExit:
		return ModeAccessPairedRemoteExit
	case ModeElevatorWaitFloorButtons:
		return ModeElevatorWaitFloorButtons
	case ModeElevatorPredefinedFloor:
		return ModeElevatorPredefinedFloor
	default:
		return ModeAccessEntry
	}
}

func normalizePairPeerRole(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case PairPeerRoleEntry:
		return PairPeerRoleEntry
	case PairPeerRoleExit:
		return PairPeerRoleExit
	default:
		return PairPeerRoleNone
	}
}

func isDualUSBKeypadMode(mode string) bool {
	return mode == ModeAccessDualUSBKeypad
}

func modeUsesExitGPIOButton(mode string) bool {
	return mode == ModeAccessEntryWithExitButton
}

func modeUsesEntryGPIOButton(mode string) bool {
	return mode == ModeAccessExitWithEntryButton
}

func isElevatorWaitFloorMode(mode string) bool {
	return mode == ModeElevatorWaitFloorButtons
}

func isElevatorPredefinedMode(mode string) bool {
	return mode == ModeElevatorPredefinedFloor
}

func isElevatorKeypadMode(mode string) bool {
	return isElevatorWaitFloorMode(mode) || isElevatorPredefinedMode(mode)
}

// Elevator wait-floor cab sense sub-modes (device.elevator_wait_floor_cab_sense).
const (
	ElevatorWaitFloorCabSenseSense  = "sense"
	ElevatorWaitFloorCabSenseIgnore = "ignore"
)

// Cab sense after wait-floor PIN grant: ignore GPIO briefly so enable relays match "ignore" mode (hold
// until timeout) while hardware energizes; then require a stable active-low window before dispatch.
const (
	elevatorCabSenseArmDelay    = 300 * time.Millisecond
	elevatorCabSenseStableTicks = 3 // 50ms monitor tick → ~150ms same reading
)

// normalizeElevatorWaitFloorCabSense returns ElevatorWaitFloorCabSenseSense or ElevatorWaitFloorCabSenseIgnore.
func normalizeElevatorWaitFloorCabSense(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ElevatorWaitFloorCabSenseIgnore, "off", "false", "no":
		return ElevatorWaitFloorCabSenseIgnore
	default:
		return ElevatorWaitFloorCabSenseSense
	}
}

func elevatorWaitFloorSenseCabInputs(cfg DeviceConfig) bool {
	return normalizeElevatorWaitFloorCabSense(cfg.ElevatorWaitFloorCabSense) == ElevatorWaitFloorCabSenseSense
}

// elevatorWaitFloorEnableChannelCount is the number of independent enable outputs (per-floor list or one legacy relay).
func elevatorWaitFloorEnableChannelCount(app *AppContext) int {
	if n := len(app.elevatorWaitFloorEnablePins); n > 0 {
		return n
	}
	if app.GPIOSettings.ElevatorEnableRelayPin != 0 {
		return 1
	}
	return 0
}

func pairedEntryPublishesToPeer(mode, pairRole string) bool {
	return mode == ModeAccessPairedRemoteExit && strings.EqualFold(pairRole, PairPeerRoleEntry)
}

func pairedExitSubscribesToPeer(mode, pairRole string) bool {
	return mode == ModeAccessPairedRemoteExit && strings.EqualFold(pairRole, PairPeerRoleExit)
}

// GPIOSettings holds BCM pin numbers and relay polarity (wiring on the Pi).
type GPIOSettings struct {
	// RelayOutputMode: RelayOutputGPIO (BCM), RelayOutputMCP23017, or RelayOutputXL9535 (I2C expanders, relay pins 0–15).
	RelayOutputMode string
	// MCP23017I2CBus: Linux I2C adapter index (/dev/i2c-<n>), default 1 on Raspberry Pi (used when relay_output_mode=mcp23017).
	MCP23017I2CBus int
	// MCP23017I2CAddr: 7-bit MCP23017 address, default 0x20 (decimal 32).
	MCP23017I2CAddr uint8
	// XL9535I2CBus / XL9535I2CAddr: used when relay_output_mode=xl9535 (defaults match MCP23017).
	XL9535I2CBus  int
	XL9535I2CAddr uint8

	DoorRelayPin         uint8
	DoorRelayActiveLow   bool
	BuzzerRelayPin       uint8
	BuzzerRelayActiveLow bool
	DoorSensorPin        uint8
	HeartbeatLEDPin      uint8
	// ExitButtonPin: free-egress input (mode access_entry_with_exit_button). 0 = disabled.
	ExitButtonPin       uint8
	ExitButtonActiveLow bool // true = contact pulls to ground when pressed
	// EntryButtonPin: inside entry request (mode access_exit_with_entry_button). 0 = disabled.
	EntryButtonPin       uint8
	EntryButtonActiveLow bool
	// ElevatorDispatchRelayPin: single shared dispatch relay when not using per-floor pins; 0 = use door relay in elevator modes.
	ElevatorDispatchRelayPin  uint8
	ElevatorDispatchActiveLow bool
	// ElevatorEnableRelayPin: elevator_wait_floor_buttons legacy single relay when elevator_wait_floor_enable_pins is empty—restores ground (or common) for all allowed cab buttons together. 0 = skip.
	ElevatorEnableRelayPin  uint8
	ElevatorEnableActiveLow bool
	// ElevatorFloorDispatchPins: per-floor dispatch outputs (pulse floor call). elevator_wait_floor_buttons + cab sense: one per elevator_floor_input_pins; + cab ignore: one per wait-floor enable channel. elevator_predefined_floor: at most one entry when no cab inputs; empty = use elevator_dispatch_relay_pin / door.
	ElevatorFloorDispatchPins string
	// ElevatorWaitFloorEnablePins: elevator_wait_floor_buttons only—comma relay pins that reconnect ground to each cab floor button; with cab sense count must match elevator_floor_input_pins; with cab ignore count is the number of enabled floors (no cab inputs). Empty = use ElevatorEnableRelayPin only.
	ElevatorWaitFloorEnablePins string
	// ElevatorPredefinedEnablePins: elevator_predefined_floor only—at most one relay that pulses to simulate the single floor call (buttons removed at panel). Not used in wait-floor mode.
	ElevatorPredefinedEnablePins string
}

// AppContext holds our global connections and configurations
type AppContext struct {
	DB           *sql.DB
	MQTTClient   mqtt.Client
	mqttMu       sync.RWMutex // serializes reconnect vs publish/handler client reads
	Config       DeviceConfig
	configMu     sync.RWMutex // protects Config, GPIOSettings, TechMenuPrompt for reload/set/save
	GPIO         *GPIOManager // nil if rpio failed to open (e.g. not on a Pi)
	GPIOSettings GPIOSettings
	// TechMenuPrompt is the technician /dev/tty status-line label, shown as "{TechMenuPrompt}> " before input.
	TechMenuPrompt string
	// ConfigPath is the JSON path from -config; used for cfg save/reload from the technician menu.
	ConfigPath string
	// PinDisplayDigits receives how many PIN digits are entered; displayController prints that many asterisks.
	PinDisplayDigits chan int

	pinFailMu  sync.Mutex
	pinFailSeq int // consecutive rejected PIN submissions (reset on success or after buzzer fires)

	techHistMu sync.Mutex
	techHist   []string // technician /dev/tty commands, oldest first (capped by TechMenuHistoryMax)

	keypadLockoutMu         sync.Mutex
	keypadLockoutUntil      time.Time   // zero = no active lockout
	keypadLockoutEndTimer   *time.Timer // wall-clock end of lockout period
	keypadLockoutEndLogOnce *sync.Once  // ensures single WARNING when lockout period ends

	elevatorMu                   sync.Mutex
	elevatorGrantUntil           time.Time // non-zero: waiting for cab floor button (elevator_wait_floor_buttons)
	elevatorGrantStartedAt       time.Time // when the current grant began; used for cab-sense arming/debounce
	elevatorCabFloorDebounceHeld []int     // pressed indices seen while accumulating elevatorCabFloorDebounceTick
	elevatorCabFloorDebounceTick int       // consecutive 50ms polls with same held snapshot
	elevatorGrantPIN             string    // credential used for current wait-floor grant (for DB floor ACL)
	elevatorGrantViaFallback     bool

	// Dual USB keypad: in-memory occupancy per PIN (entry adds, exit subtracts). Reset on process restart.
	occupancyMu    sync.Mutex
	occupancyByPIN map[string]int

	// elevatorFloorDispatchPins: parsed from GPIOSettings.ElevatorFloorDispatchPins; len matches cab floor inputs when non-empty.
	elevatorFloorDispatchPins []uint8
	// elevatorPredefinedEnablePins: parsed from GPIOSettings.ElevatorPredefinedEnablePins; at most one pin in predefined mode.
	elevatorPredefinedEnablePins []uint8
	// elevatorWaitFloorEnablePins: parsed from GPIOSettings.ElevatorWaitFloorEnablePins; len matches cab floor inputs when set.
	elevatorWaitFloorEnablePins []uint8
	// elevatorParameterModesDoc: optional JSON subtree from elevator_parameter_modes; documentation only, preserved on cfg save.
	elevatorParameterModesDoc json.RawMessage
}

// logEmitMinSeverity: emit log lines whose severity is >= this (0=DEBUG all, 1=INFO+, 2=WARNING+, 3=ERROR+, 4=CRITICAL only).
var logEmitMinSeverity atomic.Int32

// DeviceConfig represents configurable parameters (loaded from SQLite/Central Server)
type DeviceConfig struct {
	HeartbeatInterval time.Duration
	// DoorOpenWarningAfter: if the door stays open longer than this, a warning is logged once until it closes again.
	DoorOpenWarningAfter time.Duration
	// DoorSensorClosedIsLow: when true, a low GPIO level means the door is closed (e.g. switch to GND when closed, often with pull-up).
	// When false, a high level means closed (open when the pin reads low).
	DoorSensorClosedIsLow bool
	SoundCardName         string // ALSA device passed to aplay -D (e.g. plughw:1,0); empty = default card
	// WAV paths played via aplay; missing files are skipped with a debug log.
	SoundStartup   string
	SoundShutdown  string
	SoundPinOK     string
	SoundPinReject string
	SoundKeypress  string
	LogLevel       string
	// PinLength is how many digit keys must be entered before the PIN is submitted automatically.
	// If zero, the user must press Enter (or KPENTER) to submit.
	PinLength int
	// RelayPulseDuration is how long the door relay stays energized after a valid PIN.
	RelayPulseDuration time.Duration
	// PinRejectBuzzerAfterAttempts: after this many consecutive wrong PINs, pulse the buzzer relay (GPIO in GPIOSettings). Zero disables the buzzer.
	PinRejectBuzzerAfterAttempts int
	// BuzzerRelayPulseDuration is how long the buzzer relay stays on when the wrong-PIN threshold is reached.
	BuzzerRelayPulseDuration time.Duration

	// MQTT remote control (subscribe on MQTTCommandTopic). Set MQTTEnabled false or leave MQTTBroker empty to skip MQTT.
	MQTTEnabled      bool
	MQTTBroker       string
	MQTTClientID     string
	MQTTUsername     string
	MQTTPassword     string
	MQTTCommandTopic string
	MQTTStatusTopic  string // publish JSON acks/status; empty to skip publish
	MQTTCommandToken string // optional shared secret; JSON field "token" must match when set
	// TechMenuHistoryMax caps remembered /dev/tty menu lines for Up/Down recall. Zero defaults to 100; max 10000.
	TechMenuHistoryMax int

	// KeypadInterDigitTimeout: max pause between keystrokes before PIN buffer clears (clamped 3s–10s, default 5s).
	KeypadInterDigitTimeout time.Duration
	// KeypadSessionTimeout: max time from first digit until submit/clear (clamped 10s–60s, default 30s).
	KeypadSessionTimeout time.Duration
	// PinEntryFeedbackDelay: wait after PIN OK/reject sound before accepting new keys (clamped 2s–10s, default 3s).
	PinEntryFeedbackDelay time.Duration
	// PinLockoutEnabled: when false, keypad lockout is never armed and any active lockout is cleared.
	PinLockoutEnabled bool
	// PinLockoutAfterAttempts: consecutive wrong PINs before keypad lockout (0=off, else clamped 3–5, default 5).
	PinLockoutAfterAttempts int
	// PinLockoutDuration: keypad ignores input for this long after lockout triggers (clamped 30s–300s, default 60s).
	PinLockoutDuration time.Duration
	// PinLockoutOverridePin: if non-empty, submitting this PIN clears keypad lockout and wrong-PIN streak without opening the door.
	PinLockoutOverridePin string
	// FallbackAccessPin: if non-empty, accepted when no matching enabled row exists in access_pins (legacy fallback).
	FallbackAccessPin string

	// WebhookEvent*: POST JSON to WebhookEventURL on door/PIN/MQTT events when WebhookEventEnabled and URL is set.
	// When WebhookEventTokenEnabled, sends Authorization: Bearer <WebhookEventToken>.
	WebhookEventEnabled      bool
	WebhookEventURL          string
	WebhookEventTokenEnabled bool
	WebhookEventToken        string
	// WebhookHeartbeat*: POST JSON on each heartbeat tick (same interval as heartbeat_interval) when enabled and URL set.
	WebhookHeartbeatEnabled      bool
	WebhookHeartbeatURL          string
	WebhookHeartbeatTokenEnabled bool
	WebhookHeartbeatToken        string

	// KeypadOperationMode: ModeAccess* / ModeElevator* (see ModeElevatorWaitFloorButtons / ModeElevatorPredefinedFloor comments).
	KeypadOperationMode string
	// KeypadEvdevPath: Linux evdev device for primary USB keypad (default /dev/input/event1).
	KeypadEvdevPath string
	// KeypadExitEvdevPath: second keypad for access_dual_usb_keypad (must differ from KeypadEvdevPath).
	KeypadExitEvdevPath string
	// PairPeerRole: none | entry | exit — used with access_paired_remote_exit and MQTTPairPeerTopic.
	PairPeerRole string
	// MQTTPairPeerTopic: entry unit publishes unlock hint; exit unit subscribes and pulses door.
	MQTTPairPeerTopic string
	PairPeerToken     string
	// ElevatorFloorWaitTimeout: elevator_wait_floor_buttons — after valid PIN, how long enable relays stay on; with cab sense, also the window to read elevator_floor_input_pins.
	ElevatorFloorWaitTimeout time.Duration
	// ElevatorWaitFloorCabSense: elevator_wait_floor_buttons — sense (default): configure and read elevator_floor_input_pins, log selection, pulse dispatch. ignore: leave elevator_floor_input_pins empty; no cab GPIO; timeout only clears enables.
	ElevatorWaitFloorCabSense string
	// ElevatorFloorInputPins: BCM inputs wired to in-cab floor buttons (active low when pressed); used when elevator_wait_floor_cab_sense is sense (default).
	ElevatorFloorInputPins string
	// ElevatorPredefinedFloor: in elevator_predefined_floor, index into ElevatorPredefinedFloors when that list has one entry (usually 0); else legacy logical floor label for logs only.
	ElevatorPredefinedFloor int
	// ElevatorPredefinedFloors: at most one logical floor label for elevator_predefined_floor; must match gpio.elevator_predefined_enable_pins when that list is set. Empty = legacy single-floor (dispatch index from ElevatorPredefinedFloor only).
	ElevatorPredefinedFloors []int
	// ElevatorDispatchPulseDuration: default pulse for elevator dispatch (single relay or per-floor pad).
	ElevatorDispatchPulseDuration time.Duration
	// ElevatorFloorDispatchPulseDurations: per-index pulse lengths when gpio.elevator_floor_dispatch_pins is set (order matches cab inputs in wait-floor mode, or predefined floors when no cab inputs); shorter lists pad with ElevatorDispatchPulseDuration.
	ElevatorFloorDispatchPulseDurations []time.Duration
	// ElevatorEnablePulseDuration: elevator_predefined_floor: pulse length for elevator_predefined_enable_0 when >0 (ignored for elevator_wait_floor_buttons; wait enables stay on until floor selected or elevator_floor_wait_timeout).
	ElevatorEnablePulseDuration time.Duration
	// DualKeypadRejectExitWithoutEntry: in access_dual_usb_keypad, valid PIN on exit with no matching entry count rejects (no door pulse); default false warns only.
	DualKeypadRejectExitWithoutEntry bool

	// AccessControlDoorID: logical door (access_doors.id). When set with access_schedule_enforce, PIN must match an enabled access level and time window for this door (direct or via door group).
	AccessControlDoorID string
	// AccessControlElevatorID: logical elevator (access_elevators.id). When set with access_schedule_enforce and keypad is in an elevator mode, PIN must match an enabled access level and time window for this elevator (direct or via elevator group).
	AccessControlElevatorID string
	// AccessScheduleEnforce: when true (default), apply SQLite access_levels/access_time_windows when the configured door or elevator has targets.
	AccessScheduleEnforce bool
	// AccessScheduleApplyToFallbackPin: when true, device.fallback_access_pin is subject to schedules; default false (emergency bypass).
	AccessScheduleApplyToFallbackPin bool
}

// virtualkeyz2JSON is the on-disk shape of virtualkeyz2.json (see default file in repo).
type virtualkeyz2JSON struct {
	Device                 virtualkeyz2DeviceJSON `json:"device"`
	GPIO                   virtualkeyz2GPIOJSON   `json:"gpio"`
	TechMenuPrompt         *string                `json:"tech_menu_prompt"`
	ElevatorParameterModes json.RawMessage        `json:"elevator_parameter_modes,omitempty"`
}

type virtualkeyz2DeviceJSON struct {
	HeartbeatInterval                   *string `json:"heartbeat_interval"`
	DoorOpenWarningAfter                *string `json:"door_open_warning_after"`
	DoorSensorClosedIsLow               *bool   `json:"door_sensor_closed_is_low"`
	SoundCardName                       *string `json:"sound_card_name"`
	SoundStartup                        *string `json:"sound_startup"`
	SoundShutdown                       *string `json:"sound_shutdown"`
	SoundPinOK                          *string `json:"sound_pin_ok"`
	SoundPinReject                      *string `json:"sound_pin_reject"`
	SoundKeypress                       *string `json:"sound_keypress"`
	LogLevel                            *string `json:"log_level"`
	PinLength                           *int    `json:"pin_length"`
	RelayPulseDuration                  *string `json:"relay_pulse_duration"`
	PinRejectBuzzerAfterAttempts        *int    `json:"pin_reject_buzzer_after_attempts"`
	BuzzerRelayPulseDuration            *string `json:"buzzer_relay_pulse_duration"`
	MQTTEnabled                         *bool   `json:"mqtt_enabled"`
	MQTTBroker                          *string `json:"mqtt_broker"`
	MQTTClientID                        *string `json:"mqtt_client_id"`
	MQTTUsername                        *string `json:"mqtt_username"`
	MQTTPassword                        *string `json:"mqtt_password"`
	MQTTCommandTopic                    *string `json:"mqtt_command_topic"`
	MQTTStatusTopic                     *string `json:"mqtt_status_topic"`
	MQTTCommandToken                    *string `json:"mqtt_command_token"`
	TechMenuHistoryMax                  *int    `json:"tech_menu_history_max"`
	KeypadInterDigitTimeout             *string `json:"keypad_inter_digit_timeout"`
	KeypadSessionTimeout                *string `json:"keypad_session_timeout"`
	PinEntryFeedbackDelay               *string `json:"pin_entry_feedback_delay"`
	PinLockoutEnabled                   *bool   `json:"pin_lockout_enabled"`
	PinLockoutAfterAttempts             *int    `json:"pin_lockout_after_attempts"`
	PinLockoutDuration                  *string `json:"pin_lockout_duration"`
	PinLockoutOverridePin               *string `json:"pin_lockout_override_pin"`
	FallbackAccessPin                   *string `json:"fallback_access_pin"`
	WebhookEventEnabled                 *bool   `json:"webhook_event_enabled"`
	WebhookEventURL                     *string `json:"webhook_event_url"`
	WebhookEventTokenEnabled            *bool   `json:"webhook_event_token_enabled"`
	WebhookEventToken                   *string `json:"webhook_event_token"`
	WebhookHeartbeatEnabled             *bool   `json:"webhook_heartbeat_enabled"`
	WebhookHeartbeatURL                 *string `json:"webhook_heartbeat_url"`
	WebhookHeartbeatTokenEnabled        *bool   `json:"webhook_heartbeat_token_enabled"`
	WebhookHeartbeatToken               *string `json:"webhook_heartbeat_token"`
	KeypadOperationMode                 *string `json:"keypad_operation_mode"`
	KeypadEvdevPath                     *string `json:"keypad_evdev_path"`
	KeypadExitEvdevPath                 *string `json:"keypad_exit_evdev_path"`
	PairPeerRole                        *string `json:"pair_peer_role"`
	MQTTPairPeerTopic                   *string `json:"mqtt_pair_peer_topic"`
	PairPeerToken                       *string `json:"pair_peer_token"`
	ElevatorFloorWaitTimeout            *string `json:"elevator_floor_wait_timeout"`
	ElevatorWaitFloorCabSense           *string `json:"elevator_wait_floor_cab_sense"`
	ElevatorFloorInputPins              *string `json:"elevator_floor_input_pins"`
	ElevatorPredefinedFloor             *int    `json:"elevator_predefined_floor"`
	ElevatorPredefinedFloors            *string `json:"elevator_predefined_floors"`
	ElevatorDispatchPulseDuration       *string `json:"elevator_dispatch_pulse_duration"`
	ElevatorFloorDispatchPulseDurations *string `json:"elevator_floor_dispatch_pulse_durations"`
	ElevatorEnablePulseDuration         *string `json:"elevator_enable_pulse_duration"`
	DualKeypadRejectExitWithoutEntry    *bool   `json:"dual_keypad_reject_exit_without_entry"`
	AccessControlDoorID                 *string `json:"access_control_door_id,omitempty"`
	AccessControlElevatorID             *string `json:"access_control_elevator_id,omitempty"`
	AccessScheduleEnforce               *bool   `json:"access_schedule_enforce,omitempty"`
	AccessScheduleApplyToFallbackPin      *bool   `json:"access_schedule_apply_to_fallback_pin,omitempty"`
}

type virtualkeyz2GPIOJSON struct {
	RelayOutputMode              *string `json:"relay_output_mode"`
	MCP23017I2CBus               *int    `json:"mcp23017_i2c_bus"`
	MCP23017I2CAddr              *int    `json:"mcp23017_i2c_addr"`
	XL9535I2CBus                 *int    `json:"xl9535_i2c_bus"`
	XL9535I2CAddr                *int    `json:"xl9535_i2c_addr"`
	DoorRelayPin                 *int    `json:"door_relay_pin"`
	DoorRelayActiveLow           *bool   `json:"door_relay_active_low"`
	BuzzerRelayPin               *int    `json:"buzzer_relay_pin"`
	BuzzerRelayActiveLow         *bool   `json:"buzzer_relay_active_low"`
	DoorSensorPin                *int    `json:"door_sensor_pin"`
	HeartbeatLEDPin              *int    `json:"heartbeat_led_pin"`
	ExitButtonPin                *int    `json:"exit_button_pin"`
	ExitButtonActiveLow          *bool   `json:"exit_button_active_low"`
	EntryButtonPin               *int    `json:"entry_button_pin"`
	EntryButtonActiveLow         *bool   `json:"entry_button_active_low"`
	ElevatorDispatchRelayPin     *int    `json:"elevator_dispatch_relay_pin"`
	ElevatorDispatchActiveLow    *bool   `json:"elevator_dispatch_active_low"`
	ElevatorEnableRelayPin       *int    `json:"elevator_enable_relay_pin"`
	ElevatorEnableActiveLow      *bool   `json:"elevator_enable_active_low"`
	ElevatorFloorDispatchPins    *string `json:"elevator_floor_dispatch_pins"`
	ElevatorPredefinedEnablePins *string `json:"elevator_predefined_enable_pins"`
	ElevatorWaitFloorEnablePins  *string `json:"elevator_wait_floor_enable_pins"`
}

func bcmUint8(field string, v int) (uint8, error) {
	if v < 0 || v > 40 {
		return 0, fmt.Errorf("gpio.%s: BCM pin %d out of range 0-40", field, v)
	}
	return uint8(v), nil
}

func normalizeRelayOutputMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", RelayOutputGPIO, "direct", "bcm":
		return RelayOutputGPIO
	case RelayOutputMCP23017, "mcp":
		return RelayOutputMCP23017
	case RelayOutputXL9535, "xinluda":
		return RelayOutputXL9535
	case "i2c":
		return RelayOutputMCP23017
	default:
		return RelayOutputGPIO
	}
}

func isRelayOutputI2CExpander(mode string) bool {
	switch normalizeRelayOutputMode(mode) {
	case RelayOutputMCP23017, RelayOutputXL9535:
		return true
	default:
		return false
	}
}

func relayPinUint8(field string, v int, relayMode string) (uint8, error) {
	if isRelayOutputI2CExpander(relayMode) {
		if v < 0 || v > 15 {
			return 0, fmt.Errorf("gpio.%s: I2C relay expander pin %d out of range 0-15", field, v)
		}
		return uint8(v), nil
	}
	return bcmUint8(field, v)
}

func normalizeGPIORelaySettings(g *GPIOSettings) {
	g.RelayOutputMode = normalizeRelayOutputMode(g.RelayOutputMode)
	if g.MCP23017I2CBus <= 0 {
		g.MCP23017I2CBus = 1
	}
	if g.MCP23017I2CAddr == 0 {
		g.MCP23017I2CAddr = 0x20
	}
	if g.XL9535I2CBus <= 0 {
		g.XL9535I2CBus = 1
	}
	if g.XL9535I2CAddr == 0 {
		g.XL9535I2CAddr = 0x20
	}
}

func elevatorCabFloorCount(s string) int {
	pins, err := parseBCMPinList(s)
	if err != nil {
		return 0
	}
	return len(pins)
}

func parseRelayPinUint8List(field, s string, relayMode string) ([]uint8, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	var out []uint8
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("gpio.%s: invalid integer %q: %w", field, p, err)
		}
		u, err := relayPinUint8(field, n, relayMode)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func parseCommaDurationList(section, field, s string) ([]time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	var out []time.Duration
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		d, err := time.ParseDuration(p)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: invalid duration %q: %w", section, field, p, err)
		}
		out = append(out, d)
	}
	return out, nil
}

func elevatorFloorDispatchOutputName(i int) string {
	return fmt.Sprintf("elevator_floor_dispatch_%d", i)
}

func elevatorPredefinedEnableOutputName(i int) string {
	return fmt.Sprintf("elevator_predefined_enable_%d", i)
}

func elevatorWaitFloorEnableOutputName(i int) string {
	return fmt.Sprintf("elevator_wait_floor_enable_%d", i)
}

func parseCommaIntList(section, field, s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	var out []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: invalid integer %q: %w", section, field, p, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func formatIntList(nums []int) string {
	if len(nums) == 0 {
		return ""
	}
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

func syncElevatorFloorDispatchPulseDurations(app *AppContext) {
	nDisp := len(app.elevatorFloorDispatchPins)
	if nDisp == 0 {
		app.Config.ElevatorFloorDispatchPulseDurations = nil
		return
	}
	def := app.Config.ElevatorDispatchPulseDuration
	if def <= 0 {
		def = 400 * time.Millisecond
	}
	src := app.Config.ElevatorFloorDispatchPulseDurations
	out := make([]time.Duration, nDisp)
	for i := 0; i < nDisp; i++ {
		if i < len(src) && src[i] > 0 {
			out[i] = clampDuration(src[i], 50*time.Millisecond, 60*time.Second)
		} else {
			out[i] = clampDuration(def, 50*time.Millisecond, 60*time.Second)
		}
	}
	app.Config.ElevatorFloorDispatchPulseDurations = out
}

func validateElevatorFloorDispatchLayout(app *AppContext) error {
	nDisp := len(app.elevatorFloorDispatchPins)
	if nDisp == 0 {
		return nil
	}
	mode := NormalizeKeypadOperationMode(app.Config.KeypadOperationMode)
	if mode == ModeElevatorWaitFloorButtons && !elevatorWaitFloorSenseCabInputs(app.Config) {
		nEn := elevatorWaitFloorEnableChannelCount(app)
		if nEn == 0 {
			return fmt.Errorf("gpio.elevator_floor_dispatch_pins: set gpio.elevator_wait_floor_enable_pins or gpio.elevator_enable_relay_pin before using per-floor dispatch when device.elevator_wait_floor_cab_sense is ignore")
		}
		if nDisp != nEn {
			return fmt.Errorf("gpio.elevator_floor_dispatch_pins: %d entries must match %d wait-floor enable channel(s) when device.elevator_wait_floor_cab_sense is ignore", nDisp, nEn)
		}
		return nil
	}
	nCab := elevatorCabFloorCount(app.Config.ElevatorFloorInputPins)
	nPre := len(app.Config.ElevatorPredefinedFloors)
	if nCab > 0 {
		if nDisp != nCab {
			return fmt.Errorf("gpio.elevator_floor_dispatch_pins: %d entries must match elevator_floor_input_pins (%d)", nDisp, nCab)
		}
		return nil
	}
	if nPre > 0 {
		if nDisp != nPre {
			return fmt.Errorf("gpio.elevator_floor_dispatch_pins: %d entries must match elevator_predefined_floors (%d) when there are no cab input pins", nDisp, nPre)
		}
		return nil
	}
	if mode == ModeElevatorPredefinedFloor && nDisp <= 1 {
		return nil
	}
	return fmt.Errorf("gpio.elevator_floor_dispatch_pins: %d entries require matching elevator_floor_input_pins, or (no cab inputs) matching elevator_predefined_floors count (%d), or at most one pin in elevator_predefined_floor when predefined floors list is empty", nDisp, nPre)
}

func validateElevatorPredefinedFloorsLayout(app *AppContext) error {
	nF := len(app.Config.ElevatorPredefinedFloors)
	nE := len(app.elevatorPredefinedEnablePins)
	if nF > 1 {
		return fmt.Errorf("device.elevator_predefined_floors: at most one floor in elevator_predefined_floor mode")
	}
	if nE > 1 {
		return fmt.Errorf("gpio.elevator_predefined_enable_pins: at most one relay in elevator_predefined_floor mode")
	}
	if nF == 0 && nE == 0 {
		return nil
	}
	if nF != nE {
		return fmt.Errorf("device.elevator_predefined_floors (%d values) must match gpio.elevator_predefined_enable_pins (%d)", nF, nE)
	}
	nDisp := len(app.elevatorFloorDispatchPins)
	nCab := elevatorCabFloorCount(app.Config.ElevatorFloorInputPins)
	if nDisp > 0 && nF > 0 && nCab == 0 && nDisp != nF {
		return fmt.Errorf("gpio.elevator_floor_dispatch_pins (%d) must match elevator_predefined_floors (%d) when there are no cab input pins", nDisp, nF)
	}
	if nF == 1 && nCab == 0 && nDisp > 1 {
		return fmt.Errorf("gpio.elevator_floor_dispatch_pins: at most one entry when device.elevator_predefined_floors has one floor and there are no cab inputs")
	}
	return nil
}

func validateElevatorWaitFloorEnableLayout(app *AppContext) error {
	if len(app.elevatorPredefinedEnablePins) > 0 {
		return fmt.Errorf("gpio.elevator_predefined_enable_pins is only for elevator_predefined_floor; clear it or set gpio.elevator_wait_floor_enable_pins for per-floor ground-return relays")
	}
	nW := len(app.elevatorWaitFloorEnablePins)
	if elevatorWaitFloorSenseCabInputs(app.Config) {
		if nW == 0 {
			return nil
		}
		if app.GPIOSettings.ElevatorEnableRelayPin != 0 {
			return fmt.Errorf("use either gpio.elevator_wait_floor_enable_pins or gpio.elevator_enable_relay_pin, not both")
		}
		nCab := elevatorCabFloorCount(app.Config.ElevatorFloorInputPins)
		if nCab == 0 {
			return fmt.Errorf("gpio.elevator_wait_floor_enable_pins requires device.elevator_floor_input_pins (same entry count) when device.elevator_wait_floor_cab_sense is sense")
		}
		if nW != nCab {
			return fmt.Errorf("gpio.elevator_wait_floor_enable_pins: %d entries must match elevator_floor_input_pins (%d)", nW, nCab)
		}
		return nil
	}
	// Cab sense ignore: no BCM cab inputs; enable channel count comes only from relay lists.
	if strings.TrimSpace(app.Config.ElevatorFloorInputPins) != "" {
		return fmt.Errorf("device.elevator_floor_input_pins must be empty when device.elevator_wait_floor_cab_sense is ignore")
	}
	if nW > 0 {
		if app.GPIOSettings.ElevatorEnableRelayPin != 0 {
			return fmt.Errorf("use either gpio.elevator_wait_floor_enable_pins or gpio.elevator_enable_relay_pin, not both")
		}
		return nil
	}
	if app.GPIOSettings.ElevatorEnableRelayPin == 0 {
		return fmt.Errorf("device.elevator_wait_floor_cab_sense ignore: set gpio.elevator_wait_floor_enable_pins or gpio.elevator_enable_relay_pin")
	}
	return nil
}

func dispatchPulseDurationForFloor(cfg DeviceConfig, idx int) time.Duration {
	if idx >= 0 && idx < len(cfg.ElevatorFloorDispatchPulseDurations) {
		return cfg.ElevatorFloorDispatchPulseDurations[idx]
	}
	return cfg.ElevatorDispatchPulseDuration
}

func formatDurationList(ds []time.Duration) string {
	if len(ds) == 0 {
		return ""
	}
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = d.String()
	}
	return strings.Join(parts, ",")
}

func applyJSONDuration(dst *time.Duration, section, field string, s *string) error {
	if s == nil {
		return nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(*s))
	if err != nil {
		return fmt.Errorf("%s.%s: invalid duration %q: %w", section, field, *s, err)
	}
	*dst = d
	return nil
}

func clampDuration(d, minD, maxD time.Duration) time.Duration {
	if d < minD {
		return minD
	}
	if d > maxD {
		return maxD
	}
	return d
}

// normalizeKeypadAndPinUX applies defaults and allowed ranges for keypad / PIN timing and lockout.
func normalizeKeypadAndPinUX(c *DeviceConfig) {
	if c.KeypadInterDigitTimeout <= 0 {
		c.KeypadInterDigitTimeout = 5 * time.Second
	} else {
		c.KeypadInterDigitTimeout = clampDuration(c.KeypadInterDigitTimeout, 3*time.Second, 10*time.Second)
	}
	if c.KeypadSessionTimeout <= 0 {
		c.KeypadSessionTimeout = 30 * time.Second
	} else {
		c.KeypadSessionTimeout = clampDuration(c.KeypadSessionTimeout, 10*time.Second, 60*time.Second)
	}
	if c.PinEntryFeedbackDelay <= 0 {
		c.PinEntryFeedbackDelay = 3 * time.Second
	} else {
		c.PinEntryFeedbackDelay = clampDuration(c.PinEntryFeedbackDelay, 2*time.Second, 10*time.Second)
	}
	if c.PinLockoutDuration <= 0 {
		c.PinLockoutDuration = 60 * time.Second
	} else {
		c.PinLockoutDuration = clampDuration(c.PinLockoutDuration, 30*time.Second, 300*time.Second)
	}
	if c.PinLockoutAfterAttempts < 0 {
		c.PinLockoutAfterAttempts = 0
	} else if c.PinLockoutAfterAttempts > 0 && c.PinLockoutAfterAttempts < 3 {
		c.PinLockoutAfterAttempts = 3
	} else if c.PinLockoutAfterAttempts > 5 {
		c.PinLockoutAfterAttempts = 5
	}
	normalizeOperationModeConfig(c)
}

func normalizeOperationModeConfig(c *DeviceConfig) {
	c.KeypadOperationMode = NormalizeKeypadOperationMode(c.KeypadOperationMode)
	if isElevatorWaitFloorMode(c.KeypadOperationMode) {
		c.ElevatorWaitFloorCabSense = normalizeElevatorWaitFloorCabSense(c.ElevatorWaitFloorCabSense)
	} else {
		c.ElevatorWaitFloorCabSense = ""
	}
	c.PairPeerRole = normalizePairPeerRole(c.PairPeerRole)
	if strings.TrimSpace(c.KeypadEvdevPath) == "" {
		c.KeypadEvdevPath = "/dev/input/event1"
	}
	if c.ElevatorFloorWaitTimeout <= 0 {
		c.ElevatorFloorWaitTimeout = 60 * time.Second
	} else {
		c.ElevatorFloorWaitTimeout = clampDuration(c.ElevatorFloorWaitTimeout, 5*time.Second, 600*time.Second)
	}
	if c.ElevatorDispatchPulseDuration <= 0 {
		c.ElevatorDispatchPulseDuration = c.RelayPulseDuration
	}
	if c.ElevatorDispatchPulseDuration <= 0 {
		c.ElevatorDispatchPulseDuration = 400 * time.Millisecond
	} else {
		c.ElevatorDispatchPulseDuration = clampDuration(c.ElevatorDispatchPulseDuration, 50*time.Millisecond, 60*time.Second)
	}
	if n := len(c.ElevatorPredefinedFloors); n > 0 {
		if c.ElevatorPredefinedFloor < 0 {
			c.ElevatorPredefinedFloor = 0
		} else if c.ElevatorPredefinedFloor >= n {
			c.ElevatorPredefinedFloor = n - 1
		}
	} else {
		if c.ElevatorPredefinedFloor < 0 {
			c.ElevatorPredefinedFloor = 0
		}
		if c.ElevatorPredefinedFloor > 255 {
			c.ElevatorPredefinedFloor = 255
		}
	}
	if c.ElevatorEnablePulseDuration < 0 {
		c.ElevatorEnablePulseDuration = 0
	} else if c.ElevatorEnablePulseDuration > 0 {
		c.ElevatorEnablePulseDuration = clampDuration(c.ElevatorEnablePulseDuration, 50*time.Millisecond, 60*time.Second)
	}
}

// applyVirtualKeyz2JSON merges raw into app (partial JSON keys override). Caller must hold app.configMu when used concurrently.
func applyVirtualKeyz2JSON(app *AppContext, raw *virtualkeyz2JSON) error {
	d := &raw.Device
	if err := applyJSONDuration(&app.Config.HeartbeatInterval, "device", "heartbeat_interval", d.HeartbeatInterval); err != nil {
		return err
	}
	if err := applyJSONDuration(&app.Config.DoorOpenWarningAfter, "device", "door_open_warning_after", d.DoorOpenWarningAfter); err != nil {
		return err
	}
	if err := applyJSONDuration(&app.Config.RelayPulseDuration, "device", "relay_pulse_duration", d.RelayPulseDuration); err != nil {
		return err
	}
	if err := applyJSONDuration(&app.Config.BuzzerRelayPulseDuration, "device", "buzzer_relay_pulse_duration", d.BuzzerRelayPulseDuration); err != nil {
		return err
	}
	if err := applyJSONDuration(&app.Config.KeypadInterDigitTimeout, "device", "keypad_inter_digit_timeout", d.KeypadInterDigitTimeout); err != nil {
		return err
	}
	if err := applyJSONDuration(&app.Config.KeypadSessionTimeout, "device", "keypad_session_timeout", d.KeypadSessionTimeout); err != nil {
		return err
	}
	if err := applyJSONDuration(&app.Config.PinEntryFeedbackDelay, "device", "pin_entry_feedback_delay", d.PinEntryFeedbackDelay); err != nil {
		return err
	}
	if err := applyJSONDuration(&app.Config.PinLockoutDuration, "device", "pin_lockout_duration", d.PinLockoutDuration); err != nil {
		return err
	}
	if d.PinLockoutEnabled != nil {
		app.Config.PinLockoutEnabled = *d.PinLockoutEnabled
	}
	if d.DoorSensorClosedIsLow != nil {
		app.Config.DoorSensorClosedIsLow = *d.DoorSensorClosedIsLow
	}
	if d.SoundCardName != nil {
		app.Config.SoundCardName = *d.SoundCardName
	}
	if d.SoundStartup != nil {
		app.Config.SoundStartup = *d.SoundStartup
	}
	if d.SoundShutdown != nil {
		app.Config.SoundShutdown = *d.SoundShutdown
	}
	if d.SoundPinOK != nil {
		app.Config.SoundPinOK = *d.SoundPinOK
	}
	if d.SoundPinReject != nil {
		app.Config.SoundPinReject = *d.SoundPinReject
	}
	if d.SoundKeypress != nil {
		app.Config.SoundKeypress = *d.SoundKeypress
	}
	if d.LogLevel != nil {
		app.Config.LogLevel = *d.LogLevel
	}
	if d.PinLength != nil {
		app.Config.PinLength = *d.PinLength
	}
	if d.PinRejectBuzzerAfterAttempts != nil {
		app.Config.PinRejectBuzzerAfterAttempts = *d.PinRejectBuzzerAfterAttempts
	}
	if d.MQTTEnabled != nil {
		app.Config.MQTTEnabled = *d.MQTTEnabled
	}
	if d.MQTTBroker != nil {
		app.Config.MQTTBroker = *d.MQTTBroker
	}
	if d.MQTTClientID != nil {
		app.Config.MQTTClientID = *d.MQTTClientID
	}
	if d.MQTTUsername != nil {
		app.Config.MQTTUsername = *d.MQTTUsername
	}
	if d.MQTTPassword != nil {
		app.Config.MQTTPassword = *d.MQTTPassword
	}
	if d.MQTTCommandTopic != nil {
		app.Config.MQTTCommandTopic = *d.MQTTCommandTopic
	}
	if d.MQTTStatusTopic != nil {
		app.Config.MQTTStatusTopic = *d.MQTTStatusTopic
	}
	if d.MQTTCommandToken != nil {
		app.Config.MQTTCommandToken = *d.MQTTCommandToken
	}
	if d.TechMenuHistoryMax != nil {
		app.Config.TechMenuHistoryMax = *d.TechMenuHistoryMax
	}
	if d.PinLockoutAfterAttempts != nil {
		app.Config.PinLockoutAfterAttempts = *d.PinLockoutAfterAttempts
	}
	if d.PinLockoutOverridePin != nil {
		app.Config.PinLockoutOverridePin = *d.PinLockoutOverridePin
	}
	if d.FallbackAccessPin != nil {
		app.Config.FallbackAccessPin = *d.FallbackAccessPin
	}
	if d.WebhookEventEnabled != nil {
		app.Config.WebhookEventEnabled = *d.WebhookEventEnabled
	}
	if d.WebhookEventURL != nil {
		app.Config.WebhookEventURL = *d.WebhookEventURL
	}
	if d.WebhookEventTokenEnabled != nil {
		app.Config.WebhookEventTokenEnabled = *d.WebhookEventTokenEnabled
	}
	if d.WebhookEventToken != nil {
		app.Config.WebhookEventToken = *d.WebhookEventToken
	}
	if d.WebhookHeartbeatEnabled != nil {
		app.Config.WebhookHeartbeatEnabled = *d.WebhookHeartbeatEnabled
	}
	if d.WebhookHeartbeatURL != nil {
		app.Config.WebhookHeartbeatURL = *d.WebhookHeartbeatURL
	}
	if d.WebhookHeartbeatTokenEnabled != nil {
		app.Config.WebhookHeartbeatTokenEnabled = *d.WebhookHeartbeatTokenEnabled
	}
	if d.WebhookHeartbeatToken != nil {
		app.Config.WebhookHeartbeatToken = *d.WebhookHeartbeatToken
	}
	if err := applyJSONDuration(&app.Config.ElevatorFloorWaitTimeout, "device", "elevator_floor_wait_timeout", d.ElevatorFloorWaitTimeout); err != nil {
		return err
	}
	if d.ElevatorWaitFloorCabSense != nil {
		app.Config.ElevatorWaitFloorCabSense = strings.TrimSpace(*d.ElevatorWaitFloorCabSense)
	}
	if err := applyJSONDuration(&app.Config.ElevatorDispatchPulseDuration, "device", "elevator_dispatch_pulse_duration", d.ElevatorDispatchPulseDuration); err != nil {
		return err
	}
	if d.ElevatorEnablePulseDuration != nil {
		ev := strings.TrimSpace(*d.ElevatorEnablePulseDuration)
		if ev == "" {
			app.Config.ElevatorEnablePulseDuration = 0
		} else if err := applyJSONDuration(&app.Config.ElevatorEnablePulseDuration, "device", "elevator_enable_pulse_duration", d.ElevatorEnablePulseDuration); err != nil {
			return err
		}
	}
	if d.ElevatorFloorDispatchPulseDurations != nil {
		ds, err := parseCommaDurationList("device", "elevator_floor_dispatch_pulse_durations", *d.ElevatorFloorDispatchPulseDurations)
		if err != nil {
			return err
		}
		app.Config.ElevatorFloorDispatchPulseDurations = ds
	}
	if d.KeypadOperationMode != nil {
		app.Config.KeypadOperationMode = *d.KeypadOperationMode
	}
	if d.KeypadEvdevPath != nil {
		app.Config.KeypadEvdevPath = *d.KeypadEvdevPath
	}
	if d.KeypadExitEvdevPath != nil {
		app.Config.KeypadExitEvdevPath = *d.KeypadExitEvdevPath
	}
	if d.PairPeerRole != nil {
		app.Config.PairPeerRole = *d.PairPeerRole
	}
	if d.MQTTPairPeerTopic != nil {
		app.Config.MQTTPairPeerTopic = *d.MQTTPairPeerTopic
	}
	if d.PairPeerToken != nil {
		app.Config.PairPeerToken = *d.PairPeerToken
	}
	if d.ElevatorFloorInputPins != nil {
		app.Config.ElevatorFloorInputPins = *d.ElevatorFloorInputPins
	}
	if d.ElevatorPredefinedFloor != nil {
		app.Config.ElevatorPredefinedFloor = *d.ElevatorPredefinedFloor
	}
	if d.ElevatorPredefinedFloors != nil {
		fl, err := parseCommaIntList("device", "elevator_predefined_floors", *d.ElevatorPredefinedFloors)
		if err != nil {
			return err
		}
		app.Config.ElevatorPredefinedFloors = fl
	}
	if d.DualKeypadRejectExitWithoutEntry != nil {
		app.Config.DualKeypadRejectExitWithoutEntry = *d.DualKeypadRejectExitWithoutEntry
	}
	if d.AccessControlDoorID != nil {
		app.Config.AccessControlDoorID = strings.TrimSpace(*d.AccessControlDoorID)
	}
	if d.AccessControlElevatorID != nil {
		app.Config.AccessControlElevatorID = strings.TrimSpace(*d.AccessControlElevatorID)
	}
	if d.AccessScheduleEnforce != nil {
		app.Config.AccessScheduleEnforce = *d.AccessScheduleEnforce
	}
	if d.AccessScheduleApplyToFallbackPin != nil {
		app.Config.AccessScheduleApplyToFallbackPin = *d.AccessScheduleApplyToFallbackPin
	}
	g := &raw.GPIO
	if g.RelayOutputMode != nil {
		app.GPIOSettings.RelayOutputMode = strings.TrimSpace(*g.RelayOutputMode)
	}
	if g.MCP23017I2CBus != nil {
		app.GPIOSettings.MCP23017I2CBus = *g.MCP23017I2CBus
	}
	if g.MCP23017I2CAddr != nil {
		a := *g.MCP23017I2CAddr
		if a < 0 || a > 255 {
			return fmt.Errorf("gpio.mcp23017_i2c_addr: %d out of range 0-255", a)
		}
		app.GPIOSettings.MCP23017I2CAddr = uint8(a)
	}
	if g.XL9535I2CBus != nil {
		app.GPIOSettings.XL9535I2CBus = *g.XL9535I2CBus
	}
	if g.XL9535I2CAddr != nil {
		a := *g.XL9535I2CAddr
		if a < 0 || a > 255 {
			return fmt.Errorf("gpio.xl9535_i2c_addr: %d out of range 0-255", a)
		}
		app.GPIOSettings.XL9535I2CAddr = uint8(a)
	}
	normalizeGPIORelaySettings(&app.GPIOSettings)
	relayMode := normalizeRelayOutputMode(app.GPIOSettings.RelayOutputMode)

	if g.DoorRelayPin != nil {
		u, err := relayPinUint8("door_relay_pin", *g.DoorRelayPin, relayMode)
		if err != nil {
			return err
		}
		app.GPIOSettings.DoorRelayPin = u
	}
	if g.DoorRelayActiveLow != nil {
		app.GPIOSettings.DoorRelayActiveLow = *g.DoorRelayActiveLow
	}
	if g.BuzzerRelayPin != nil {
		u, err := relayPinUint8("buzzer_relay_pin", *g.BuzzerRelayPin, relayMode)
		if err != nil {
			return err
		}
		app.GPIOSettings.BuzzerRelayPin = u
	}
	if g.BuzzerRelayActiveLow != nil {
		app.GPIOSettings.BuzzerRelayActiveLow = *g.BuzzerRelayActiveLow
	}
	if g.DoorSensorPin != nil {
		u, err := bcmUint8("door_sensor_pin", *g.DoorSensorPin)
		if err != nil {
			return err
		}
		app.GPIOSettings.DoorSensorPin = u
	}
	if g.HeartbeatLEDPin != nil {
		u, err := bcmUint8("heartbeat_led_pin", *g.HeartbeatLEDPin)
		if err != nil {
			return err
		}
		app.GPIOSettings.HeartbeatLEDPin = u
	}
	if g.ExitButtonPin != nil {
		u, err := bcmUint8("exit_button_pin", *g.ExitButtonPin)
		if err != nil {
			return err
		}
		app.GPIOSettings.ExitButtonPin = u
	}
	if g.ExitButtonActiveLow != nil {
		app.GPIOSettings.ExitButtonActiveLow = *g.ExitButtonActiveLow
	}
	if g.EntryButtonPin != nil {
		u, err := bcmUint8("entry_button_pin", *g.EntryButtonPin)
		if err != nil {
			return err
		}
		app.GPIOSettings.EntryButtonPin = u
	}
	if g.EntryButtonActiveLow != nil {
		app.GPIOSettings.EntryButtonActiveLow = *g.EntryButtonActiveLow
	}
	if g.ElevatorDispatchRelayPin != nil {
		u, err := relayPinUint8("elevator_dispatch_relay_pin", *g.ElevatorDispatchRelayPin, relayMode)
		if err != nil {
			return err
		}
		app.GPIOSettings.ElevatorDispatchRelayPin = u
	}
	if g.ElevatorDispatchActiveLow != nil {
		app.GPIOSettings.ElevatorDispatchActiveLow = *g.ElevatorDispatchActiveLow
	}
	if g.ElevatorEnableRelayPin != nil {
		u, err := relayPinUint8("elevator_enable_relay_pin", *g.ElevatorEnableRelayPin, relayMode)
		if err != nil {
			return err
		}
		app.GPIOSettings.ElevatorEnableRelayPin = u
	}
	if g.ElevatorEnableActiveLow != nil {
		app.GPIOSettings.ElevatorEnableActiveLow = *g.ElevatorEnableActiveLow
	}
	if g.ElevatorFloorDispatchPins != nil {
		app.GPIOSettings.ElevatorFloorDispatchPins = strings.TrimSpace(*g.ElevatorFloorDispatchPins)
	}
	efPins, err := parseRelayPinUint8List("elevator_floor_dispatch_pins", app.GPIOSettings.ElevatorFloorDispatchPins, relayMode)
	if err != nil {
		return err
	}
	app.elevatorFloorDispatchPins = efPins
	if g.ElevatorPredefinedEnablePins != nil {
		app.GPIOSettings.ElevatorPredefinedEnablePins = strings.TrimSpace(*g.ElevatorPredefinedEnablePins)
	}
	preEn, err := parseRelayPinUint8List("elevator_predefined_enable_pins", app.GPIOSettings.ElevatorPredefinedEnablePins, relayMode)
	if err != nil {
		return err
	}
	app.elevatorPredefinedEnablePins = preEn
	if g.ElevatorWaitFloorEnablePins != nil {
		app.GPIOSettings.ElevatorWaitFloorEnablePins = strings.TrimSpace(*g.ElevatorWaitFloorEnablePins)
	}
	wfe, err := parseRelayPinUint8List("elevator_wait_floor_enable_pins", app.GPIOSettings.ElevatorWaitFloorEnablePins, relayMode)
	if err != nil {
		return err
	}
	app.elevatorWaitFloorEnablePins = wfe
	if raw.TechMenuPrompt != nil {
		app.TechMenuPrompt = *raw.TechMenuPrompt
	}
	if len(raw.ElevatorParameterModes) > 0 {
		app.elevatorParameterModesDoc = append(json.RawMessage(nil), raw.ElevatorParameterModes...)
	} else {
		app.elevatorParameterModesDoc = nil
	}
	normalizeKeypadAndPinUX(&app.Config)
	syncElevatorFloorDispatchPulseDurations(app)
	if err := validateElevatorConfigsForMode(app); err != nil {
		return err
	}
	return nil
}

func validateElevatorConfigsForMode(app *AppContext) error {
	if err := validateElevatorFloorDispatchLayout(app); err != nil {
		return err
	}
	mode := NormalizeKeypadOperationMode(app.Config.KeypadOperationMode)
	if mode == ModeElevatorPredefinedFloor {
		if len(app.elevatorWaitFloorEnablePins) > 0 {
			return fmt.Errorf("gpio.elevator_wait_floor_enable_pins applies only to elevator_wait_floor_buttons")
		}
		if err := validateElevatorPredefinedFloorsLayout(app); err != nil {
			return err
		}
	}
	if mode == ModeElevatorWaitFloorButtons {
		if err := validateElevatorWaitFloorEnableLayout(app); err != nil {
			return err
		}
	}
	return nil
}

// loadVirtualKeyz2Config reads path and merges into app. Missing file is ignored; parse errors are fatal to the caller.
func loadVirtualKeyz2Config(path string, app *AppContext) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Printf("INFO: Config file %q not found; using built-in defaults.", path)
			return nil
		}
		return err
	}
	var raw virtualkeyz2JSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("config %q: %w", path, err)
	}
	if err := applyVirtualKeyz2JSON(app, &raw); err != nil {
		return err
	}
	log.Printf("INFO: Loaded configuration from %q", path)
	return nil
}

// virtualkeyz2PersistFile is the full JSON document written by cfg save.
type virtualkeyz2PersistFile struct {
	Device                 virtualkeyz2PersistDevice `json:"device"`
	GPIO                   virtualkeyz2PersistGPIO   `json:"gpio"`
	TechMenuPrompt         string                    `json:"tech_menu_prompt"`
	ElevatorParameterModes json.RawMessage           `json:"elevator_parameter_modes,omitempty"`
}

type virtualkeyz2PersistDevice struct {
	HeartbeatInterval                   string `json:"heartbeat_interval"`
	DoorOpenWarningAfter                string `json:"door_open_warning_after"`
	DoorSensorClosedIsLow               bool   `json:"door_sensor_closed_is_low"`
	SoundCardName                       string `json:"sound_card_name"`
	SoundStartup                        string `json:"sound_startup"`
	SoundShutdown                       string `json:"sound_shutdown"`
	SoundPinOK                          string `json:"sound_pin_ok"`
	SoundPinReject                      string `json:"sound_pin_reject"`
	SoundKeypress                       string `json:"sound_keypress"`
	LogLevel                            string `json:"log_level"`
	PinLength                           int    `json:"pin_length"`
	RelayPulseDuration                  string `json:"relay_pulse_duration"`
	PinRejectBuzzerAfterAttempts        int    `json:"pin_reject_buzzer_after_attempts"`
	BuzzerRelayPulseDuration            string `json:"buzzer_relay_pulse_duration"`
	MQTTEnabled                         bool   `json:"mqtt_enabled"`
	MQTTBroker                          string `json:"mqtt_broker"`
	MQTTClientID                        string `json:"mqtt_client_id"`
	MQTTUsername                        string `json:"mqtt_username"`
	MQTTPassword                        string `json:"mqtt_password"`
	MQTTCommandTopic                    string `json:"mqtt_command_topic"`
	MQTTStatusTopic                     string `json:"mqtt_status_topic"`
	MQTTCommandToken                    string `json:"mqtt_command_token"`
	TechMenuHistoryMax                  int    `json:"tech_menu_history_max"`
	KeypadInterDigitTimeout             string `json:"keypad_inter_digit_timeout"`
	KeypadSessionTimeout                string `json:"keypad_session_timeout"`
	PinEntryFeedbackDelay               string `json:"pin_entry_feedback_delay"`
	PinLockoutEnabled                   bool   `json:"pin_lockout_enabled"`
	PinLockoutAfterAttempts             int    `json:"pin_lockout_after_attempts"`
	PinLockoutDuration                  string `json:"pin_lockout_duration"`
	PinLockoutOverridePin               string `json:"pin_lockout_override_pin"`
	FallbackAccessPin                   string `json:"fallback_access_pin"`
	WebhookEventEnabled                 bool   `json:"webhook_event_enabled"`
	WebhookEventURL                     string `json:"webhook_event_url"`
	WebhookEventTokenEnabled            bool   `json:"webhook_event_token_enabled"`
	WebhookEventToken                   string `json:"webhook_event_token"`
	WebhookHeartbeatEnabled             bool   `json:"webhook_heartbeat_enabled"`
	WebhookHeartbeatURL                 string `json:"webhook_heartbeat_url"`
	WebhookHeartbeatTokenEnabled        bool   `json:"webhook_heartbeat_token_enabled"`
	WebhookHeartbeatToken               string `json:"webhook_heartbeat_token"`
	KeypadOperationMode                 string `json:"keypad_operation_mode"`
	KeypadEvdevPath                     string `json:"keypad_evdev_path"`
	KeypadExitEvdevPath                 string `json:"keypad_exit_evdev_path"`
	PairPeerRole                        string `json:"pair_peer_role"`
	MQTTPairPeerTopic                   string `json:"mqtt_pair_peer_topic"`
	PairPeerToken                       string `json:"pair_peer_token"`
	ElevatorFloorWaitTimeout            string `json:"elevator_floor_wait_timeout"`
	ElevatorWaitFloorCabSense           string `json:"elevator_wait_floor_cab_sense,omitempty"`
	ElevatorFloorInputPins              string `json:"elevator_floor_input_pins"`
	ElevatorPredefinedFloor             int    `json:"elevator_predefined_floor"`
	ElevatorPredefinedFloors            string `json:"elevator_predefined_floors"`
	ElevatorDispatchPulseDuration       string `json:"elevator_dispatch_pulse_duration"`
	ElevatorFloorDispatchPulseDurations string `json:"elevator_floor_dispatch_pulse_durations"`
	ElevatorEnablePulseDuration         string `json:"elevator_enable_pulse_duration"`
	DualKeypadRejectExitWithoutEntry    bool   `json:"dual_keypad_reject_exit_without_entry"`
	AccessControlDoorID              string `json:"access_control_door_id,omitempty"`
	AccessControlElevatorID          string `json:"access_control_elevator_id,omitempty"`
	AccessScheduleEnforce            bool   `json:"access_schedule_enforce"`
	AccessScheduleApplyToFallbackPin bool   `json:"access_schedule_apply_to_fallback_pin"`
}

type virtualkeyz2PersistGPIO struct {
	RelayOutputMode              string `json:"relay_output_mode"`
	MCP23017I2CBus               int    `json:"mcp23017_i2c_bus"`
	MCP23017I2CAddr              int    `json:"mcp23017_i2c_addr"`
	XL9535I2CBus                 int    `json:"xl9535_i2c_bus"`
	XL9535I2CAddr                int    `json:"xl9535_i2c_addr"`
	DoorRelayPin                 int    `json:"door_relay_pin"`
	DoorRelayActiveLow           bool   `json:"door_relay_active_low"`
	BuzzerRelayPin               int    `json:"buzzer_relay_pin"`
	BuzzerRelayActiveLow         bool   `json:"buzzer_relay_active_low"`
	DoorSensorPin                int    `json:"door_sensor_pin"`
	HeartbeatLEDPin              int    `json:"heartbeat_led_pin"`
	ExitButtonPin                int    `json:"exit_button_pin"`
	ExitButtonActiveLow          bool   `json:"exit_button_active_low"`
	EntryButtonPin               int    `json:"entry_button_pin"`
	EntryButtonActiveLow         bool   `json:"entry_button_active_low"`
	ElevatorDispatchRelayPin     int    `json:"elevator_dispatch_relay_pin"`
	ElevatorDispatchActiveLow    bool   `json:"elevator_dispatch_active_low"`
	ElevatorEnableRelayPin       int    `json:"elevator_enable_relay_pin"`
	ElevatorEnableActiveLow      bool   `json:"elevator_enable_active_low"`
	ElevatorFloorDispatchPins    string `json:"elevator_floor_dispatch_pins"`
	ElevatorPredefinedEnablePins string `json:"elevator_predefined_enable_pins"`
	ElevatorWaitFloorEnablePins  string `json:"elevator_wait_floor_enable_pins"`
}

func buildPersistFile(app *AppContext) virtualkeyz2PersistFile {
	app.configMu.RLock()
	defer app.configMu.RUnlock()
	c := app.Config
	g := app.GPIOSettings
	var out virtualkeyz2PersistFile
	out.TechMenuPrompt = app.TechMenuPrompt
	out.Device.HeartbeatInterval = c.HeartbeatInterval.String()
	out.Device.DoorOpenWarningAfter = c.DoorOpenWarningAfter.String()
	out.Device.DoorSensorClosedIsLow = c.DoorSensorClosedIsLow
	out.Device.SoundCardName = c.SoundCardName
	out.Device.SoundStartup = c.SoundStartup
	out.Device.SoundShutdown = c.SoundShutdown
	out.Device.SoundPinOK = c.SoundPinOK
	out.Device.SoundPinReject = c.SoundPinReject
	out.Device.SoundKeypress = c.SoundKeypress
	out.Device.LogLevel = c.LogLevel
	out.Device.PinLength = c.PinLength
	out.Device.RelayPulseDuration = c.RelayPulseDuration.String()
	out.Device.PinRejectBuzzerAfterAttempts = c.PinRejectBuzzerAfterAttempts
	out.Device.BuzzerRelayPulseDuration = c.BuzzerRelayPulseDuration.String()
	out.Device.MQTTEnabled = c.MQTTEnabled
	out.Device.MQTTBroker = c.MQTTBroker
	out.Device.MQTTClientID = c.MQTTClientID
	out.Device.MQTTUsername = c.MQTTUsername
	out.Device.MQTTPassword = c.MQTTPassword
	out.Device.MQTTCommandTopic = c.MQTTCommandTopic
	out.Device.MQTTStatusTopic = c.MQTTStatusTopic
	out.Device.MQTTCommandToken = c.MQTTCommandToken
	out.Device.TechMenuHistoryMax = c.TechMenuHistoryMax
	out.Device.KeypadInterDigitTimeout = c.KeypadInterDigitTimeout.String()
	out.Device.KeypadSessionTimeout = c.KeypadSessionTimeout.String()
	out.Device.PinEntryFeedbackDelay = c.PinEntryFeedbackDelay.String()
	out.Device.PinLockoutEnabled = c.PinLockoutEnabled
	out.Device.PinLockoutAfterAttempts = c.PinLockoutAfterAttempts
	out.Device.PinLockoutDuration = c.PinLockoutDuration.String()
	out.Device.PinLockoutOverridePin = c.PinLockoutOverridePin
	out.Device.FallbackAccessPin = c.FallbackAccessPin
	out.Device.WebhookEventEnabled = c.WebhookEventEnabled
	out.Device.WebhookEventURL = c.WebhookEventURL
	out.Device.WebhookEventTokenEnabled = c.WebhookEventTokenEnabled
	out.Device.WebhookEventToken = c.WebhookEventToken
	out.Device.WebhookHeartbeatEnabled = c.WebhookHeartbeatEnabled
	out.Device.WebhookHeartbeatURL = c.WebhookHeartbeatURL
	out.Device.WebhookHeartbeatTokenEnabled = c.WebhookHeartbeatTokenEnabled
	out.Device.WebhookHeartbeatToken = c.WebhookHeartbeatToken
	out.Device.KeypadOperationMode = c.KeypadOperationMode
	out.Device.KeypadEvdevPath = c.KeypadEvdevPath
	out.Device.KeypadExitEvdevPath = c.KeypadExitEvdevPath
	out.Device.PairPeerRole = c.PairPeerRole
	out.Device.MQTTPairPeerTopic = c.MQTTPairPeerTopic
	out.Device.PairPeerToken = c.PairPeerToken
	out.Device.ElevatorFloorWaitTimeout = c.ElevatorFloorWaitTimeout.String()
	if isElevatorWaitFloorMode(NormalizeKeypadOperationMode(c.KeypadOperationMode)) {
		if normalizeElevatorWaitFloorCabSense(c.ElevatorWaitFloorCabSense) == ElevatorWaitFloorCabSenseIgnore {
			out.Device.ElevatorWaitFloorCabSense = ElevatorWaitFloorCabSenseIgnore
		} else {
			out.Device.ElevatorWaitFloorCabSense = ElevatorWaitFloorCabSenseSense
		}
	}
	out.Device.ElevatorFloorInputPins = c.ElevatorFloorInputPins
	out.Device.ElevatorPredefinedFloor = c.ElevatorPredefinedFloor
	out.Device.ElevatorPredefinedFloors = formatIntList(c.ElevatorPredefinedFloors)
	out.Device.ElevatorDispatchPulseDuration = c.ElevatorDispatchPulseDuration.String()
	out.Device.ElevatorFloorDispatchPulseDurations = formatDurationList(c.ElevatorFloorDispatchPulseDurations)
	if c.ElevatorEnablePulseDuration > 0 {
		out.Device.ElevatorEnablePulseDuration = c.ElevatorEnablePulseDuration.String()
	} else {
		out.Device.ElevatorEnablePulseDuration = ""
	}
	out.Device.DualKeypadRejectExitWithoutEntry = c.DualKeypadRejectExitWithoutEntry
	out.Device.AccessControlDoorID = c.AccessControlDoorID
	out.Device.AccessControlElevatorID = c.AccessControlElevatorID
	out.Device.AccessScheduleEnforce = c.AccessScheduleEnforce
	out.Device.AccessScheduleApplyToFallbackPin = c.AccessScheduleApplyToFallbackPin
	out.GPIO.RelayOutputMode = normalizeRelayOutputMode(g.RelayOutputMode)
	out.GPIO.MCP23017I2CBus = g.MCP23017I2CBus
	out.GPIO.MCP23017I2CAddr = int(g.MCP23017I2CAddr)
	out.GPIO.XL9535I2CBus = g.XL9535I2CBus
	out.GPIO.XL9535I2CAddr = int(g.XL9535I2CAddr)
	out.GPIO.DoorRelayPin = int(g.DoorRelayPin)
	out.GPIO.DoorRelayActiveLow = g.DoorRelayActiveLow
	out.GPIO.BuzzerRelayPin = int(g.BuzzerRelayPin)
	out.GPIO.BuzzerRelayActiveLow = g.BuzzerRelayActiveLow
	out.GPIO.DoorSensorPin = int(g.DoorSensorPin)
	out.GPIO.HeartbeatLEDPin = int(g.HeartbeatLEDPin)
	out.GPIO.ExitButtonPin = int(g.ExitButtonPin)
	out.GPIO.ExitButtonActiveLow = g.ExitButtonActiveLow
	out.GPIO.EntryButtonPin = int(g.EntryButtonPin)
	out.GPIO.EntryButtonActiveLow = g.EntryButtonActiveLow
	out.GPIO.ElevatorDispatchRelayPin = int(g.ElevatorDispatchRelayPin)
	out.GPIO.ElevatorDispatchActiveLow = g.ElevatorDispatchActiveLow
	out.GPIO.ElevatorEnableRelayPin = int(g.ElevatorEnableRelayPin)
	out.GPIO.ElevatorEnableActiveLow = g.ElevatorEnableActiveLow
	out.GPIO.ElevatorFloorDispatchPins = g.ElevatorFloorDispatchPins
	out.GPIO.ElevatorPredefinedEnablePins = g.ElevatorPredefinedEnablePins
	out.GPIO.ElevatorWaitFloorEnablePins = g.ElevatorWaitFloorEnablePins
	if len(app.elevatorParameterModesDoc) > 0 {
		out.ElevatorParameterModes = app.elevatorParameterModesDoc
	}
	return out
}

func saveVirtualKeyz2Config(app *AppContext) error {
	path := strings.TrimSpace(app.ConfigPath)
	if path == "" {
		path = "virtualkeyz2.json"
	}
	doc := buildPersistFile(app)
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func syncLogFilterFromConfigLevel(level string) {
	logEmitMinSeverity.Store(parseLogLevelMin(level))
}

func parseLogLevelMin(level string) int32 {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "all", "debug":
		return 0
	case "info":
		return 1
	case "warning", "warn":
		return 2
	case "error":
		return 3
	case "critical":
		return 4
	default:
		return 0
	}
}

// lineLogSeverity returns 0 DEBUG .. 4 CRITICAL; unknown lines treated as INFO (1).
func lineLogSeverity(line []byte) int32 {
	s := string(line)
	switch {
	case strings.Contains(s, "CRITICAL:"):
		return 4
	case strings.Contains(s, "ERROR:"):
		return 3
	case strings.Contains(s, "WARNING:"):
		return 2
	case strings.Contains(s, "DEBUG:"):
		return 0
	case strings.Contains(s, "INFO:"):
		return 1
	default:
		return 1
	}
}

func shouldEmitLogLine(line []byte) bool {
	min := logEmitMinSeverity.Load()
	return lineLogSeverity(line) >= min
}

func (ctx *AppContext) reconnectMQTT() {
	ctx.mqttMu.Lock()
	old := ctx.MQTTClient
	ctx.MQTTClient = nil
	ctx.mqttMu.Unlock()
	if old != nil {
		old.Disconnect(250)
	}
	c := initMQTT(ctx)
	ctx.mqttMu.Lock()
	ctx.MQTTClient = c
	ctx.mqttMu.Unlock()
}

// reloadVirtualKeyz2ConfigLive reads ConfigPath from disk, applies settings, updates log filter, tech prompt, and MQTT.
func reloadVirtualKeyz2ConfigLive(ctx *AppContext) error {
	path := strings.TrimSpace(ctx.ConfigPath)
	if path == "" {
		path = "virtualkeyz2.json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw virtualkeyz2JSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("parse JSON: %w", err)
	}
	ctx.configMu.Lock()
	if err := applyVirtualKeyz2JSON(ctx, &raw); err != nil {
		ctx.configMu.Unlock()
		return err
	}
	lvl := ctx.Config.LogLevel
	prompt := ctx.TechMenuPrompt
	ctx.configMu.Unlock()
	registerTechMenuPrompt(prompt)
	syncLogFilterFromConfigLevel(lvl)
	log.Println("INFO: Configuration reloaded from disk (MQTT reconnecting; GPIO / relay_output_mode changes need a full restart).")
	ctx.reconnectMQTT()
	ctx.techHistoryTrimToMax()
	return nil
}

func effectiveConfigPath(ctx *AppContext) string {
	p := strings.TrimSpace(ctx.ConfigPath)
	if p == "" {
		return "virtualkeyz2.json"
	}
	return p
}

// applyInMemoryConfigLive reapplies current in-memory settings: log filter, tech prompt, MQTT reconnect.
func applyInMemoryConfigLive(ctx *AppContext) {
	ctx.configMu.RLock()
	prompt := ctx.TechMenuPrompt
	lvl := ctx.Config.LogLevel
	ctx.configMu.RUnlock()
	registerTechMenuPrompt(prompt)
	syncLogFilterFromConfigLevel(lvl)
	log.Println("INFO: In-memory configuration applied live (MQTT reconnect; GPIO pin map unchanged until reboot).")
	ctx.reconnectMQTT()
}

func techMenuHandleCfg(ctx *AppContext, line string, parts []string) {
	if len(parts) < 2 {
		techMenuSyncPrint(func(w io.Writer) { techMenuCfgKeysHelp(w) })
		return
	}
	sub := strings.ToLower(parts[1])
	switch sub {
	case "keys", "help", "h", "?":
		techMenuSyncPrint(func(w io.Writer) { techMenuCfgKeysHelp(w) })
	case "list", "show", "l":
		techMenuSyncPrint(func(w io.Writer) { techMenuShowConfig(w, ctx) })
		log.Println("INFO: Technician menu: cfg list (full configuration).")
	case "save", "write":
		if err := saveVirtualKeyz2Config(ctx); err != nil {
			log.Printf("WARNING: cfg save: %v", err)
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "cfg save failed: %v\n", err) })
		} else {
			p := effectiveConfigPath(ctx)
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Configuration saved to %q\n", p) })
			log.Printf("INFO: Technician menu: configuration saved to %q", p)
		}
	case "reload", "reread":
		if err := reloadVirtualKeyz2ConfigLive(ctx); err != nil {
			log.Printf("WARNING: cfg reload: %v", err)
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "cfg reload failed: %v\n", err) })
		} else {
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Reloaded from disk and applied live.") })
		}
	case "apply", "live":
		applyInMemoryConfigLive(ctx)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "In-memory settings applied live (log level, prompt, MQTT).") })
	case "history":
		if len(parts) >= 3 && strings.ToLower(parts[2]) == "clear" {
			ctx.techHistoryClear()
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Command history cleared.") })
			log.Println("INFO: Technician menu: command history cleared (cfg history clear).")
		} else {
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Usage: cfg history clear") })
		}
	case "set":
		key, val, ok := techMenuExtractCfgSetValue(line)
		if !ok {
			techMenuSyncPrint(func(w io.Writer) {
				fmt.Fprintln(w, "Usage: cfg set <key> <value>")
				fmt.Fprintln(w, "Example: cfg set log_level info")
			})
			return
		}
		if err := techMenuCfgSetValue(ctx, key, val); err != nil {
			log.Printf("WARNING: cfg set: %v", err)
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "cfg set failed: %v\n", err) })
			return
		}
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Set %q OK. Use \"cfg apply\" for MQTT/log live refresh, or \"cfg save\" to persist.\n", key)
		})
		log.Printf("INFO: Technician menu: cfg set %q", key)
	default:
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Unknown cfg subcommand %q. Try: cfg keys\n", parts[1])
		})
	}
}

func techMenuCfgSetValue(ctx *AppContext, key, value string) error {
	key = strings.ToLower(strings.TrimSpace(key))
	value = strings.TrimSpace(value)
	trimHistoryAfter := false
	ctx.configMu.Lock()
	defer func() {
		ctx.configMu.Unlock()
		if trimHistoryAfter {
			ctx.techHistoryTrimToMax()
		}
	}()
	var err error
	switch key {
	case "heartbeat_interval":
		err = applyJSONDuration(&ctx.Config.HeartbeatInterval, "device", "heartbeat_interval", &value)
	case "door_open_warning_after":
		err = applyJSONDuration(&ctx.Config.DoorOpenWarningAfter, "device", "door_open_warning_after", &value)
	case "relay_pulse_duration":
		err = applyJSONDuration(&ctx.Config.RelayPulseDuration, "device", "relay_pulse_duration", &value)
	case "buzzer_relay_pulse_duration":
		err = applyJSONDuration(&ctx.Config.BuzzerRelayPulseDuration, "device", "buzzer_relay_pulse_duration", &value)
	case "door_sensor_closed_is_low":
		ctx.Config.DoorSensorClosedIsLow, err = strconv.ParseBool(value)
	case "sound_card_name":
		ctx.Config.SoundCardName = value
	case "sound_startup":
		ctx.Config.SoundStartup = value
	case "sound_shutdown":
		ctx.Config.SoundShutdown = value
	case "sound_pin_ok":
		ctx.Config.SoundPinOK = value
	case "sound_pin_reject":
		ctx.Config.SoundPinReject = value
	case "sound_keypress":
		ctx.Config.SoundKeypress = value
	case "log_level":
		ctx.Config.LogLevel = value
	case "pin_length":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.Config.PinLength = int(n)
		}
	case "pin_reject_buzzer_after_attempts":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.Config.PinRejectBuzzerAfterAttempts = int(n)
		}
	case "mqtt_enabled":
		ctx.Config.MQTTEnabled, err = strconv.ParseBool(value)
	case "mqtt_broker":
		ctx.Config.MQTTBroker = value
	case "mqtt_client_id":
		ctx.Config.MQTTClientID = value
	case "mqtt_username":
		ctx.Config.MQTTUsername = value
	case "mqtt_password":
		ctx.Config.MQTTPassword = value
	case "mqtt_command_topic":
		ctx.Config.MQTTCommandTopic = value
	case "mqtt_status_topic":
		ctx.Config.MQTTStatusTopic = value
	case "mqtt_command_token":
		ctx.Config.MQTTCommandToken = value
	case "tech_menu_history_max":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.Config.TechMenuHistoryMax = int(n)
			trimHistoryAfter = true
		}
	case "relay_output_mode":
		ctx.GPIOSettings.RelayOutputMode = value
		normalizeGPIORelaySettings(&ctx.GPIOSettings)
	case "mcp23017_i2c_bus":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.GPIOSettings.MCP23017I2CBus = int(n)
			normalizeGPIORelaySettings(&ctx.GPIOSettings)
		}
	case "mcp23017_i2c_addr":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			if n < 0 || n > 255 {
				err = fmt.Errorf("mcp23017_i2c_addr %d out of range 0-255", n)
			} else {
				ctx.GPIOSettings.MCP23017I2CAddr = uint8(n)
				normalizeGPIORelaySettings(&ctx.GPIOSettings)
			}
		}
	case "xl9535_i2c_bus":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.GPIOSettings.XL9535I2CBus = int(n)
			normalizeGPIORelaySettings(&ctx.GPIOSettings)
		}
	case "xl9535_i2c_addr":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			if n < 0 || n > 255 {
				err = fmt.Errorf("xl9535_i2c_addr %d out of range 0-255", n)
			} else {
				ctx.GPIOSettings.XL9535I2CAddr = uint8(n)
				normalizeGPIORelaySettings(&ctx.GPIOSettings)
			}
		}
	case "door_relay_pin":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
			ctx.GPIOSettings.DoorRelayPin, err = relayPinUint8("door_relay_pin", int(n), mode)
		}
	case "door_relay_active_low":
		ctx.GPIOSettings.DoorRelayActiveLow, err = strconv.ParseBool(value)
	case "buzzer_relay_pin":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
			ctx.GPIOSettings.BuzzerRelayPin, err = relayPinUint8("buzzer_relay_pin", int(n), mode)
		}
	case "buzzer_relay_active_low":
		ctx.GPIOSettings.BuzzerRelayActiveLow, err = strconv.ParseBool(value)
	case "door_sensor_pin":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.GPIOSettings.DoorSensorPin, err = bcmUint8("door_sensor_pin", int(n))
		}
	case "heartbeat_led_pin":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.GPIOSettings.HeartbeatLEDPin, err = bcmUint8("heartbeat_led_pin", int(n))
		}
	case "tech_menu_prompt":
		ctx.TechMenuPrompt = value
	case "keypad_inter_digit_timeout":
		err = applyJSONDuration(&ctx.Config.KeypadInterDigitTimeout, "device", "keypad_inter_digit_timeout", &value)
	case "keypad_session_timeout":
		err = applyJSONDuration(&ctx.Config.KeypadSessionTimeout, "device", "keypad_session_timeout", &value)
	case "pin_entry_feedback_delay":
		err = applyJSONDuration(&ctx.Config.PinEntryFeedbackDelay, "device", "pin_entry_feedback_delay", &value)
	case "pin_lockout_enabled":
		ctx.Config.PinLockoutEnabled, err = strconv.ParseBool(value)
	case "pin_lockout_duration":
		err = applyJSONDuration(&ctx.Config.PinLockoutDuration, "device", "pin_lockout_duration", &value)
	case "pin_lockout_after_attempts":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.Config.PinLockoutAfterAttempts = int(n)
		}
	case "pin_lockout_override_pin":
		ctx.Config.PinLockoutOverridePin = value
	case "fallback_access_pin":
		ctx.Config.FallbackAccessPin = value
	case "webhook_event_enabled":
		ctx.Config.WebhookEventEnabled, err = strconv.ParseBool(value)
	case "webhook_event_url":
		ctx.Config.WebhookEventURL = value
	case "webhook_event_token_enabled":
		ctx.Config.WebhookEventTokenEnabled, err = strconv.ParseBool(value)
	case "webhook_event_token":
		ctx.Config.WebhookEventToken = value
	case "webhook_heartbeat_enabled":
		ctx.Config.WebhookHeartbeatEnabled, err = strconv.ParseBool(value)
	case "webhook_heartbeat_url":
		ctx.Config.WebhookHeartbeatURL = value
	case "webhook_heartbeat_token_enabled":
		ctx.Config.WebhookHeartbeatTokenEnabled, err = strconv.ParseBool(value)
	case "webhook_heartbeat_token":
		ctx.Config.WebhookHeartbeatToken = value
	case "keypad_operation_mode":
		ctx.Config.KeypadOperationMode = value
	case "keypad_evdev_path":
		ctx.Config.KeypadEvdevPath = value
	case "keypad_exit_evdev_path":
		ctx.Config.KeypadExitEvdevPath = value
	case "pair_peer_role":
		ctx.Config.PairPeerRole = value
	case "mqtt_pair_peer_topic":
		ctx.Config.MQTTPairPeerTopic = value
	case "pair_peer_token":
		ctx.Config.PairPeerToken = value
	case "elevator_floor_wait_timeout":
		err = applyJSONDuration(&ctx.Config.ElevatorFloorWaitTimeout, "device", "elevator_floor_wait_timeout", &value)
	case "elevator_wait_floor_cab_sense":
		v := strings.TrimSpace(strings.ToLower(value))
		if v == "" {
			ctx.Config.ElevatorWaitFloorCabSense = ""
		} else {
			switch v {
			case "sense", "on", "true", "yes":
				ctx.Config.ElevatorWaitFloorCabSense = ElevatorWaitFloorCabSenseSense
			case "ignore", "off", "false", "no":
				ctx.Config.ElevatorWaitFloorCabSense = ElevatorWaitFloorCabSenseIgnore
			default:
				err = fmt.Errorf("elevator_wait_floor_cab_sense: use sense or ignore, got %q", value)
			}
		}
	case "elevator_floor_input_pins":
		ctx.Config.ElevatorFloorInputPins = value
	case "elevator_predefined_floor":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.Config.ElevatorPredefinedFloor = int(n)
		}
	case "elevator_predefined_floors":
		fl, perr := parseCommaIntList("device", "elevator_predefined_floors", value)
		if perr != nil {
			err = perr
		} else {
			ctx.Config.ElevatorPredefinedFloors = fl
		}
	case "elevator_dispatch_pulse_duration":
		err = applyJSONDuration(&ctx.Config.ElevatorDispatchPulseDuration, "device", "elevator_dispatch_pulse_duration", &value)
	case "elevator_floor_dispatch_pulse_durations":
		ds, perr := parseCommaDurationList("device", "elevator_floor_dispatch_pulse_durations", value)
		if perr != nil {
			err = perr
		} else {
			ctx.Config.ElevatorFloorDispatchPulseDurations = ds
		}
	case "elevator_enable_pulse_duration":
		ev := strings.TrimSpace(value)
		if ev == "" {
			ctx.Config.ElevatorEnablePulseDuration = 0
		} else {
			err = applyJSONDuration(&ctx.Config.ElevatorEnablePulseDuration, "device", "elevator_enable_pulse_duration", &value)
		}
	case "elevator_floor_dispatch_pins":
		ctx.GPIOSettings.ElevatorFloorDispatchPins = strings.TrimSpace(value)
		mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
		ctx.elevatorFloorDispatchPins, err = parseRelayPinUint8List("elevator_floor_dispatch_pins", ctx.GPIOSettings.ElevatorFloorDispatchPins, mode)
	case "elevator_predefined_enable_pins":
		ctx.GPIOSettings.ElevatorPredefinedEnablePins = strings.TrimSpace(value)
		mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
		ctx.elevatorPredefinedEnablePins, err = parseRelayPinUint8List("elevator_predefined_enable_pins", ctx.GPIOSettings.ElevatorPredefinedEnablePins, mode)
	case "elevator_wait_floor_enable_pins":
		ctx.GPIOSettings.ElevatorWaitFloorEnablePins = strings.TrimSpace(value)
		mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
		ctx.elevatorWaitFloorEnablePins, err = parseRelayPinUint8List("elevator_wait_floor_enable_pins", ctx.GPIOSettings.ElevatorWaitFloorEnablePins, mode)
	case "exit_button_pin":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.GPIOSettings.ExitButtonPin, err = bcmUint8("exit_button_pin", int(n))
		}
	case "exit_button_active_low":
		ctx.GPIOSettings.ExitButtonActiveLow, err = strconv.ParseBool(value)
	case "entry_button_pin":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.GPIOSettings.EntryButtonPin, err = bcmUint8("entry_button_pin", int(n))
		}
	case "entry_button_active_low":
		ctx.GPIOSettings.EntryButtonActiveLow, err = strconv.ParseBool(value)
	case "elevator_dispatch_relay_pin":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
			ctx.GPIOSettings.ElevatorDispatchRelayPin, err = relayPinUint8("elevator_dispatch_relay_pin", int(n), mode)
		}
	case "elevator_dispatch_active_low":
		ctx.GPIOSettings.ElevatorDispatchActiveLow, err = strconv.ParseBool(value)
	case "elevator_enable_relay_pin":
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
			ctx.GPIOSettings.ElevatorEnableRelayPin, err = relayPinUint8("elevator_enable_relay_pin", int(n), mode)
		}
	case "elevator_enable_active_low":
		ctx.GPIOSettings.ElevatorEnableActiveLow, err = strconv.ParseBool(value)
	case "dual_keypad_reject_exit_without_entry":
		ctx.Config.DualKeypadRejectExitWithoutEntry, err = strconv.ParseBool(value)
	case "access_control_door_id":
		ctx.Config.AccessControlDoorID = value
	case "access_control_elevator_id":
		ctx.Config.AccessControlElevatorID = value
	case "access_schedule_enforce":
		ctx.Config.AccessScheduleEnforce, err = strconv.ParseBool(value)
	case "access_schedule_apply_to_fallback_pin":
		ctx.Config.AccessScheduleApplyToFallbackPin, err = strconv.ParseBool(value)
	default:
		return fmt.Errorf("unknown key %q (try: cfg keys)", key)
	}
	if err != nil {
		return err
	}
	normalizeKeypadAndPinUX(&ctx.Config)
	syncElevatorFloorDispatchPulseDurations(ctx)
	if err := validateElevatorConfigsForMode(ctx); err != nil {
		return err
	}
	if key == "log_level" {
		syncLogFilterFromConfigLevel(ctx.Config.LogLevel)
	}
	return nil
}

func techMenuExtractCfgSetValue(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", "", false
	}
	if strings.ToLower(fields[0]) != "cfg" || strings.ToLower(fields[1]) != "set" {
		return "", "", false
	}
	key = strings.ToLower(fields[2])
	tail := line
	for _, w := range fields[:3] {
		i := strings.Index(strings.ToLower(tail), strings.ToLower(w))
		if i < 0 {
			return "", "", false
		}
		tail = strings.TrimSpace(tail[i+len(w):])
	}
	if tail == "" {
		return key, "", true
	}
	return key, tail, true
}

func techMenuCfgKeysHelp(w io.Writer) {
	fmt.Fprint(w, `
Settable keys (snake_case, same as virtualkeyz2.json):
  log_level                         debug | info | warning | error | critical | all (empty=all)
  heartbeat_interval                e.g. 60s
  door_open_warning_after           duration string
  door_sensor_closed_is_low         true|false
  relay_pulse_duration              e.g. 400ms
  buzzer_relay_pulse_duration       e.g. 800ms
  pin_length                        0 = Enter to submit
  pin_reject_buzzer_after_attempts  0 disables buzzer
  sound_card_name                   ALSA -D e.g. plughw:1,0
  sound_startup                     WAV path
  sound_shutdown                    WAV path
  sound_pin_ok                      WAV path
  sound_pin_reject                  WAV path
  sound_keypress                    WAV path
  mqtt_enabled                      true|false
  mqtt_broker
  mqtt_client_id
  mqtt_username
  mqtt_password
  mqtt_command_topic
  mqtt_status_topic
  mqtt_command_token
  tech_menu_history_max             default 100, max 10000
  keypad_inter_digit_timeout        3s–10s, default 5s
  keypad_session_timeout            10s–60s from first digit, default 30s
  pin_entry_feedback_delay          2s–10s after PIN sound, default 3s
  pin_lockout_enabled               true|false (false disables keypad lockout entirely)
  pin_lockout_after_attempts        0=off, else 3–5 failed PINs, default 5
  pin_lockout_duration              30s–300s keypad ignore, default 60s
  pin_lockout_override_pin          clears lockout when submitted (empty=disabled)
  fallback_access_pin               PIN accepted when no access_pins DB match (empty=disabled)
  webhook_event_enabled             true|false POST JSON on PIN/door/MQTT events
  webhook_event_url                 HTTPS/HTTP URL (empty = no event webhooks)
  webhook_event_token_enabled       true|false send Authorization: Bearer token
  webhook_event_token               secret when token enabled (empty = no header)
  webhook_heartbeat_enabled         true|false POST JSON each heartbeat_interval
  webhook_heartbeat_url             URL for heartbeat callbacks
  webhook_heartbeat_token_enabled   true|false Bearer token on heartbeat POST
  webhook_heartbeat_token           secret when heartbeat token enabled
  keypad_operation_mode             access_* modes | elevator_wait_floor_buttons (see elevator_wait_floor_cab_sense) | elevator_predefined_floor (one relay pulse simulates floor call; cab buttons not used)
  keypad_evdev_path                 e.g. /dev/input/event1
  keypad_exit_evdev_path            second keypad for access_dual_usb_keypad
  pair_peer_role                    none|entry|exit (with access_paired_remote_exit + mqtt_pair_peer_topic)
  mqtt_pair_peer_topic              exit unit subscribes; entry unit publishes after PIN
  pair_peer_token                   optional shared secret in pair JSON
  elevator_floor_wait_timeout       5s–600s enable relay hold (elevator_wait_floor_buttons); with cab sense, window to read floor inputs
  elevator_wait_floor_cab_sense     elevator_wait_floor_buttons: sense (default) or ignore — ignore = no elevator_floor_input_pins, no floor logging/dispatch from GPIO
  elevator_floor_input_pins         comma BCM cab floor sense inputs; used only when elevator_wait_floor_cab_sense is sense (default)
  elevator_predefined_floors        at most one logical floor label; must match elevator_predefined_enable_pins when set
  elevator_predefined_floor         index into elevator_predefined_floors when set; else legacy logical floor label for logs only
  elevator_dispatch_pulse_duration  default elevator dispatch pulse (single relay or pad for per-floor list)
  elevator_floor_dispatch_pulse_durations  comma durations, one per cab floor (with elevator_floor_dispatch_pins); short lists pad with elevator_dispatch_pulse_duration
  elevator_enable_pulse_duration   elevator_predefined_floor only: pulse length for predefined enable relay; wait-floor holds enables for full elevator_floor_wait_timeout (this key ignored there)
  dual_keypad_reject_exit_without_entry  true|false (dual USB: reject exit PIN if no entry recorded)
  access_control_door_id            logical door id (access_doors.id); empty = PIN-only, no schedule enforcement
  access_control_elevator_id        logical elevator id (access_elevators.id); empty = no elevator schedule; used in elevator_* keypad modes when set
  access_schedule_enforce           true|false (default true): when door/elevator id set, enforce access_levels + time windows if DB maps that target
  access_schedule_apply_to_fallback_pin  true|false (default false): subject device.fallback_access_pin to schedules
  relay_output_mode                 gpio|mcp23017|xl9535 (relays on BCM vs I2C expander; sensors/LED stay BCM)
  mcp23017_i2c_bus                  MCP23017: Linux I2C bus (default 1)
  mcp23017_i2c_addr                 MCP23017: 7-bit address, default 32 (0x20)
  xl9535_i2c_bus                    XL9535: Linux I2C bus (default 1)
  xl9535_i2c_addr                   XL9535: 7-bit address, default 32 (0x20)
  exit_button_pin                   REX GPIO (access_entry_with_exit_button)
  exit_button_active_low            true|false
  entry_button_pin                  GPIO (access_exit_with_entry_button)
  entry_button_active_low           true|false
  elevator_dispatch_relay_pin       0 = use door relay when elevator_floor_dispatch_pins empty
  elevator_dispatch_active_low      true|false
  elevator_floor_dispatch_pins      wait-floor+cab sense: one per elevator_floor_input_pins. wait-floor+cab ignore: one per wait-floor enable channel. predefined: optional single dispatch when no cab inputs (or use elevator_dispatch_relay_pin)
  elevator_wait_floor_enable_pins   wait-floor: ground-return relays; with cab sense one per elevator_floor_input_pins; with cab ignore one per enabled floor (empty = use elevator_enable_relay_pin)
  elevator_predefined_enable_pins   predefined only: at most one relay that pulses to simulate the floor call
  elevator_enable_relay_pin       wait-floor legacy: single relay for all cab floor buttons when elevator_wait_floor_enable_pins empty; not used with per-floor wait enables
  elevator_enable_active_low        true|false
  door_relay_pin                    BCM 0-40, or expander pin 0-15 if relay_output_mode=mcp23017 or xl9535
  door_relay_active_low             true|false
  buzzer_relay_pin
  buzzer_relay_active_low           true|false
  door_sensor_pin
  heartbeat_led_pin
  tech_menu_prompt

Commands:
  acl help                          SQLite access control (doors, PINs, schedules); Tab completes subcommands
  cfg keys                          this list
  cfg list                          current values (one line per parameter)
  cfg set <key> <value>             change in memory
  cfg save                          write JSON (-config path)
  cfg reload                        load JSON + live apply
  cfg history clear                 clear command history
`)
}

// WrongPINCount returns consecutive rejected PIN submissions (for technician diagnostics).
func (ctx *AppContext) WrongPINCount() int {
	ctx.pinFailMu.Lock()
	defer ctx.pinFailMu.Unlock()
	return ctx.pinFailSeq
}

// ResetWrongPINCount clears the consecutive wrong-PIN counter.
func (ctx *AppContext) ResetWrongPINCount() {
	ctx.pinFailMu.Lock()
	defer ctx.pinFailMu.Unlock()
	ctx.pinFailSeq = 0
}

func main() {
	// 1. Run Mode Configuration (Foreground vs Daemon) [cite: 8]
	daemonMode := flag.Bool("daemon", false, "Run system as a background daemon")
	noTechMenu := flag.Bool("notechmenu", false, "Disable interactive technician debug menu on /dev/tty")
	configPath := flag.String("config", "virtualkeyz2.json", "Path to JSON configuration file (optional; defaults used if missing)")
	flag.Parse()

	if *daemonMode {
		fmt.Println("Starting in Daemon Mode...")
		// In a production environment, you would handle systemd integration here
	}

	// 2. Initialize Logging Levels (Info, Debug, Warning, Critical) [cite: 9]
	initLogger()
	log.Printf("INFO: VirtualKeyz2 software build %s (release %s).", SoftwareVersion, SoftwareReleaseUTC)

	// 3. Initialize Local SQLite Database
	db := initDatabase()
	defer db.Close()

	appCtx := &AppContext{
		DB: db,
		Config: DeviceConfig{
			HeartbeatInterval:            60 * time.Second,
			DoorOpenWarningAfter:         10 * time.Second,
			DoorSensorClosedIsLow:        true,
			PinLength:                    6,
			RelayPulseDuration:           400 * time.Millisecond,
			PinRejectBuzzerAfterAttempts: 3,
			BuzzerRelayPulseDuration:     800 * time.Millisecond,
			SoundCardName:                "plughw:1,0",
			SoundStartup:                 "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/startup.wav",
			SoundShutdown:                "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/shutdown.wav",
			SoundPinOK:                   "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/pin_ok.wav",
			SoundPinReject:               "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/pin_reject.wav",
			SoundKeypress:                "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/key.wav",
			MQTTEnabled:                  true,
			MQTTBroker:                   "tcp://central-mqtt-server:1883",
			MQTTClientID:                 "virtualkeyz2-pi-001",
			MQTTCommandTopic:             "virtualkeyz2/commands",
			MQTTStatusTopic:              "virtualkeyz2/status",
			TechMenuHistoryMax:           100,
			KeypadInterDigitTimeout:      5 * time.Second,
			KeypadSessionTimeout:         30 * time.Second,
			PinEntryFeedbackDelay:        3 * time.Second,
			PinLockoutEnabled:            true,
			PinLockoutAfterAttempts:      5,
			PinLockoutDuration:           60 * time.Second,
			PinLockoutOverridePin:        "",
			FallbackAccessPin:            "",
			WebhookEventEnabled:          false,
			WebhookEventTokenEnabled:     false,
			WebhookHeartbeatEnabled:      false,
			WebhookHeartbeatTokenEnabled: false,
			AccessScheduleEnforce:        true,
			KeypadOperationMode:          ModeAccessEntry,
			KeypadEvdevPath:              "/dev/input/event1",
		},
		GPIOSettings: GPIOSettings{
			RelayOutputMode:      RelayOutputGPIO,
			MCP23017I2CBus:       1,
			MCP23017I2CAddr:      0x20,
			XL9535I2CBus:         1,
			XL9535I2CAddr:        0x20,
			DoorRelayPin:         5,
			DoorRelayActiveLow:   false,
			BuzzerRelayPin:       10,
			BuzzerRelayActiveLow: false,
			DoorSensorPin:        7,
			HeartbeatLEDPin:      26,
		},
		PinDisplayDigits: make(chan int, 16),
		TechMenuPrompt:   "MeSpace-Siam-5th-Floor-Right-Door",
	}
	if err := loadVirtualKeyz2Config(*configPath, appCtx); err != nil {
		releaseStartupLogBuffer(os.Stdout)
		log.Fatalf("CRITICAL: configuration: %v", err)
	}
	normalizeKeypadAndPinUX(&appCtx.Config)
	appCtx.ConfigPath = *configPath
	syncLogFilterFromConfigLevel(appCtx.Config.LogLevel)
	registerTechMenuPrompt(appCtx.TechMenuPrompt)
	log.Printf("INFO: Keypad operation mode: %s", NormalizeKeypadOperationMode(appCtx.Config.KeypadOperationMode))
	log.Printf("INFO: Relay output mode: %s", normalizeRelayOutputMode(appCtx.GPIOSettings.RelayOutputMode))

	// 4. Initialize Hardware IO (GPIO, Relays, Heartbeat LED) [cite: 1, 3]
	err := rpio.Open()
	if err != nil {
		log.Printf("WARNING: Cannot open GPIO (Not running on Pi?): %v", err)
	} else {
		defer rpio.Close()
		go manageHardwareHeartbeat(appCtx.GPIOSettings.HeartbeatLEDPin)
		gpio := NewGPIOManager()
		relayI2CMode := isRelayOutputI2CExpander(appCtx.GPIOSettings.RelayOutputMode)
		useI2CExpander := false
		relayOutMode := normalizeRelayOutputMode(appCtx.GPIOSettings.RelayOutputMode)
		if relayI2CMode {
			switch relayOutMode {
			case RelayOutputMCP23017:
				bus := appCtx.GPIOSettings.MCP23017I2CBus
				addr := appCtx.GPIOSettings.MCP23017I2CAddr
				mcpDev, mcpErr := mcp23017.Open(bus, addr)
				if mcpErr != nil {
					log.Printf("WARNING: MCP23017 relay backend (%s / 0x%02x): %v", fmt.Sprintf("/dev/i2c-%d", bus), addr, mcpErr)
					log.Println("WARNING: Relay outputs disabled (mcp23017 mode but expander not available; pins 0-15 are not valid BCM numbers).")
				} else {
					gpio.SetI2CRelayExpander(mcpDev)
					useI2CExpander = true
					defer mcpDev.Close()
					log.Printf("INFO: Relay outputs on MCP23017 bus %d address 0x%02x (pins 0-15 = GPA0..GPB7).", bus, addr)
				}
			case RelayOutputXL9535:
				bus := appCtx.GPIOSettings.XL9535I2CBus
				addr := appCtx.GPIOSettings.XL9535I2CAddr
				xlDev, xlErr := xl9535.Open(bus, addr)
				if xlErr != nil {
					log.Printf("WARNING: XL9535 relay backend (%s / 0x%02x): %v", fmt.Sprintf("/dev/i2c-%d", bus), addr, xlErr)
					log.Println("WARNING: Relay outputs disabled (xl9535 mode but expander not available; pins 0-15 are not valid BCM numbers).")
				} else {
					gpio.SetI2CRelayExpander(xlDev)
					useI2CExpander = true
					defer xlDev.Close()
					log.Printf("INFO: Relay outputs on XL9535 bus %d address 0x%02x (pins 0-7 = port0, 8-15 = port1).", bus, addr)
				}
			}
		}
		if !relayI2CMode || useI2CExpander {
			gpio.AddOutput("door", appCtx.GPIOSettings.DoorRelayPin, appCtx.GPIOSettings.DoorRelayActiveLow, useI2CExpander)
			gpio.AddOutput("buzzer", appCtx.GPIOSettings.BuzzerRelayPin, appCtx.GPIOSettings.BuzzerRelayActiveLow, useI2CExpander)
			if len(appCtx.elevatorFloorDispatchPins) > 0 {
				for i, pin := range appCtx.elevatorFloorDispatchPins {
					gpio.AddOutput(elevatorFloorDispatchOutputName(i), pin, appCtx.GPIOSettings.ElevatorDispatchActiveLow, useI2CExpander)
				}
			} else if appCtx.GPIOSettings.ElevatorDispatchRelayPin != 0 {
				gpio.AddOutput("elevator_dispatch", appCtx.GPIOSettings.ElevatorDispatchRelayPin, appCtx.GPIOSettings.ElevatorDispatchActiveLow, useI2CExpander)
			}
			if len(appCtx.elevatorPredefinedEnablePins) > 0 {
				for i, pin := range appCtx.elevatorPredefinedEnablePins {
					gpio.AddOutput(elevatorPredefinedEnableOutputName(i), pin, appCtx.GPIOSettings.ElevatorEnableActiveLow, useI2CExpander)
				}
			}
			if len(appCtx.elevatorWaitFloorEnablePins) > 0 {
				for i, pin := range appCtx.elevatorWaitFloorEnablePins {
					gpio.AddOutput(elevatorWaitFloorEnableOutputName(i), pin, appCtx.GPIOSettings.ElevatorEnableActiveLow, useI2CExpander)
				}
			} else if appCtx.GPIOSettings.ElevatorEnableRelayPin != 0 {
				gpio.AddOutput("elevator_enable", appCtx.GPIOSettings.ElevatorEnableRelayPin, appCtx.GPIOSettings.ElevatorEnableActiveLow, useI2CExpander)
			}
		}
		gpio.ConfigureDoorSensor(appCtx.GPIOSettings.DoorSensorPin)
		waitMode := NormalizeKeypadOperationMode(appCtx.Config.KeypadOperationMode) == ModeElevatorWaitFloorButtons
		if waitMode && elevatorWaitFloorSenseCabInputs(appCtx.Config) {
			if pins, err := parseBCMPinList(appCtx.Config.ElevatorFloorInputPins); err == nil && len(pins) > 0 {
				gpio.ConfigureElevatorFloorPins(pins)
			} else if err != nil && strings.TrimSpace(appCtx.Config.ElevatorFloorInputPins) != "" {
				log.Printf("WARNING: elevator_floor_input_pins: %v", err)
			}
		}
		setupOperationModeGPIOInputs(appCtx, gpio)
		appCtx.GPIO = gpio
		go gpio.StartListening()
	}

	// 5. Initialize MQTT for Centralized Remote Control [cite: 6, 7]
	appCtx.MQTTClient = initMQTT(appCtx)

	// 6. Start Concurrent Subsystems
	go startHeartbeatAPI(appCtx) // Regular heartbeat via API [cite: 9]
	go startKeypadListeners(appCtx)
	go monitorElevatorFloorSelection(appCtx)
	go monitorDoorSensors(appCtx) // Door open timers & warnings
	go displayController(appCtx)  // Manage DSI Screen and random QR codes [cite: 3, 10, 11]

	// 7. Start Web Server (Web UI & REST HTTP API with token support) [cite: 6, 7]
	srv := startWebServer(appCtx)
	appCtx.configMu.RLock()
	startupCfg := appCtx.Config
	appCtx.configMu.RUnlock()
	playSoundAsync(startupCfg, startupCfg.SoundStartup)

	shutdownFromMenu := make(chan struct{}, 1)
	if !*noTechMenu {
		go runTechnicianMenu(appCtx, shutdownFromMenu)
	} else {
		releaseStartupLogBuffer(os.Stdout)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	select {
	case <-quit:
	case <-shutdownFromMenu:
	}
	log.Println("INFO: Shutdown signal received.")
	appCtx.configMu.RLock()
	shutdownCfg := appCtx.Config
	appCtx.configMu.RUnlock()
	playSoundSync(shutdownCfg, shutdownCfg.SoundShutdown)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("WARNING: HTTP server shutdown: %v", err)
	}
}

// --- Subsystem Implementations (Stubs) ---

func initLogger() {
	// Set up logger with configurable levels (Info, Debug, Warning, Critical) [cite: 9]
	// Allows console debugging [cite: 8]
	log.SetOutput(newColorLogWriter(os.Stdout))
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("INFO: Access Control System Booting...")
}

// ANSI level colors (foreground). Set NO_COLOR in the environment to disable.
const (
	colorReset   = "\033[0m"
	colorDebug   = "\033[36m"   // cyan
	colorInfo    = "\033[32m"   // green
	colorWarning = "\033[33m"   // yellow
	colorError   = "\033[31m"   // red
	colorCrit    = "\033[1;31m" // bold red
)

// levelTag associates a log prefix with a color code.
var logLevelTags = []struct {
	prefix string
	color  string
}{
	{"CRITICAL:", colorCrit},
	{"ERROR:", colorError},
	{"WARNING:", colorWarning},
	{"DEBUG:", colorDebug},
	{"INFO:", colorInfo},
}

// Technician terminal UI: reserve bottom row for "{TechMenuPrompt}> " while logs scroll above (DECSTBM + prompt redraw).
var (
	techUILock            sync.Mutex
	techBottomLineEnabled bool
	techTerminalRows      int

	// techMenuInputDraft is the current in-progress line from readTechMenuLine; repainted after log lines (same lock as UI).
	techMenuInputDraft []byte

	startupLogMu        sync.Mutex
	startupLogBuffer    [][]byte
	startupLogsReleased bool // after menu banner or -notechmenu; further log lines are not buffered

	techMenuPromptMu   sync.RWMutex
	techMenuPromptText string // copy of AppContext.TechMenuPrompt for the log writer (set via registerTechMenuPrompt)
)

// linux / glibc TIOCGWINSZ
const tiocgwinsz = 0x5413

type termWinSize struct {
	row uint16
	col uint16
	x   uint16
	y   uint16
}

func queryTerminalRows() int {
	var ws termWinSize
	fd := os.Stdout.Fd()
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(tiocgwinsz), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.row < 2 {
		if s := os.Getenv("LINES"); s != "" {
			var n int
			_, _ = fmt.Sscanf(s, "%d", &n)
			if n >= 2 {
				return n
			}
		}
		return 24
	}
	return int(ws.row)
}

// registerTechMenuPrompt copies the label from AppContext for use on the technician status line (call after appCtx is built).
func registerTechMenuPrompt(label string) {
	techMenuPromptMu.Lock()
	defer techMenuPromptMu.Unlock()
	if strings.TrimSpace(label) == "" {
		techMenuPromptText = "tech"
		return
	}
	techMenuPromptText = label
}

func activeTechMenuPrompt() string {
	techMenuPromptMu.RLock()
	defer techMenuPromptMu.RUnlock()
	if techMenuPromptText == "" {
		return "tech"
	}
	return techMenuPromptText
}

// moveToScrollRegionBottomUnlocked moves the cursor to the first column of the bottom line
// inside the scrolling region (row rows-1). The following text + LF scrolls only that region,
// so logs never print on the reserved status row (row rows).
func moveToScrollRegionBottomUnlocked(w io.Writer) {
	if !techBottomLineEnabled || techTerminalRows < 2 {
		return
	}
	_, _ = fmt.Fprintf(w, "\033[%d;1H", techTerminalRows-1)
}

// paintTechPromptRowUnlocked redraws the bottom status row and leaves the cursor after "{prompt}> "
// for /dev/tty echo. Does not use save/restore (that restored the cursor onto the status line and broke logging).
func paintTechPromptRowUnlocked(w io.Writer) {
	if !techBottomLineEnabled || techTerminalRows < 2 {
		return
	}
	_, _ = fmt.Fprintf(w, "\033[%d;1H\033[K", techTerminalRows)
	_, _ = fmt.Fprint(w, activeTechMenuPrompt())
	_, _ = fmt.Fprint(w, "> ")
}

// paintTechPromptAndInputDraftUnlocked redraws the status prompt and any in-progress technician input.
// Caller must hold techUILock.
func paintTechPromptAndInputDraftUnlocked(w io.Writer) {
	paintTechPromptRowUnlocked(w)
	if len(techMenuInputDraft) > 0 {
		_, _ = w.Write(techMenuInputDraft)
	}
}

func enableTechBottomTerminalLayout() {
	rows := queryTerminalRows()
	if rows < 2 {
		return
	}
	techUILock.Lock()
	techTerminalRows = rows
	techBottomLineEnabled = true
	// Scroll only lines 1..rows-1; bottom row stays fixed. Home cursor in scroll region for new logs.
	_, _ = fmt.Fprintf(os.Stdout, "\033[1;%dr\033[1;1H", rows-1)
	paintTechPromptAndInputDraftUnlocked(os.Stdout)
	techUILock.Unlock()
}

func disableTechBottomTerminalLayout() {
	techUILock.Lock()
	techBottomLineEnabled = false
	_, _ = fmt.Fprint(os.Stdout, "\033[r\n")
	techUILock.Unlock()
}

// terminalHardReset sends RIS and related sequences (like the `reset` command) so margins, modes, and colors return to defaults.
func terminalHardReset() {
	const seq = "\033[0m\033[?25h\033[r\033c"
	_, _ = fmt.Fprint(os.Stdout, seq)
	if t, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		_, _ = fmt.Fprint(t, seq)
		_ = t.Close()
	}
}

// techMenuClearScreenAndRelayout clears the visible screen and restores the scrolling region and bottom prompt.
func techMenuClearScreenAndRelayout() {
	rows := queryTerminalRows()
	if rows < 2 {
		techUILock.Lock()
		rows = techTerminalRows
		techUILock.Unlock()
	}
	if rows < 2 {
		rows = 24
	}
	techUILock.Lock()
	defer techUILock.Unlock()
	techTerminalRows = rows
	techBottomLineEnabled = true
	_, _ = fmt.Fprint(os.Stdout, "\033[2J\033[1;1H")
	_, _ = fmt.Fprintf(os.Stdout, "\033[1;%dr\033[1;1H", rows-1)
	paintTechPromptAndInputDraftUnlocked(os.Stdout)
}

// bufferStartupLogLine returns true if the line was buffered (caller should not emit yet).
func bufferStartupLogLine(line []byte) bool {
	startupLogMu.Lock()
	defer startupLogMu.Unlock()
	if startupLogsReleased {
		return false
	}
	cp := append([]byte(nil), line...)
	startupLogBuffer = append(startupLogBuffer, cp)
	return true
}

// releaseStartupLogBuffer flushes buffered log lines after the menu is visible (or when there is no menu).
func releaseStartupLogBuffer(w io.Writer) {
	startupLogMu.Lock()
	if startupLogsReleased {
		startupLogMu.Unlock()
		return
	}
	lines := startupLogBuffer
	startupLogBuffer = nil
	startupLogsReleased = true
	startupLogMu.Unlock()

	for _, ln := range lines {
		techUILock.Lock()
		moveToScrollRegionBottomUnlocked(w)
		_, _ = w.Write(ln)
		paintTechPromptAndInputDraftUnlocked(w)
		techUILock.Unlock()
	}
}

// techMenuSyncPrint runs f on stdout under the UI lock and redraws the bottom prompt. Do not call log from inside f.
func techMenuSyncPrint(f func(w io.Writer)) {
	techUILock.Lock()
	defer techUILock.Unlock()
	moveToScrollRegionBottomUnlocked(os.Stdout)
	f(os.Stdout)
	paintTechPromptAndInputDraftUnlocked(os.Stdout)
}

type colorLogWriter struct {
	w   io.Writer
	buf []byte
}

func newColorLogWriter(w io.Writer) *colorLogWriter {
	return &colorLogWriter{w: w}
}

func (c *colorLogWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	if os.Getenv("NO_COLOR") != "" {
		c.buf = append(c.buf, p...)
		for {
			idx := bytes.IndexByte(c.buf, '\n')
			if idx < 0 {
				return n, nil
			}
			line := c.buf[:idx+1]
			c.buf = append([]byte(nil), c.buf[idx+1:]...)
			c.writePlainLogLine(line)
		}
	}
	c.buf = append(c.buf, p...)
	for {
		idx := bytes.IndexByte(c.buf, '\n')
		if idx < 0 {
			return n, nil
		}
		line := c.buf[:idx+1]
		c.buf = append([]byte(nil), c.buf[idx+1:]...)
		c.writeColoredLine(line)
	}
}

func (c *colorLogWriter) writePlainLogLine(line []byte) {
	if !shouldEmitLogLine(line) {
		return
	}
	if bufferStartupLogLine(line) {
		return
	}
	techUILock.Lock()
	moveToScrollRegionBottomUnlocked(c.w)
	_, _ = c.w.Write(line)
	paintTechPromptAndInputDraftUnlocked(c.w)
	techUILock.Unlock()
}

func (c *colorLogWriter) writeColoredLine(line []byte) {
	if !shouldEmitLogLine(line) {
		return
	}
	s := string(line)
	colored := s
	for _, lt := range logLevelTags {
		if strings.Contains(s, lt.prefix) {
			colored = lt.color + s + colorReset
			break
		}
	}
	if bufferStartupLogLine([]byte(colored)) {
		return
	}
	techUILock.Lock()
	moveToScrollRegionBottomUnlocked(c.w)
	_, _ = io.WriteString(c.w, colored)
	paintTechPromptAndInputDraftUnlocked(c.w)
	techUILock.Unlock()
}

// Access scheduling (SQLite): see access_time_profiles, access_levels, access_level_targets in initAccessScheduleSchema.
//
// Model: access_doors / door_groups — door strikes (device.access_control_door_id = access_doors.id).
// access_elevators / elevator_groups — elevator banks (device.access_control_elevator_id = access_elevators.id; used in elevator_* keypad modes).
// access_user_groups + access_user_group_members — who (PIN in access_pins).
// access_time_profiles + access_time_windows — named schedules; weekday 0–6 Sun–Sat or 7 = any day; minutes 0–1439; start>end crosses midnight.
// access_levels + access_level_targets — time profile + user group + exactly one target: door, door_group, elevator, or elevator_group.
//
// Example Mon–Fri 8:45–17:00 for door "east", group "staff", PIN 123456:
//
//	INSERT INTO access_doors VALUES ('east','East entry');
//	INSERT INTO access_user_groups VALUES ('staff','Staff');
//	INSERT INTO access_pins VALUES ('123456','Alice',1);
//	INSERT INTO access_user_group_members VALUES ('staff','123456');
//	INSERT INTO access_time_profiles VALUES ('biz','Standard Business','','');
//	INSERT INTO access_time_windows (time_profile_id,weekday,start_minute,end_minute) VALUES
//	  ('biz',1,525,1020),('biz',2,525,1020),('biz',3,525,1020),('biz',4,525,1020),('biz',5,525,1020);
//	INSERT INTO access_levels VALUES ('L1','Staff business hours','biz','staff',1);
//	INSERT INTO access_level_targets (access_level_id,door_id,door_group_id,elevator_id,elevator_group_id) VALUES ('L1','east',NULL,NULL,NULL);
//
// Elevator-only target example:
//
//	INSERT INTO access_elevators VALUES ('cab_a','Lobby car A');
//	INSERT INTO access_level_targets (access_level_id,door_id,door_group_id,elevator_id,elevator_group_id) VALUES ('L1',NULL,NULL,'cab_a',NULL);
//
// Per-PIN allowed floors (optional): access_elevator_pin_floors — only when device.access_control_elevator_id matches elevator_id.
// floor_index is 0-based in the same order as device.elevator_floor_input_pins / gpio.elevator_wait_floor_enable_pins / gpio.elevator_floor_dispatch_pins.
// If there are no rows for a PIN+elevator pair, all floors are allowed (backward compatible). If one or more rows exist, only listed indices are allowed.
// Bulk assignment: access_elevator_floor_groups + access_elevator_floor_group_members + access_elevator_pin_floor_groups (PIN may belong to groups; union of member floor_index values applies).
//
//	INSERT INTO access_elevator_pin_floors (pin,elevator_id,floor_index) VALUES ('123456','cab_a',0),('123456','cab_a',2);
//
// Logical labels / relay documentation: access_elevator_floor_labels — optional floor_name and relay_pin per elevator_id + floor_index (for logs and operator reference; relay_pin matches gpio expander/BCM index for that channel when set).
//
// Timed floor policy: access_elevator_floor_time_rules — per floor_index, reuse access_time_profiles + access_time_windows.
// action 'lock' denies that floor during matching windows (overrides PIN lists). action 'open' allows that floor during matching windows even when PIN would not list it (still subject to elevator access_schedule and valid credential).
//
//	INSERT INTO access_elevator_floor_labels (elevator_id,floor_index,floor_name,relay_pin) VALUES ('cab_a',0,'Lobby',5);
//	INSERT INTO access_elevator_floor_groups (id,elevator_id,display_name) VALUES ('grp_public','cab_a','Public');
//	INSERT INTO access_elevator_floor_group_members (group_id,floor_index) VALUES ('grp_public',0),('grp_public',1);
//	INSERT INTO access_elevator_pin_floor_groups (pin,group_id) VALUES ('123456','grp_public');
//	INSERT INTO access_elevator_floor_time_rules (elevator_id,floor_index,time_profile_id,action) VALUES ('cab_a',3,'nights','lock');

func accessLevelTargetsTableHasElevatorColumns(db *sql.DB) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(access_level_targets)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == "elevator_id" {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// migrateAccessLevelTargetsElevatorSupport rebuilds access_level_targets when upgrading from a schema
// that only had door targets, so elevator_id / elevator_group_id and the four-way CHECK apply.
func migrateAccessLevelTargetsElevatorSupport(db *sql.DB) error {
	ok, err := accessLevelTargetsTableHasElevatorColumns(db)
	if err != nil || ok {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmts := []string{
		`CREATE TABLE access_level_targets_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			access_level_id TEXT NOT NULL REFERENCES access_levels(id) ON DELETE CASCADE,
			door_id TEXT REFERENCES access_doors(id) ON DELETE CASCADE,
			door_group_id TEXT REFERENCES access_door_groups(id) ON DELETE CASCADE,
			elevator_id TEXT REFERENCES access_elevators(id) ON DELETE CASCADE,
			elevator_group_id TEXT REFERENCES access_elevator_groups(id) ON DELETE CASCADE,
			CHECK (
				(door_id IS NOT NULL AND door_group_id IS NULL AND elevator_id IS NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NOT NULL AND elevator_id IS NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NULL AND elevator_id IS NOT NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NULL AND elevator_id IS NULL AND elevator_group_id IS NOT NULL)
			)
		)`,
		`INSERT INTO access_level_targets_new (id, access_level_id, door_id, door_group_id, elevator_id, elevator_group_id)
			SELECT id, access_level_id, door_id, door_group_id, NULL, NULL FROM access_level_targets`,
		`DROP TABLE access_level_targets`,
		`ALTER TABLE access_level_targets_new RENAME TO access_level_targets`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migrate access_level_targets for elevators: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Println("INFO: Migrated access_level_targets for elevator access control columns.")
	return nil
}

// initAccessScheduleSchema creates tables for named time profiles, user groups, door/elevator groups, and access levels.
func initAccessScheduleSchema(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("pragma foreign_keys: %w", err)
	}

	tableStmts := []string{
		`CREATE TABLE IF NOT EXISTS access_doors (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_door_groups (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_door_group_members (
			door_group_id TEXT NOT NULL REFERENCES access_door_groups(id) ON DELETE CASCADE,
			door_id TEXT NOT NULL REFERENCES access_doors(id) ON DELETE CASCADE,
			PRIMARY KEY (door_group_id, door_id)
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevators (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_groups (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_group_members (
			elevator_group_id TEXT NOT NULL REFERENCES access_elevator_groups(id) ON DELETE CASCADE,
			elevator_id TEXT NOT NULL REFERENCES access_elevators(id) ON DELETE CASCADE,
			PRIMARY KEY (elevator_group_id, elevator_id)
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_pin_floors (
			pin TEXT NOT NULL,
			elevator_id TEXT NOT NULL REFERENCES access_elevators(id) ON DELETE CASCADE,
			floor_index INTEGER NOT NULL,
			PRIMARY KEY (pin, elevator_id, floor_index),
			FOREIGN KEY (pin) REFERENCES access_pins(pin) ON DELETE CASCADE,
			CHECK (floor_index >= 0)
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_floor_labels (
			elevator_id TEXT NOT NULL REFERENCES access_elevators(id) ON DELETE CASCADE,
			floor_index INTEGER NOT NULL,
			floor_name TEXT NOT NULL,
			relay_pin INTEGER,
			PRIMARY KEY (elevator_id, floor_index),
			CHECK (floor_index >= 0)
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_floor_groups (
			id TEXT PRIMARY KEY NOT NULL,
			elevator_id TEXT NOT NULL REFERENCES access_elevators(id) ON DELETE CASCADE,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_floor_group_members (
			group_id TEXT NOT NULL REFERENCES access_elevator_floor_groups(id) ON DELETE CASCADE,
			floor_index INTEGER NOT NULL,
			PRIMARY KEY (group_id, floor_index),
			CHECK (floor_index >= 0)
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_pin_floor_groups (
			pin TEXT NOT NULL,
			group_id TEXT NOT NULL REFERENCES access_elevator_floor_groups(id) ON DELETE CASCADE,
			PRIMARY KEY (pin, group_id),
			FOREIGN KEY (pin) REFERENCES access_pins(pin) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS access_elevator_floor_time_rules (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			elevator_id TEXT NOT NULL REFERENCES access_elevators(id) ON DELETE CASCADE,
			floor_index INTEGER NOT NULL,
			time_profile_id TEXT NOT NULL REFERENCES access_time_profiles(id) ON DELETE CASCADE,
			action TEXT NOT NULL CHECK (action IN ('open','lock')),
			enabled INTEGER NOT NULL DEFAULT 1,
			CHECK (floor_index >= 0)
		)`,
		`CREATE TABLE IF NOT EXISTS access_user_groups (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS access_user_group_members (
			group_id TEXT NOT NULL REFERENCES access_user_groups(id) ON DELETE CASCADE,
			pin TEXT NOT NULL,
			PRIMARY KEY (group_id, pin),
			FOREIGN KEY (pin) REFERENCES access_pins(pin) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS access_time_profiles (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT,
			description TEXT,
			iana_timezone TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS access_time_windows (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time_profile_id TEXT NOT NULL REFERENCES access_time_profiles(id) ON DELETE CASCADE,
			weekday INTEGER NOT NULL,
			start_minute INTEGER NOT NULL,
			end_minute INTEGER NOT NULL,
			CHECK (weekday >= 0 AND weekday <= 7),
			CHECK (start_minute >= 0 AND start_minute <= 1439),
			CHECK (end_minute >= 0 AND end_minute <= 1439)
		)`,
		`CREATE TABLE IF NOT EXISTS access_levels (
			id TEXT PRIMARY KEY NOT NULL,
			display_name TEXT,
			time_profile_id TEXT NOT NULL REFERENCES access_time_profiles(id),
			user_group_id TEXT NOT NULL REFERENCES access_user_groups(id),
			enabled INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS access_level_targets (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			access_level_id TEXT NOT NULL REFERENCES access_levels(id) ON DELETE CASCADE,
			door_id TEXT REFERENCES access_doors(id) ON DELETE CASCADE,
			door_group_id TEXT REFERENCES access_door_groups(id) ON DELETE CASCADE,
			elevator_id TEXT REFERENCES access_elevators(id) ON DELETE CASCADE,
			elevator_group_id TEXT REFERENCES access_elevator_groups(id) ON DELETE CASCADE,
			CHECK (
				(door_id IS NOT NULL AND door_group_id IS NULL AND elevator_id IS NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NOT NULL AND elevator_id IS NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NULL AND elevator_id IS NOT NULL AND elevator_group_id IS NULL)
				OR (door_id IS NULL AND door_group_id IS NULL AND elevator_id IS NULL AND elevator_group_id IS NOT NULL)
			)
		)`,
	}
	for _, q := range tableStmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("access schedule schema: %w", err)
		}
	}
	if err := migrateAccessLevelTargetsElevatorSupport(db); err != nil {
		return fmt.Errorf("access schedule schema: %w", err)
	}
	indexStmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_access_level_targets_level ON access_level_targets(access_level_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_level_targets_door ON access_level_targets(door_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_level_targets_elevator ON access_level_targets(elevator_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_elevator_pin_floors_lookup ON access_elevator_pin_floors(elevator_id, pin)`,
		`CREATE INDEX IF NOT EXISTS idx_access_elevator_floor_groups_elevator ON access_elevator_floor_groups(elevator_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_elevator_pin_floor_groups_pin ON access_elevator_pin_floor_groups(pin)`,
		`CREATE INDEX IF NOT EXISTS idx_access_elevator_floor_time_rules_lookup ON access_elevator_floor_time_rules(elevator_id, floor_index, enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_access_time_windows_profile ON access_time_windows(time_profile_id)`,
		`CREATE INDEX IF NOT EXISTS idx_access_user_group_members_pin ON access_user_group_members(pin)`,
	}
	for _, q := range indexStmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("access schedule schema: %w", err)
		}
	}
	log.Println("INFO: SQLite access schedule tables ready (doors, elevators, time profiles, user groups, access levels).")
	return nil
}

func accessScheduleTimeLocation(iana string) *time.Location {
	s := strings.TrimSpace(iana)
	if s == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(s)
	if err != nil {
		log.Printf("WARNING: access_time_profiles.iana_timezone %q invalid (%v); using local time.", s, err)
		return time.Local
	}
	return loc
}

// minuteMatchesWindow reports whether minute-of-day m is inside [start, end] inclusive.
// If start > end, the window crosses midnight (e.g. 22:00–06:00).
func minuteMatchesWindow(m, start, end int) bool {
	if start <= end {
		return m >= start && m <= end
	}
	return m >= start || m <= end
}

// timeMatchesProfileWindows returns true if t (already in the profile location) matches any window.
// weekday is Go's time.Weekday() (Sunday=0). SQL weekday 7 means "any day".
func timeMatchesProfileWindows(weekday time.Weekday, minuteOfDay int, rows []struct {
	weekday      int
	start, end int
}) bool {
	wd := int(weekday)
	for _, r := range rows {
		w := r.weekday
		if w != 7 && w != wd {
			continue
		}
		if minuteMatchesWindow(minuteOfDay, r.start, r.end) {
			return true
		}
	}
	return false
}

func accessScheduleHasTargetsForDoor(db *sql.DB, doorID string) (bool, error) {
	if db == nil || strings.TrimSpace(doorID) == "" {
		return false, nil
	}
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT 1
			FROM access_levels al
			INNER JOIN access_level_targets t ON t.access_level_id = al.id
			WHERE al.enabled = 1 AND (
				t.door_id = ?
				OR EXISTS (
					SELECT 1 FROM access_door_group_members dgm
					WHERE dgm.door_group_id = t.door_group_id AND dgm.door_id = ?
				)
			)
			LIMIT 1
		)`, doorID, doorID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// accessScheduleAllows returns whether PIN may open the given door at atTime under schedule rules.
// When scheduling does not apply, returns (true, "").
func (ctx *AppContext) accessScheduleAllows(pin, doorID string, atTime time.Time, viaFallback bool) (bool, string) {
	pin = strings.TrimSpace(pin)
	doorID = strings.TrimSpace(doorID)
	ctx.configMu.RLock()
	enforce := ctx.Config.AccessScheduleEnforce
	applyFallback := ctx.Config.AccessScheduleApplyToFallbackPin
	ctx.configMu.RUnlock()

	if ctx.DB == nil || doorID == "" || !enforce {
		return true, ""
	}
	if viaFallback && !applyFallback {
		return true, ""
	}
	hasRules, err := accessScheduleHasTargetsForDoor(ctx.DB, doorID)
	if err != nil {
		log.Printf("WARNING: access schedule door target check: %v", err)
		return false, "schedule_db_error"
	}
	if !hasRules {
		return true, ""
	}

	rows, err := ctx.DB.Query(`
		SELECT DISTINCT al.time_profile_id, tp.iana_timezone
		FROM access_levels al
		INNER JOIN access_time_profiles tp ON tp.id = al.time_profile_id
		INNER JOIN access_level_targets t ON t.access_level_id = al.id
		INNER JOIN access_user_group_members ugm ON ugm.group_id = al.user_group_id AND ugm.pin = ?
		WHERE al.enabled = 1 AND (
			t.door_id = ?
			OR EXISTS (
				SELECT 1 FROM access_door_group_members dgm
				WHERE dgm.door_group_id = t.door_group_id AND dgm.door_id = ?
			)
		)`, pin, doorID, doorID)
	if err != nil {
		log.Printf("WARNING: access schedule level query: %v", err)
		return false, "schedule_db_error"
	}
	defer rows.Close()

	type profTZ struct {
		id  string
		tz  string
		key string
	}
	var list []profTZ
	for rows.Next() {
		var pid, iana string
		if err := rows.Scan(&pid, &iana); err != nil {
			log.Printf("WARNING: access schedule scan: %v", err)
			continue
		}
		list = append(list, profTZ{id: strings.TrimSpace(pid), tz: strings.TrimSpace(iana), key: strings.TrimSpace(pid) + "\x00" + strings.TrimSpace(iana)})
	}
	if err := rows.Err(); err != nil {
		return false, "schedule_db_error"
	}
	if len(list) == 0 {
		return false, "no_access_level_for_credential"
	}

	seen := make(map[string]struct{})
	for _, pt := range list {
		if _, ok := seen[pt.key]; ok {
			continue
		}
		seen[pt.key] = struct{}{}

		loc := accessScheduleTimeLocation(pt.tz)
		tLocal := atTime.In(loc)
		minuteOfDay := tLocal.Hour()*60 + tLocal.Minute()
		wd := tLocal.Weekday()

		wrows, err := ctx.DB.Query(`
			SELECT weekday, start_minute, end_minute
			FROM access_time_windows
			WHERE time_profile_id = ?
			ORDER BY id`, pt.id)
		if err != nil {
			log.Printf("WARNING: access schedule windows: %v", err)
			return false, "schedule_db_error"
		}
		var wins []struct {
			weekday      int
			start, end int
		}
		for wrows.Next() {
			var wk, sm, em int
			if err := wrows.Scan(&wk, &sm, &em); err != nil {
				_ = wrows.Close()
				return false, "schedule_db_error"
			}
			wins = append(wins, struct {
				weekday      int
				start, end int
			}{wk, sm, em})
		}
		if err := wrows.Close(); err != nil {
			return false, "schedule_db_error"
		}

		if len(wins) == 0 {
			continue
		}
		if timeMatchesProfileWindows(wd, minuteOfDay, wins) {
			return true, ""
		}
	}

	return false, "outside_scheduled_hours"
}

func accessScheduleHasTargetsForElevator(db *sql.DB, elevatorID string) (bool, error) {
	if db == nil || strings.TrimSpace(elevatorID) == "" {
		return false, nil
	}
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM (
			SELECT 1
			FROM access_levels al
			INNER JOIN access_level_targets t ON t.access_level_id = al.id
			WHERE al.enabled = 1 AND (
				t.elevator_id = ?
				OR EXISTS (
					SELECT 1 FROM access_elevator_group_members egm
					WHERE egm.elevator_group_id = t.elevator_group_id AND egm.elevator_id = ?
				)
			)
			LIMIT 1
		)`, elevatorID, elevatorID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// accessScheduleAllowsElevator returns whether PIN may use elevator control at atTime under schedule rules.
// When scheduling does not apply, returns (true, "").
func (ctx *AppContext) accessScheduleAllowsElevator(pin, elevatorID string, atTime time.Time, viaFallback bool) (bool, string) {
	pin = strings.TrimSpace(pin)
	elevatorID = strings.TrimSpace(elevatorID)
	ctx.configMu.RLock()
	enforce := ctx.Config.AccessScheduleEnforce
	applyFallback := ctx.Config.AccessScheduleApplyToFallbackPin
	ctx.configMu.RUnlock()

	if ctx.DB == nil || elevatorID == "" || !enforce {
		return true, ""
	}
	if viaFallback && !applyFallback {
		return true, ""
	}
	hasRules, err := accessScheduleHasTargetsForElevator(ctx.DB, elevatorID)
	if err != nil {
		log.Printf("WARNING: access schedule elevator target check: %v", err)
		return false, "schedule_db_error"
	}
	if !hasRules {
		return true, ""
	}

	rows, err := ctx.DB.Query(`
		SELECT DISTINCT al.time_profile_id, tp.iana_timezone
		FROM access_levels al
		INNER JOIN access_time_profiles tp ON tp.id = al.time_profile_id
		INNER JOIN access_level_targets t ON t.access_level_id = al.id
		INNER JOIN access_user_group_members ugm ON ugm.group_id = al.user_group_id AND ugm.pin = ?
		WHERE al.enabled = 1 AND (
			t.elevator_id = ?
			OR EXISTS (
				SELECT 1 FROM access_elevator_group_members egm
				WHERE egm.elevator_group_id = t.elevator_group_id AND egm.elevator_id = ?
			)
		)`, pin, elevatorID, elevatorID)
	if err != nil {
		log.Printf("WARNING: access schedule elevator level query: %v", err)
		return false, "schedule_db_error"
	}
	defer rows.Close()

	type profTZ struct {
		id  string
		tz  string
		key string
	}
	var list []profTZ
	for rows.Next() {
		var pid, iana string
		if err := rows.Scan(&pid, &iana); err != nil {
			log.Printf("WARNING: access schedule elevator scan: %v", err)
			continue
		}
		list = append(list, profTZ{id: strings.TrimSpace(pid), tz: strings.TrimSpace(iana), key: strings.TrimSpace(pid) + "\x00" + strings.TrimSpace(iana)})
	}
	if err := rows.Err(); err != nil {
		return false, "schedule_db_error"
	}
	if len(list) == 0 {
		return false, "no_access_level_for_credential"
	}

	seen := make(map[string]struct{})
	for _, pt := range list {
		if _, ok := seen[pt.key]; ok {
			continue
		}
		seen[pt.key] = struct{}{}

		loc := accessScheduleTimeLocation(pt.tz)
		tLocal := atTime.In(loc)
		minuteOfDay := tLocal.Hour()*60 + tLocal.Minute()
		wd := tLocal.Weekday()

		wrows, err := ctx.DB.Query(`
			SELECT weekday, start_minute, end_minute
			FROM access_time_windows
			WHERE time_profile_id = ?
			ORDER BY id`, pt.id)
		if err != nil {
			log.Printf("WARNING: access schedule elevator windows: %v", err)
			return false, "schedule_db_error"
		}
		var wins []struct {
			weekday      int
			start, end int
		}
		for wrows.Next() {
			var wk, sm, em int
			if err := wrows.Scan(&wk, &sm, &em); err != nil {
				_ = wrows.Close()
				return false, "schedule_db_error"
			}
			wins = append(wins, struct {
				weekday      int
				start, end int
			}{wk, sm, em})
		}
		if err := wrows.Close(); err != nil {
			return false, "schedule_db_error"
		}

		if len(wins) == 0 {
			continue
		}
		if timeMatchesProfileWindows(wd, minuteOfDay, wins) {
			return true, ""
		}
	}

	return false, "outside_scheduled_hours"
}

func (ctx *AppContext) effectiveAccessDoorID() string {
	ctx.configMu.RLock()
	defer ctx.configMu.RUnlock()
	return strings.TrimSpace(ctx.Config.AccessControlDoorID)
}

func (ctx *AppContext) effectiveAccessElevatorID() string {
	ctx.configMu.RLock()
	defer ctx.configMu.RUnlock()
	return strings.TrimSpace(ctx.Config.AccessControlElevatorID)
}

// loadElevatorPinFloorAllowSet reads per-floor allow list for this PIN and elevator from
// access_elevator_pin_floors and from access_elevator_pin_floor_groups (union of group members).
// When hasRows is false, the caller should treat the credential as unrestricted for floors (PIN-only rules).
func loadElevatorPinFloorAllowSet(db *sql.DB, pin, elevatorID string) (map[int]bool, bool, error) {
	pin = strings.TrimSpace(pin)
	elevatorID = strings.TrimSpace(elevatorID)
	if db == nil || pin == "" || elevatorID == "" {
		return nil, false, nil
	}
	rows, err := db.Query(`
		SELECT floor_index FROM access_elevator_pin_floors
		WHERE pin = ? AND elevator_id = ?
		UNION
		SELECT m.floor_index FROM access_elevator_pin_floor_groups pfg
		INNER JOIN access_elevator_floor_groups g ON g.id = pfg.group_id AND g.elevator_id = ?
		INNER JOIN access_elevator_floor_group_members m ON m.group_id = g.id
		WHERE pfg.pin = ?
		ORDER BY floor_index`, pin, elevatorID, elevatorID, pin)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	m := make(map[int]bool)
	for rows.Next() {
		var fi int
		if err := rows.Scan(&fi); err != nil {
			return nil, false, err
		}
		if fi >= 0 {
			m[fi] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return m, len(m) > 0, nil
}

// elevatorPinMayUseFloor enforces access_elevator_pin_floors when device.access_control_elevator_id is set
// and there is at least one row for this PIN+elevator. Fallback PIN behavior matches access_schedule_apply_to_fallback_pin.
func (ctx *AppContext) elevatorPinMayUseFloor(pin, elevatorID string, floorIndex int, viaFallback bool) bool {
	pin = strings.TrimSpace(pin)
	elevatorID = strings.TrimSpace(elevatorID)
	if ctx.DB == nil || elevatorID == "" || floorIndex < 0 {
		return true
	}
	ctx.configMu.RLock()
	applyFallback := ctx.Config.AccessScheduleApplyToFallbackPin
	ctx.configMu.RUnlock()
	if viaFallback && !applyFallback {
		return true
	}
	m, hasRows, err := loadElevatorPinFloorAllowSet(ctx.DB, pin, elevatorID)
	if err != nil || !hasRows {
		return true
	}
	return m[floorIndex]
}

// elevatorFloorTimedPolicy reports whether active time windows mark the floor locked and/or open.
// lock takes precedence in elevatorFloorChannelAllowed. open allows bypass of PIN floor lists only.
func (ctx *AppContext) elevatorFloorTimedPolicy(elevatorID string, floorIndex int, at time.Time) (locked, openActive bool) {
	elevatorID = strings.TrimSpace(elevatorID)
	if ctx.DB == nil || elevatorID == "" || floorIndex < 0 {
		return false, false
	}
	rows, err := ctx.DB.Query(`
		SELECT r.action, tp.iana_timezone, tw.weekday, tw.start_minute, tw.end_minute
		FROM access_elevator_floor_time_rules r
		INNER JOIN access_time_profiles tp ON tp.id = r.time_profile_id
		INNER JOIN access_time_windows tw ON tw.time_profile_id = tp.id
		WHERE r.enabled = 1 AND r.elevator_id = ? AND r.floor_index = ?
		ORDER BY r.id, tw.id`, elevatorID, floorIndex)
	if err != nil {
		log.Printf("WARNING: access_elevator_floor_time_rules: %v", err)
		return false, false
	}
	defer rows.Close()
	for rows.Next() {
		var action, iana string
		var wk, sm, em int
		if err := rows.Scan(&action, &iana, &wk, &sm, &em); err != nil {
			log.Printf("WARNING: access_elevator_floor_time_rules scan: %v", err)
			continue
		}
		loc := accessScheduleTimeLocation(iana)
		tLocal := at.In(loc)
		minuteOfDay := tLocal.Hour()*60 + tLocal.Minute()
		wd := tLocal.Weekday()
		wins := []struct {
			weekday      int
			start, end int
		}{{wk, sm, em}}
		if !timeMatchesProfileWindows(wd, minuteOfDay, wins) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "lock":
			locked = true
		case "open":
			openActive = true
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("WARNING: access_elevator_floor_time_rules: %v", err)
	}
	return locked, openActive
}

// elevatorFloorChannelAllowed is the full per-floor check: timed lock/open rules, then PIN floor list (and groups).
func (ctx *AppContext) elevatorFloorChannelAllowed(pin, elevatorID string, floorIndex int, viaFallback bool, at time.Time) bool {
	pin = strings.TrimSpace(pin)
	elevatorID = strings.TrimSpace(elevatorID)
	if ctx.DB == nil || elevatorID == "" || floorIndex < 0 {
		return true
	}
	locked, openWin := ctx.elevatorFloorTimedPolicy(elevatorID, floorIndex, at)
	if locked {
		return false
	}
	if openWin {
		return true
	}
	return ctx.elevatorPinMayUseFloor(pin, elevatorID, floorIndex, viaFallback)
}

// elevatorFloorLogLabel returns a short label for logs/webhooks: "name [index N]" or "index N".
func elevatorFloorLogLabel(db *sql.DB, elevatorID string, floorIndex int) string {
	elevatorID = strings.TrimSpace(elevatorID)
	if db == nil || elevatorID == "" || floorIndex < 0 {
		return fmt.Sprintf("index %d", floorIndex)
	}
	var name sql.NullString
	err := db.QueryRow(`
		SELECT floor_name FROM access_elevator_floor_labels
		WHERE elevator_id = ? AND floor_index = ?`, elevatorID, floorIndex).Scan(&name)
	if err != nil || !name.Valid {
		return fmt.Sprintf("index %d", floorIndex)
	}
	n := strings.TrimSpace(name.String)
	if n == "" {
		return fmt.Sprintf("index %d", floorIndex)
	}
	return fmt.Sprintf("%q [index %d]", n, floorIndex)
}

func elevatorFloorLogLabels(db *sql.DB, elevatorID string, indices []int) []string {
	out := make([]string, 0, len(indices))
	for _, fi := range indices {
		out = append(out, elevatorFloorLogLabel(db, elevatorID, fi))
	}
	return out
}

// elevatorPredefinedDispatchIndexForACL returns the 0-based floor index used for access_elevator_pin_floors and dispatch wiring.
func (ctx *AppContext) elevatorPredefinedDispatchIndexForACL(cfg DeviceConfig) int {
	nf := len(cfg.ElevatorPredefinedFloors)
	nDisp := len(ctx.elevatorFloorDispatchPins)
	idx := cfg.ElevatorPredefinedFloor
	if nf == 0 {
		if nDisp > 0 {
			if idx < 0 {
				idx = 0
			}
			if idx >= nDisp {
				idx = nDisp - 1
			}
		}
		return idx
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= nf {
		idx = nf - 1
	}
	return idx
}

func initDatabase() *sql.DB {
	// Initialize SQLite for storing logs, configs, and access control lists [cite: 6, 7]
	db, err := sql.Open("sqlite3", "file:./access_control.db?_fk=1&_busy_timeout=5000")
	if err != nil {
		releaseStartupLogBuffer(os.Stdout)
		log.Fatalf("CRITICAL: Failed to open database: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS access_pins (
		pin TEXT PRIMARY KEY NOT NULL,
		label TEXT,
		enabled INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		log.Printf("WARNING: access_pins table: %v", err)
	} else {
		log.Println("INFO: SQLite access_pins table ready (PINs optional; device.fallback_access_pin used when set and no row matches).")
	}
	if err := initAccessScheduleSchema(db); err != nil {
		log.Printf("WARNING: access schedule schema: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at TEXT NOT NULL,
		event_type TEXT NOT NULL DEFAULT 'event',
		event_name TEXT NOT NULL,
		device_client_id TEXT,
		detail_json TEXT NOT NULL DEFAULT '{}'
	)`); err != nil {
		log.Printf("WARNING: logs table: %v", err)
	} else {
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_logs_created_at ON logs(created_at)`); err != nil {
			log.Printf("WARNING: logs index idx_logs_created_at: %v", err)
		}
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_logs_event_name ON logs(event_name)`); err != nil {
			log.Printf("WARNING: logs index idx_logs_event_name: %v", err)
		}
		log.Println("INFO: SQLite logs table ready (audit trail for event activities).")
	}
	return db
}

// accessPinLookupResult is the outcome of validating a PIN against access_pins and optional fallback.
type accessPinLookupResult struct {
	OK          bool
	Label       string
	ViaFallback bool
}

// accessCredentialForPIN returns whether the PIN is allowed: row in access_pins with enabled=1, or FallbackAccessPin when set and no DB match.
func (ctx *AppContext) accessCredentialForPIN(pin string) accessPinLookupResult {
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return accessPinLookupResult{}
	}
	if ctx.DB != nil {
		var lbl sql.NullString
		err := ctx.DB.QueryRow(`SELECT label FROM access_pins WHERE pin = ? AND enabled = 1`, pin).Scan(&lbl)
		if err == nil {
			return accessPinLookupResult{OK: true, Label: strings.TrimSpace(lbl.String)}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("WARNING: access_pins lookup: %v", err)
		}
	}
	ctx.configMu.RLock()
	fallback := strings.TrimSpace(ctx.Config.FallbackAccessPin)
	ctx.configMu.RUnlock()
	if fallback != "" && pin == fallback {
		return accessPinLookupResult{OK: true, ViaFallback: true}
	}
	return accessPinLookupResult{}
}

// adjustDualKeypadOccupancy updates per-PIN "inside" counts for access_dual_usb_keypad (entry +1, exit −1). Returns total people across all PINs and this PIN's inside count after the change.
func (ctx *AppContext) adjustDualKeypadOccupancy(pin, keypadRole string) (areaTotal int, insideThisPIN int, mismatch string) {
	ctx.occupancyMu.Lock()
	defer ctx.occupancyMu.Unlock()
	if ctx.occupancyByPIN == nil {
		ctx.occupancyByPIN = make(map[string]int)
	}
	switch keypadRole {
	case "entry":
		ctx.occupancyByPIN[pin]++
		insideThisPIN = ctx.occupancyByPIN[pin]
	case "exit":
		if ctx.occupancyByPIN[pin] > 0 {
			ctx.occupancyByPIN[pin]--
			insideThisPIN = ctx.occupancyByPIN[pin]
			if insideThisPIN == 0 {
				delete(ctx.occupancyByPIN, pin)
			}
		} else {
			mismatch = "exit_without_recorded_entry"
		}
	default:
		return 0, 0, ""
	}
	for _, n := range ctx.occupancyByPIN {
		if n > 0 {
			areaTotal += n
		}
	}
	return areaTotal, insideThisPIN, mismatch
}

func keypadLogTag(keypadRole string) string {
	if strings.TrimSpace(keypadRole) == "" {
		return "single"
	}
	return keypadRole
}

// dualKeypadExitWouldMismatch is true when exit would not decrement an existing inside count (no prior entry for this PIN).
func (ctx *AppContext) dualKeypadExitWouldMismatch(pin string) bool {
	ctx.occupancyMu.Lock()
	defer ctx.occupancyMu.Unlock()
	if ctx.occupancyByPIN == nil {
		return true
	}
	return ctx.occupancyByPIN[pin] <= 0
}

// maskPINForTechDisplay hides a credential for technician output (last two digits visible).
func maskPINForTechDisplay(pin string) string {
	pin = strings.TrimSpace(pin)
	if len(pin) <= 2 {
		return "****"
	}
	return strings.Repeat("*", len(pin)-2) + pin[len(pin)-2:]
}

// mqttInitialConnectTimeout bounds the first broker connection attempt; if the broker is unreachable,
// initMQTT returns nil so the rest of the process starts normally.
const mqttInitialConnectTimeout = 5 * time.Second

func initMQTT(ctx *AppContext) mqtt.Client {
	ctx.configMu.RLock()
	cfg := ctx.Config
	ctx.configMu.RUnlock()
	if !cfg.MQTTEnabled {
		log.Println("INFO: MQTT disabled (MQTTEnabled false).")
		return nil
	}
	if strings.TrimSpace(cfg.MQTTBroker) == "" {
		log.Println("INFO: MQTT disabled (MQTTBroker empty).")
		return nil
	}

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.MQTTBroker).
		SetClientID(cfg.MQTTClientID).
		SetConnectTimeout(mqttInitialConnectTimeout).
		SetConnectRetry(false).
		SetAutoReconnect(true)

	if cfg.MQTTUsername != "" {
		opts.SetUsername(cfg.MQTTUsername)
		opts.SetPassword(cfg.MQTTPassword)
	}

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		ctx.configMu.RLock()
		topic := strings.TrimSpace(ctx.Config.MQTTCommandTopic)
		pairTopic := strings.TrimSpace(ctx.Config.MQTTPairPeerTopic)
		mode := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
		role := normalizePairPeerRole(ctx.Config.PairPeerRole)
		ctx.configMu.RUnlock()
		if topic != "" {
			h := mqttRemoteMessageHandler(ctx)
			if t := c.Subscribe(topic, 1, h); t.Wait() && t.Error() != nil {
				log.Printf("WARNING: MQTT subscribe %q: %v", topic, t.Error())
			} else {
				log.Printf("INFO: MQTT remote control subscribed to %q", topic)
			}
		}
		if pairTopic != "" && pairedExitSubscribesToPeer(mode, role) {
			ph := mqttPairPeerMessageHandler(ctx)
			if t := c.Subscribe(pairTopic, 1, ph); t.Wait() && t.Error() != nil {
				log.Printf("WARNING: MQTT pair-peer subscribe %q: %v", pairTopic, t.Error())
			} else {
				log.Printf("INFO: MQTT pair-peer (exit station) subscribed to %q", pairTopic)
			}
		}
	})

	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(mqttInitialConnectTimeout) {
		log.Printf("WARNING: MQTT broker %q not reachable within %v; MQTT disabled for this run.", cfg.MQTTBroker, mqttInitialConnectTimeout)
		client.Disconnect(250)
		return nil
	}
	if err := token.Error(); err != nil {
		log.Printf("WARNING: MQTT connection failed: %v; MQTT disabled for this run.", err)
		client.Disconnect(250)
		return nil
	}
	log.Printf("INFO: MQTT connected to %q", cfg.MQTTBroker)
	return client
}

// mqttRemoteCmd is the expected JSON body on MQTTCommandTopic.
type mqttRemoteCmd struct {
	Cmd   string `json:"cmd"`
	Token string `json:"token,omitempty"`
}

// mqttRemoteAck is published to MQTTStatusTopic when that topic is configured.
type mqttRemoteAck struct {
	OK     bool   `json:"ok"`
	Cmd    string `json:"cmd"`
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
	// DoorOpen is set for cmd door_status when GPIO is available.
	DoorOpen *bool `json:"door_open,omitempty"`
}

var mqttRemoteMu sync.Mutex

func mqttRemoteMessageHandler(ctx *AppContext) mqtt.MessageHandler {
	return func(_ mqtt.Client, m mqtt.Message) {
		handleMQTTRemotePayload(ctx, m.Topic(), m.Payload())
	}
}

func handleMQTTRemotePayload(ctx *AppContext, topic string, payload []byte) {
	mqttRemoteMu.Lock()
	defer mqttRemoteMu.Unlock()

	ctx.mqttMu.RLock()
	clientOK := ctx.MQTTClient != nil && ctx.MQTTClient.IsConnected()
	ctx.mqttMu.RUnlock()
	if !clientOK {
		log.Printf("WARNING: MQTT remote command ignored (client not connected). topic=%s", topic)
		return
	}

	p := bytes.TrimSpace(payload)
	var req mqttRemoteCmd
	jsonOK := json.Unmarshal(p, &req) == nil && strings.TrimSpace(req.Cmd) != ""
	cmd := ""
	if jsonOK {
		cmd = strings.TrimSpace(req.Cmd)
	} else {
		cmd = strings.TrimSpace(string(p))
	}
	cmdLower := strings.ToLower(cmd)

	ctx.configMu.RLock()
	token := ctx.Config.MQTTCommandToken
	cfg := ctx.Config
	ctx.configMu.RUnlock()

	if token != "" {
		if !jsonOK || req.Token != token {
			mqttPublishRemoteAck(ctx, mqttRemoteAck{OK: false, Cmd: cmdLower, Error: "invalid or missing token (JSON + token required)"})
			log.Println("WARNING: MQTT remote command rejected (bad token or non-JSON payload).")
			return
		}
	}

	if cmdLower == "" {
		mqttPublishRemoteAck(ctx, mqttRemoteAck{OK: false, Error: "empty_command"})
		return
	}

	switch cmdLower {
	case "open_door", "door_open", "unlock":
		log.Printf("INFO: MQTT remote: open door (topic=%s)", topic)
		playSoundAsync(cfg, cfg.SoundPinOK)
		if ctx.GPIO != nil {
			ctx.GPIO.ActionPulse("door", cfg.RelayPulseDuration)
			mqttPublishRemoteAck(ctx, mqttRemoteAck{OK: true, Cmd: cmdLower, Detail: "door relay pulsed"})
			fireEventWebhook(ctx, "mqtt_remote_door_open", map[string]any{"mqtt_topic": topic})
		} else {
			mqttPublishRemoteAck(ctx, mqttRemoteAck{OK: false, Cmd: cmdLower, Error: "gpio_unavailable"})
			log.Println("WARNING: MQTT open_door: GPIO unavailable.")
		}

	case "buzzer", "buzz", "alarm":
		log.Printf("INFO: MQTT remote: buzzer (topic=%s)", topic)
		if ctx.GPIO != nil {
			ctx.GPIO.ActionPulse("buzzer", cfg.BuzzerRelayPulseDuration)
			mqttPublishRemoteAck(ctx, mqttRemoteAck{OK: true, Cmd: cmdLower, Detail: "buzzer relay pulsed"})
			fireEventWebhook(ctx, "mqtt_remote_buzzer", map[string]any{"mqtt_topic": topic})
		} else {
			mqttPublishRemoteAck(ctx, mqttRemoteAck{OK: false, Cmd: cmdLower, Error: "gpio_unavailable"})
		}

	case "door_status", "status_door":
		if ctx.GPIO == nil || !ctx.GPIO.DoorSensorConfigured() {
			mqttPublishRemoteAck(ctx, mqttRemoteAck{OK: false, Cmd: cmdLower, Error: "door_sensor_unavailable"})
			return
		}
		open := ctx.GPIO.DoorIsOpen(cfg.DoorSensorClosedIsLow)
		mqttPublishRemoteAck(ctx, mqttRemoteAck{OK: true, Cmd: cmdLower, DoorOpen: &open})

	case "ping", "hello":
		mqttPublishRemoteAck(ctx, mqttRemoteAck{OK: true, Cmd: cmdLower, Detail: "pong"})

	default:
		mqttPublishRemoteAck(ctx, mqttRemoteAck{OK: false, Cmd: cmdLower, Error: "unknown_command"})
		log.Printf("WARNING: MQTT remote unknown cmd %q", cmd)
	}
}

func mqttPublishRemoteAck(ctx *AppContext, ack mqttRemoteAck) {
	ctx.mqttMu.RLock()
	client := ctx.MQTTClient
	ctx.mqttMu.RUnlock()
	if client == nil || !client.IsConnected() {
		return
	}
	ctx.configMu.RLock()
	topic := strings.TrimSpace(ctx.Config.MQTTStatusTopic)
	ctx.configMu.RUnlock()
	if topic == "" {
		return
	}
	b, err := json.Marshal(ack)
	if err != nil {
		return
	}
	if t := client.Publish(topic, 1, false, b); t.Wait() && t.Error() != nil {
		log.Printf("WARNING: MQTT publish status: %v", t.Error())
	}
}

// webhookHTTPClient is used for configurable event and heartbeat callback POSTs.
var webhookHTTPClient = &http.Client{Timeout: 25 * time.Second}

// auditLogInsertMu serializes SQLite inserts into logs to reduce SQLITE_BUSY contention.
var auditLogInsertMu sync.Mutex

// auditLogEvent records an event activity row in logs (same semantic events as webhooks). Runs even when
// webhook_event_enabled is false. detail_json matches webhook detail maps (no PINs or secrets).
func auditLogEvent(ctx *AppContext, event string, detail map[string]any) {
	if ctx == nil || ctx.DB == nil || strings.TrimSpace(event) == "" {
		return
	}
	ctx.configMu.RLock()
	cid := ctx.Config.MQTTClientID
	ctx.configMu.RUnlock()
	det := detail
	if det == nil {
		det = map[string]any{}
	}
	b, err := json.Marshal(det)
	if err != nil {
		log.Printf("WARNING: audit log JSON marshal: %v", err)
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	auditLogInsertMu.Lock()
	_, err = ctx.DB.Exec(`INSERT INTO logs (created_at, event_type, event_name, device_client_id, detail_json) VALUES (?, ?, ?, ?, ?)`,
		ts, "event", event, cid, string(b))
	auditLogInsertMu.Unlock()
	if err != nil {
		log.Printf("WARNING: audit log insert: %v", err)
	}
}

func webhookPostJSONAsync(url string, tokenEnabled bool, token string, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("WARNING: webhook JSON marshal: %v", err)
		return
	}
	u := strings.TrimSpace(url)
	tok := strings.TrimSpace(token)
	go func() {
		reqCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			log.Printf("WARNING: webhook request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "virtualkeyz2-webhook/1.0")
		if tokenEnabled && tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := webhookHTTPClient.Do(req)
		if err != nil {
			log.Printf("WARNING: webhook POST %q: %v", u, err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			log.Printf("WARNING: webhook POST %q: HTTP %s", u, resp.Status)
		}
	}()
}

// fireEventWebhook POSTs JSON to webhook_event_url when webhook_event_enabled and URL are set.
// Payload never includes PINs or tokens. Optional Bearer token when webhook_event_token_enabled and token non-empty.
func fireEventWebhook(ctx *AppContext, event string, detail map[string]any) {
	auditLogEvent(ctx, event, detail)
	ctx.configMu.RLock()
	if !ctx.Config.WebhookEventEnabled {
		ctx.configMu.RUnlock()
		return
	}
	url := strings.TrimSpace(ctx.Config.WebhookEventURL)
	tokEn := ctx.Config.WebhookEventTokenEnabled
	tok := ctx.Config.WebhookEventToken
	cid := ctx.Config.MQTTClientID
	ctx.configMu.RUnlock()
	if url == "" {
		return
	}
	pay := map[string]any{
		"type":             "event",
		"event":            event,
		"timestamp":        time.Now().UTC().Format(time.RFC3339Nano),
		"device_client_id": cid,
	}
	for k, v := range detail {
		pay[k] = v
	}
	webhookPostJSONAsync(url, tokEn, tok, pay)
}

// fireHeartbeatWebhook POSTs to webhook_heartbeat_url on each heartbeat tick when enabled.
func fireHeartbeatWebhook(ctx *AppContext) {
	ctx.configMu.RLock()
	if !ctx.Config.WebhookHeartbeatEnabled {
		ctx.configMu.RUnlock()
		return
	}
	url := strings.TrimSpace(ctx.Config.WebhookHeartbeatURL)
	tokEn := ctx.Config.WebhookHeartbeatTokenEnabled
	tok := ctx.Config.WebhookHeartbeatToken
	cid := ctx.Config.MQTTClientID
	interval := ctx.Config.HeartbeatInterval
	ctx.configMu.RUnlock()
	if url == "" {
		return
	}
	if interval <= 0 {
		interval = 60 * time.Second
	}
	pay := map[string]any{
		"type":               "heartbeat",
		"event":              "heartbeat",
		"timestamp":          time.Now().UTC().Format(time.RFC3339Nano),
		"device_client_id":   cid,
		"heartbeat_interval": interval.String(),
	}
	webhookPostJSONAsync(url, tokEn, tok, pay)
}

func startHeartbeatAPI(ctx *AppContext) {
	for {
		ctx.configMu.RLock()
		d := ctx.Config.HeartbeatInterval
		ctx.configMu.RUnlock()
		if d <= 0 {
			d = 60 * time.Second
		}
		timer := time.NewTimer(d)
		<-timer.C
		log.Println("DEBUG: Heartbeat tick (webhook if configured).")
		fireHeartbeatWebhook(ctx)
	}
}

func startWebServer(ctx *AppContext) *http.Server {
	router := gin.Default()

	// REST API with Token Support & ACL [cite: 7]
	api := router.Group("/api")
	api.Use(TokenAuthMiddleware())
	{
		api.POST("/remote-control", func(c *gin.Context) {
			// Trigger GPIO based on remote REST command [cite: 7]
			c.JSON(http.StatusOK, gin.H{"status": "door_opened"})
		})
	}

	// Local Web Interface for Config and Monitoring
	router.GET("/admin", func(c *gin.Context) {
		c.String(http.StatusOK, "Local Configuration Interface")
	})

	srv := &http.Server{
		Addr:    ":8080",
		Handler: router,
	}
	go func() {
		log.Println("INFO: Starting Web Server on port 8080")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("CRITICAL: Web server: %v", err)
		}
	}()
	return srv
}

func monitorDoorSensors(ctx *AppContext) {
	if ctx.GPIO == nil {
		log.Println("INFO: Door sensor monitor disabled (GPIO not available).")
		return
	}
	if !ctx.GPIO.DoorSensorConfigured() {
		log.Println("INFO: Door sensor monitor disabled (no door sensor pin configured).")
		return
	}

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	var openSince time.Time
	var warned bool
	first := true
	var wasOpen bool

	for range tick.C {
		ctx.configMu.RLock()
		warnAfter := ctx.Config.DoorOpenWarningAfter
		closedLow := ctx.Config.DoorSensorClosedIsLow
		ctx.configMu.RUnlock()
		if warnAfter <= 0 {
			warnAfter = 10 * time.Second
		}

		open := ctx.GPIO.DoorIsOpen(closedLow)

		if first {
			first = false
			wasOpen = open
			if open {
				log.Printf("INFO: Door status: OPEN (sensor GPIO %d).", ctx.GPIOSettings.DoorSensorPin)
				openSince = time.Now()
			} else {
				log.Printf("INFO: Door status: CLOSED (sensor GPIO %d).", ctx.GPIOSettings.DoorSensorPin)
				openSince = time.Time{}
			}
			warned = false
			continue
		} else if open != wasOpen {
			wasOpen = open
			pin := int(ctx.GPIOSettings.DoorSensorPin)
			if open {
				log.Printf("INFO: Door is now OPEN (sensor GPIO %d).", ctx.GPIOSettings.DoorSensorPin)
				openSince = time.Now()
				warned = false
				fireEventWebhook(ctx, "door_opened", map[string]any{"door_sensor_gpio": pin})
			} else {
				log.Printf("INFO: Door is now CLOSED (sensor GPIO %d).", ctx.GPIOSettings.DoorSensorPin)
				openSince = time.Time{}
				warned = false
				fireEventWebhook(ctx, "door_closed", map[string]any{"door_sensor_gpio": pin})
			}
		}

		if !open {
			continue
		}
		if openSince.IsZero() {
			openSince = time.Now()
		}
		if warned {
			continue
		}
		if time.Since(openSince) >= warnAfter {
			log.Printf("WARNING: Door open longer than %v (door sensor GPIO %d).", warnAfter, ctx.GPIOSettings.DoorSensorPin)
			warned = true
			fireEventWebhook(ctx, "door_open_timeout", map[string]any{
				"door_sensor_gpio": int(ctx.GPIOSettings.DoorSensorPin),
				"threshold":        warnAfter.String(),
			})
		}
	}
}

// pinMaskLineWidth is the column where the rightmost asterisk is placed (right-aligned mask).
const pinMaskLineWidth = 80

func displayController(ctx *AppContext) {
	// Manages DSI screen, displays greeting messages [cite: 3, 9]
	// Displays random QR code for external mobile phone interaction
	for n := range ctx.PinDisplayDigits {
		if n <= 0 {
			continue
		}
		mask := strings.Repeat("*", n)
		log.Printf("DEBUG: Pin Digits Count %s", mask)
	}
}

// playSoundSync plays a WAV via ALSA aplay; blocks until finished.
func playSoundSync(cfg DeviceConfig, path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		log.Printf("DEBUG: sound skipped (not found): %s", path)
		return
	}
	args := []string{"-q"}
	if cfg.SoundCardName != "" {
		args = append(args, "-D", cfg.SoundCardName)
	}
	args = append(args, path)
	cmd := exec.Command("aplay", args...)
	if err := cmd.Run(); err != nil {
		log.Printf("WARNING: aplay failed for %s: %v", path, err)
	}
}

func playSoundAsync(cfg DeviceConfig, path string) {
	if path == "" {
		return
	}
	go playSoundSync(cfg, path)
}

func manageHardwareHeartbeat(bcm uint8) {
	// Blinks onboard heartbeat LED to indicate software is running
	pin := rpio.Pin(bcm)
	pin.Output()
	for {
		pin.Toggle()
		time.Sleep(500 * time.Millisecond)
	}
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return len(s) > 0
}

func pinDigitCount(pin string) int {
	n := 0
	for _, r := range pin {
		if unicode.IsDigit(r) {
			n++
		}
	}
	return n
}

func notifyPinDisplay(ctx *AppContext, pin string) {
	if ctx.PinDisplayDigits == nil {
		return
	}
	ctx.PinDisplayDigits <- pinDigitCount(pin)
}

func TokenAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Validates tokens for the REST HTTP API [cite: 7]
		c.Next()
	}
}

// Use evtest in linux to test the capabilities of the keypad.
// keypadRole is "entry", "exit", or "" for single-keypad modes (used in logs, webhooks, and dual-keypad setups).
func runKeypadListener(ctx *AppContext, devicePath, keypadRole string) {
	dev, err := evdev.Open(devicePath)
	if err != nil {
		log.Printf("CRITICAL: Failed to open USB keypad at %s: %v", devicePath, err)
		return
	}
	defer dev.File.Close()

	kpLog := keypadLogTag(keypadRole)
	if keypadRole != "" {
		log.Printf("INFO: Keypad role %q: %s @ %s", keypadRole, dev.Name, devicePath)
	} else {
		log.Printf("INFO: Listening to USB Keypad: %s @ %s", dev.Name, devicePath)
	}

	// Block in a goroutine so the main loop can select on inter-digit/session timers and react immediately.
	eventCh := make(chan *evdev.InputEvent, 64)
	go func() {
		for {
			ev, rerr := dev.ReadOne()
			if rerr != nil {
				log.Printf("ERROR: Failed to read from keypad: %v", rerr)
				time.Sleep(1 * time.Second)
				continue
			}
			if ev != nil {
				eventCh <- ev
			}
		}
	}()

	var pinBuffer string
	interTimer := time.NewTimer(time.Hour)
	interTimer.Stop()
	sessionTimer := time.NewTimer(time.Hour)
	sessionTimer.Stop()

	drainTimer := func(t *time.Timer) {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	}

	stopEntryTimers := func() {
		drainTimer(interTimer)
		drainTimer(sessionTimer)
	}

	restartInterDigit := func() {
		ctx.configMu.RLock()
		d := ctx.Config.KeypadInterDigitTimeout
		ctx.configMu.RUnlock()
		if d <= 0 {
			d = 5 * time.Second
		}
		d = clampDuration(d, 3*time.Second, 10*time.Second)
		drainTimer(interTimer)
		interTimer.Reset(d)
	}

	startSessionFromFirstDigit := func() {
		ctx.configMu.RLock()
		d := ctx.Config.KeypadSessionTimeout
		ctx.configMu.RUnlock()
		if d <= 0 {
			d = 30 * time.Second
		}
		d = clampDuration(d, 10*time.Second, 60*time.Second)
		drainTimer(sessionTimer)
		sessionTimer.Reset(d)
	}

	for {
		select {
		case ev := <-eventCh:
			if ev.Type == evdev.EV_KEY && ev.Value == 1 {
				restartInterDigit()

				ctx.configMu.RLock()
				cfg := ctx.Config
				ctx.configMu.RUnlock()
				playSoundSync(cfg, cfg.SoundKeypress)

				ke := evdev.NewKeyEvent(ev)
				char := ""

				switch ke.Scancode {
				case evdev.KEY_KP0, evdev.KEY_0:
					char = "0"
					log.Printf("DEBUG: [%s keypad] digit 0 pressed", kpLog)
				case evdev.KEY_KP1, evdev.KEY_1:
					char = "1"
					log.Printf("DEBUG: [%s keypad] digit 1 pressed", kpLog)
				case evdev.KEY_KP2, evdev.KEY_2:
					char = "2"
					log.Printf("DEBUG: [%s keypad] digit 2 pressed", kpLog)
				case evdev.KEY_KP3, evdev.KEY_3:
					char = "3"
					log.Printf("DEBUG: [%s keypad] digit 3 pressed", kpLog)
				case evdev.KEY_KP4, evdev.KEY_4:
					char = "4"
					log.Printf("DEBUG: [%s keypad] digit 4 pressed", kpLog)
				case evdev.KEY_KP5, evdev.KEY_5:
					char = "5"
					log.Printf("DEBUG: [%s keypad] digit 5 pressed", kpLog)
				case evdev.KEY_KP6, evdev.KEY_6:
					char = "6"
					log.Printf("DEBUG: [%s keypad] digit 6 pressed", kpLog)
				case evdev.KEY_KP7, evdev.KEY_7:
					char = "7"
					log.Printf("DEBUG: [%s keypad] digit 7 pressed", kpLog)
				case evdev.KEY_KP8, evdev.KEY_8:
					char = "8"
					log.Printf("DEBUG: [%s keypad] digit 8 pressed", kpLog)
				case evdev.KEY_KP9, evdev.KEY_9:
					char = "9"
					log.Printf("DEBUG: [%s keypad] digit 9 pressed", kpLog)
				case evdev.KEY_KPSLASH, evdev.KEY_SLASH:
					char = "/"
					log.Printf("DEBUG: [%s keypad] slash pressed", kpLog)
				case evdev.KEY_KPMINUS:
					char = "-"
					log.Printf("DEBUG: [%s keypad] minus pressed", kpLog)
				case evdev.KEY_KPPLUS:
					char = "+"
					log.Printf("DEBUG: [%s keypad] plus pressed", kpLog)
				case evdev.KEY_KPDOT:
					char = "."
					log.Printf("DEBUG: [%s keypad] dot pressed", kpLog)
				case evdev.KEY_BACKSPACE:
					if len(pinBuffer) > 0 {
						pinBuffer = pinBuffer[:len(pinBuffer)-1]
						if len(pinBuffer) == 0 {
							drainTimer(sessionTimer)
						}
					}
					notifyPinDisplay(ctx, pinBuffer)
				case evdev.KEY_KPENTER, evdev.KEY_ENTER:
					log.Printf("INFO: PIN submission initiated (%s keypad).", kpLog)
					processPIN(ctx, pinBuffer, keypadRole)
					pinBuffer = ""
					stopEntryTimers()
					notifyPinDisplay(ctx, pinBuffer)
				case evdev.KEY_KPASTERISK:
					log.Printf("INFO: 'Call for Help' triggered via USB keypad (%s).", kpLog)
					triggerCallForHelp(ctx)
					pinBuffer = ""
					stopEntryTimers()
					notifyPinDisplay(ctx, pinBuffer)
				}

				if char != "" {
					wasEmpty := len(pinBuffer) == 0
					pinBuffer += char
					if wasEmpty {
						startSessionFromFirstDigit()
					}
					notifyPinDisplay(ctx, pinBuffer)

					ctx.configMu.RLock()
					pinLen := ctx.Config.PinLength
					ctx.configMu.RUnlock()
					if pinLen > 0 && len(pinBuffer) >= pinLen && isAllDigits(pinBuffer) {
						log.Printf("INFO: PIN auto-submitted after %d digits (%s keypad).", pinLen, kpLog)
						processPIN(ctx, pinBuffer, keypadRole)
						pinBuffer = ""
						stopEntryTimers()
						notifyPinDisplay(ctx, pinBuffer)
					}
				}
			}

		case <-interTimer.C:
			if len(pinBuffer) > 0 {
				ctx.configMu.RLock()
				lim := ctx.Config.KeypadInterDigitTimeout
				ctx.configMu.RUnlock()
				log.Printf("WARNING: Keypad inter-digit timeout (%s) expired; PIN buffer cleared (%s keypad).", lim, kpLog)
				pinBuffer = ""
				stopEntryTimers()
				notifyPinDisplay(ctx, pinBuffer)
			}

		case <-sessionTimer.C:
			if len(pinBuffer) > 0 {
				ctx.configMu.RLock()
				lim := ctx.Config.KeypadSessionTimeout
				ctx.configMu.RUnlock()
				log.Printf("WARNING: Keypad session timeout (%s) expired; PIN buffer cleared (%s keypad).", lim, kpLog)
				pinBuffer = ""
				stopEntryTimers()
				notifyPinDisplay(ctx, pinBuffer)
			}
		}
	}
}

// pinRejectWithStreak plays reject sound, increments wrong-PIN streak, fires webhook, optional buzzer/lockout.
func (ctx *AppContext) pinRejectWithStreak(cfg DeviceConfig, keypadRole string, buzzerBCM uint8, feedbackDelay time.Duration, webhookReason string, extra map[string]any) {
	playSoundSync(cfg, cfg.SoundPinReject)
	ctx.pinFailMu.Lock()
	ctx.pinFailSeq++
	failCount := ctx.pinFailSeq
	ctx.pinFailMu.Unlock()
	wh := map[string]any{"reason": webhookReason, "wrong_pin_streak": failCount, "keypad_role": keypadRole}
	for k, v := range extra {
		wh[k] = v
	}
	fireEventWebhook(ctx, "pin_rejected", wh)
	ctx.configMu.RLock()
	buzzTh := ctx.Config.PinRejectBuzzerAfterAttempts
	buzzDur := ctx.Config.BuzzerRelayPulseDuration
	lockN := ctx.Config.PinLockoutAfterAttempts
	lockDur := ctx.Config.PinLockoutDuration
	lockOn := ctx.Config.PinLockoutEnabled
	ctx.configMu.RUnlock()
	buzzFire := buzzTh > 0 && failCount >= buzzTh && failCount%buzzTh == 0
	lockFire := lockOn && lockN > 0 && failCount >= lockN
	if buzzFire {
		log.Printf("INFO: Wrong PIN count %d; pulsing buzzer relay (GPIO %d).", failCount, buzzerBCM)
		fireEventWebhook(ctx, "wrong_pin_buzzer", map[string]any{"wrong_pin_streak": failCount, "buzzer_relay_gpio": int(buzzerBCM)})
		if ctx.GPIO != nil {
			ctx.GPIO.ActionPulse("buzzer", buzzDur)
		} else {
			log.Println("WARNING: GPIO unavailable; buzzer relay pulse skipped.")
		}
	}
	if lockFire {
		ctx.keypadArmLockout(lockDur)
		ctx.pinFailMu.Lock()
		ctx.pinFailSeq = 0
		ctx.pinFailMu.Unlock()
		log.Printf("WARNING: Keypad lockout for %s (configured pin_lockout_duration) after %d failed PIN attempts.", lockDur, failCount)
		fireEventWebhook(ctx, "keypad_lockout_activated", map[string]any{
			"duration":        lockDur.String(),
			"failed_attempts": failCount,
			"lockout_enabled": lockOn,
		})
	}
	time.Sleep(feedbackDelay)
}

func processPIN(ctx *AppContext, pin string, keypadRole string) {
	// Query local SQLite for permissions
	// Validate PIN against door constraints
	// Trigger Relay, Sound Event, MQTT Update, and Logging
	pin = strings.TrimSpace(pin)
	if pin == "" {
		return
	}
	log.Printf("DEBUG: Processing PIN from %s keypad (length %d).", keypadLogTag(keypadRole), len(pin))
	ctx.configMu.RLock()
	cfg := ctx.Config
	doorBCM := ctx.GPIOSettings.DoorRelayPin
	buzzerBCM := ctx.GPIOSettings.BuzzerRelayPin
	feedbackDelay := cfg.PinEntryFeedbackDelay
	ctx.configMu.RUnlock()

	override := strings.TrimSpace(cfg.PinLockoutOverridePin)
	if override != "" && pin == override {
		ctx.keypadClearLockout()
		ctx.pinFailMu.Lock()
		ctx.pinFailSeq = 0
		ctx.pinFailMu.Unlock()
		log.Printf("INFO: Keypad lockout cleared by override PIN (%s keypad).", keypadLogTag(keypadRole))
		fireEventWebhook(ctx, "keypad_lockout_override", map[string]any{"keypad_role": keypadRole})
		playSoundSync(cfg, cfg.SoundPinOK)
		time.Sleep(feedbackDelay)
		return
	}

	if ctx.keypadLockoutActive() {
		log.Printf("INFO: PIN rejected (keypad lockout; %s keypad).", keypadLogTag(keypadRole))
		fireEventWebhook(ctx, "pin_rejected", map[string]any{"reason": "keypad_lockout", "keypad_role": keypadRole})
		playSoundSync(cfg, cfg.SoundPinReject)
		time.Sleep(feedbackDelay)
		return
	}

	cred := ctx.accessCredentialForPIN(pin)
	pinOK := cred.OK
	credLabel := cred.Label
	modePre := NormalizeKeypadOperationMode(cfg.KeypadOperationMode)
	if pinOK && modePre == ModeAccessDualUSBKeypad && keypadRole == "exit" && cfg.DualKeypadRejectExitWithoutEntry && ctx.dualKeypadExitWouldMismatch(pin) {
		log.Printf("INFO: PIN rejected (exit keypad; no recorded entry for this credential; door not opened).")
		ex := map[string]any{}
		if credLabel != "" {
			ex["credential_label"] = credLabel
		}
		ctx.pinRejectWithStreak(cfg, keypadRole, buzzerBCM, feedbackDelay, "exit_without_recorded_entry", ex)
		return
	}

	if pinOK {
		doorID := ctx.effectiveAccessDoorID()
		if ok, schedReason := ctx.accessScheduleAllows(pin, doorID, time.Now(), cred.ViaFallback); !ok {
			log.Printf("INFO: PIN rejected (access schedule: %s; door=%q).", schedReason, doorID)
			ex := map[string]any{"schedule_reason": schedReason, "access_control_door_id": doorID}
			if credLabel != "" {
				ex["credential_label"] = credLabel
			}
			ctx.pinRejectWithStreak(cfg, keypadRole, buzzerBCM, feedbackDelay, "access_schedule", ex)
			return
		}
		if isElevatorKeypadMode(modePre) {
			elevID := ctx.effectiveAccessElevatorID()
			if ok, schedReason := ctx.accessScheduleAllowsElevator(pin, elevID, time.Now(), cred.ViaFallback); !ok {
				log.Printf("INFO: PIN rejected (access schedule: %s; elevator=%q).", schedReason, elevID)
				ex := map[string]any{"schedule_reason": schedReason, "access_control_elevator_id": elevID}
				if credLabel != "" {
					ex["credential_label"] = credLabel
				}
				ctx.pinRejectWithStreak(cfg, keypadRole, buzzerBCM, feedbackDelay, "access_schedule", ex)
				return
			}
		}

		ctx.pinFailMu.Lock()
		ctx.pinFailSeq = 0
		ctx.pinFailMu.Unlock()
		ctx.keypadClearLockout()

		mode := modePre
		kTag := keypadLogTag(keypadRole)
		credTag := credLabel
		if credTag == "" {
			credTag = "legacy_or_unlabeled"
		}

		switch mode {
		case ModeElevatorWaitFloorButtons:
			cabSense := normalizeElevatorWaitFloorCabSense(cfg.ElevatorWaitFloorCabSense)
			ctx.elevatorMu.Lock()
			ctx.elevatorGrantPIN = pin
			ctx.elevatorGrantViaFallback = cred.ViaFallback
			ctx.elevatorMu.Unlock()
			log.Printf("INFO: PIN accepted (elevator wait-floor; %s keypad; credential=%s); enable window started (cab_sense=%s).", kTag, credTag, cabSense)
			playSoundSync(cfg, cfg.SoundPinOK)
			startElevatorFloorWaitGrant(ctx, cfg)
			fireEventWebhook(ctx, "pin_accepted", map[string]any{
				"operation_mode":                mode,
				"keypad_role":                   keypadRole,
				"credential_label":              credLabel,
				"elevator_phase":                "wait_floor_input",
				"elevator_wait_floor_cab_sense": cabSense,
				"door_relay_gpio":               int(doorBCM),
				"floor_wait_until":              cfg.ElevatorFloorWaitTimeout.String(),
			})
			time.Sleep(feedbackDelay)
			return

		case ModeElevatorPredefinedFloor:
			ex, okElev := activateElevatorPredefinedFloor(ctx, cfg, kTag, credTag, pin, cred.ViaFallback)
			if !okElev {
				aclIdx := ctx.elevatorPredefinedDispatchIndexForACL(cfg)
				elevDenyID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
				denyEx := map[string]any{
					"keypad_role":                keypadRole,
					"elevator_floor_index":       aclIdx,
					"access_control_elevator_id": elevDenyID,
				}
				if credLabel != "" {
					denyEx["credential_label"] = credLabel
				}
				if ctx.DB != nil && elevDenyID != "" {
					denyEx["elevator_floor_label"] = elevatorFloorLogLabel(ctx.DB, elevDenyID, aclIdx)
				}
				playSoundSync(cfg, cfg.SoundPinReject)
				fireEventWebhook(ctx, "elevator_floor_denied", denyEx)
				time.Sleep(feedbackDelay)
				return
			}
			playSoundSync(cfg, cfg.SoundPinOK)
			wh := map[string]any{
				"operation_mode":   mode,
				"keypad_role":      keypadRole,
				"credential_label": credLabel,
				"door_relay_gpio":  int(doorBCM),
			}
			for k, v := range ex {
				wh[k] = v
			}
			fireEventWebhook(ctx, "pin_accepted", wh)
			time.Sleep(feedbackDelay)
			return

		default:
			var areaTotal, insideThis int
			var occMismatch string
			if mode == ModeAccessDualUSBKeypad && (keypadRole == "entry" || keypadRole == "exit") {
				areaTotal, insideThis, occMismatch = ctx.adjustDualKeypadOccupancy(pin, keypadRole)
				log.Printf("INFO: PIN accepted (dual USB %s keypad; credential=%s; people_in_area_total=%d; this_credential_inside=%d); door relay GPIO %d.",
					keypadRole, credTag, areaTotal, insideThis, doorBCM)
				if occMismatch != "" {
					log.Printf("WARNING: Dual keypad occupancy: %s (%s keypad; credential=%s) — door still opened.", occMismatch, keypadRole, credTag)
				}
			} else {
				if mode == ModeAccessDualUSBKeypad && keypadRole == "" {
					log.Printf("WARNING: access_dual_usb_keypad but keypad role unknown; occupancy not updated. Use distinct keypad_exit_evdev_path.")
				}
				log.Printf("INFO: PIN accepted (mode=%s %s keypad; credential=%s); door relay GPIO %d.", mode, kTag, credTag, doorBCM)
			}
			playSoundSync(cfg, cfg.SoundPinOK)
			relPulsed := false
			if ctx.GPIO != nil {
				ctx.GPIO.ActionPulse("door", cfg.RelayPulseDuration)
				relPulsed = true
			} else {
				log.Println("WARNING: GPIO unavailable; relay pulse skipped.")
			}
			if pairedEntryPublishesToPeer(mode, cfg.PairPeerRole) {
				publishMQTTPairPeerPulse(ctx, cfg)
			}
			wh := map[string]any{
				"operation_mode":   mode,
				"keypad_role":      keypadRole,
				"credential_label": credLabel,
				"door_relay_gpio":  int(doorBCM),
				"relay_pulsed":     relPulsed,
			}
			if mode == ModeAccessDualUSBKeypad && (keypadRole == "entry" || keypadRole == "exit") {
				wh["access_area_occupancy_total"] = areaTotal
				wh["credential_inside_count"] = insideThis
				if occMismatch != "" {
					wh["occupancy_mismatch"] = occMismatch
				}
			}
			fireEventWebhook(ctx, "pin_accepted", wh)
			time.Sleep(feedbackDelay)
			return
		}
	}

	log.Printf("INFO: PIN rejected (%s keypad).", keypadLogTag(keypadRole))
	ctx.pinRejectWithStreak(cfg, keypadRole, buzzerBCM, feedbackDelay, "invalid_pin", nil)
}

func triggerCallForHelp(ctx *AppContext) {
	// Signal the centralized operations center via API/MQTT
	// to call the user on the IP-based intercom
}

func (ctx *AppContext) keypadLockoutActive() bool {
	ctx.configMu.RLock()
	enabled := ctx.Config.PinLockoutEnabled
	ctx.configMu.RUnlock()
	if !enabled {
		ctx.keypadClearLockout()
		return false
	}
	ctx.keypadLockoutMu.Lock()
	defer ctx.keypadLockoutMu.Unlock()
	if ctx.keypadLockoutUntil.IsZero() {
		return false
	}
	if time.Now().Before(ctx.keypadLockoutUntil) {
		return true
	}
	// Lockout period ended (first check after deadline, e.g. next keypress).
	if ctx.keypadLockoutEndTimer != nil {
		ctx.keypadLockoutEndTimer.Stop()
		ctx.keypadLockoutEndTimer = nil
	}
	if ctx.keypadLockoutEndLogOnce != nil {
		ctx.keypadLockoutEndLogOnce.Do(func() {
			log.Println("WARNING: Keypad lockout period ended; keypad accepting input again.")
		})
	}
	ctx.keypadLockoutUntil = time.Time{}
	ctx.keypadLockoutEndLogOnce = nil
	return false
}

func (ctx *AppContext) keypadArmLockout(d time.Duration) {
	if d <= 0 {
		return
	}
	ctx.configMu.RLock()
	on := ctx.Config.PinLockoutEnabled
	ctx.configMu.RUnlock()
	if !on {
		return
	}
	ctx.keypadLockoutMu.Lock()
	defer ctx.keypadLockoutMu.Unlock()
	if ctx.keypadLockoutEndTimer != nil {
		ctx.keypadLockoutEndTimer.Stop()
		ctx.keypadLockoutEndTimer = nil
	}
	ctx.keypadLockoutEndLogOnce = new(sync.Once)
	onceRef := ctx.keypadLockoutEndLogOnce
	ctx.keypadLockoutUntil = time.Now().Add(d)
	ctx.keypadLockoutEndTimer = time.AfterFunc(d, func() {
		ctx.keypadLockoutMu.Lock()
		defer ctx.keypadLockoutMu.Unlock()
		ctx.keypadLockoutEndTimer = nil
		if onceRef != nil {
			onceRef.Do(func() {
				log.Println("WARNING: Keypad lockout period ended; keypad accepting input again.")
			})
		}
		ctx.keypadLockoutUntil = time.Time{}
		ctx.keypadLockoutEndLogOnce = nil
	})
}

func (ctx *AppContext) keypadClearLockout() {
	ctx.keypadLockoutMu.Lock()
	defer ctx.keypadLockoutMu.Unlock()
	if ctx.keypadLockoutEndTimer != nil {
		ctx.keypadLockoutEndTimer.Stop()
		ctx.keypadLockoutEndTimer = nil
	}
	ctx.keypadLockoutEndLogOnce = nil
	ctx.keypadLockoutUntil = time.Time{}
}

func (ctx *AppContext) keypadLockoutRemaining() time.Duration {
	ctx.configMu.RLock()
	on := ctx.Config.PinLockoutEnabled
	ctx.configMu.RUnlock()
	if !on {
		return 0
	}
	ctx.keypadLockoutMu.Lock()
	defer ctx.keypadLockoutMu.Unlock()
	if ctx.keypadLockoutUntil.IsZero() || !time.Now().Before(ctx.keypadLockoutUntil) {
		return 0
	}
	return time.Until(ctx.keypadLockoutUntil)
}

func (ctx *AppContext) techHistoryMax() int {
	ctx.configMu.RLock()
	m := ctx.Config.TechMenuHistoryMax
	ctx.configMu.RUnlock()
	if m <= 0 {
		return 100
	}
	if m > 10000 {
		return 10000
	}
	return m
}

func (ctx *AppContext) techHistoryTrimToMax() {
	max := ctx.techHistoryMax()
	ctx.techHistMu.Lock()
	for len(ctx.techHist) > max {
		ctx.techHist = ctx.techHist[1:]
	}
	ctx.techHistMu.Unlock()
}

func (ctx *AppContext) techHistoryAppend(entry string) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return
	}
	max := ctx.techHistoryMax()
	ctx.techHistMu.Lock()
	ctx.techHist = append(ctx.techHist, entry)
	for len(ctx.techHist) > max {
		ctx.techHist = ctx.techHist[1:]
	}
	ctx.techHistMu.Unlock()
}

func (ctx *AppContext) techHistoryClear() {
	ctx.techHistMu.Lock()
	ctx.techHist = nil
	ctx.techHistMu.Unlock()
}

// techMenuCfgKeysForCompletion must match keys accepted by techMenuCfgSetValue (cfg set <key>).
// Keep sorted for predictable Tab order.
var techMenuCfgKeysForCompletion = []string{
	"access_control_door_id",
	"access_control_elevator_id",
	"access_schedule_apply_to_fallback_pin",
	"access_schedule_enforce",
	"buzzer_relay_active_low",
	"buzzer_relay_pin",
	"door_open_warning_after",
	"door_relay_active_low",
	"door_relay_pin",
	"door_sensor_closed_is_low",
	"door_sensor_pin",
	"dual_keypad_reject_exit_without_entry",
	"elevator_dispatch_active_low",
	"elevator_dispatch_pulse_duration",
	"elevator_dispatch_relay_pin",
	"elevator_enable_active_low",
	"elevator_enable_pulse_duration",
	"elevator_floor_dispatch_pins",
	"elevator_floor_dispatch_pulse_durations",
	"elevator_floor_input_pins",
	"elevator_floor_wait_timeout",
	"elevator_predefined_enable_pins",
	"elevator_predefined_floor",
	"elevator_predefined_floors",
	"elevator_wait_floor_cab_sense",
	"elevator_wait_floor_enable_pins",
	"entry_button_active_low",
	"entry_button_pin",
	"exit_button_active_low",
	"exit_button_pin",
	"fallback_access_pin",
	"heartbeat_interval",
	"heartbeat_led_pin",
	"keypad_evdev_path",
	"keypad_exit_evdev_path",
	"keypad_inter_digit_timeout",
	"keypad_operation_mode",
	"keypad_session_timeout",
	"log_level",
	"mcp23017_i2c_addr",
	"mcp23017_i2c_bus",
	"mqtt_broker",
	"mqtt_client_id",
	"mqtt_command_token",
	"mqtt_command_topic",
	"mqtt_enabled",
	"mqtt_pair_peer_topic",
	"mqtt_password",
	"mqtt_status_topic",
	"mqtt_username",
	"pair_peer_role",
	"pair_peer_token",
	"pin_entry_feedback_delay",
	"pin_length",
	"pin_lockout_after_attempts",
	"pin_lockout_duration",
	"pin_lockout_enabled",
	"pin_lockout_override_pin",
	"pin_reject_buzzer_after_attempts",
	"relay_output_mode",
	"relay_pulse_duration",
	"sound_card_name",
	"sound_keypress",
	"sound_pin_ok",
	"sound_pin_reject",
	"sound_shutdown",
	"sound_startup",
	"tech_menu_history_max",
	"tech_menu_prompt",
	"webhook_event_enabled",
	"webhook_event_token",
	"webhook_event_token_enabled",
	"webhook_event_url",
	"webhook_heartbeat_enabled",
	"webhook_heartbeat_token",
	"webhook_heartbeat_token_enabled",
	"webhook_heartbeat_url",
	"xl9535_i2c_addr",
	"xl9535_i2c_bus",
}

func techMenuRootCommands() []string {
	return []string{
		"...", "…",
		"1", "2", "3", "4", "5", "6", "7", "8", "9",
		"acl",
		"c", "cfg", "ch", "clear", "cls",
		"exit",
		"h", "help", "i", "kb", "kbd", "keypads",
		"m", "menu", "occ", "p", "q", "quit",
		"v", "z",
	}
}

// techMenuCfgSubcommands lists cfg second tokens for Tab completion (no single-letter aliases — they share prefixes and block LCP).
func techMenuCfgSubcommands() []string {
	return []string{
		"apply", "help", "history", "keys", "list", "live",
		"reread", "reload", "save", "set", "show", "write",
	}
}

func techMenuSplitForComplete(line string) (prefix []string, partial string, trailingSpace bool) {
	if len(line) == 0 {
		return nil, "", false
	}
	trailingSpace = line[len(line)-1] == ' ' || line[len(line)-1] == '\t'
	trimmed := strings.TrimRight(line, " \t")
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return nil, "", trailingSpace
	}
	if trailingSpace {
		return fields, "", true
	}
	if len(fields) == 1 {
		return nil, fields[0], false
	}
	return fields[:len(fields)-1], fields[len(fields)-1], false
}

func techMenuLowerPrefixSlice(s []string) []string {
	out := make([]string, len(s))
	for i, w := range s {
		out[i] = strings.ToLower(w)
	}
	return out
}

func techMenuFilterPrefixLower(cands []string, lowPrefix string) []string {
	var out []string
	for _, c := range cands {
		if strings.HasPrefix(strings.ToLower(c), lowPrefix) {
			out = append(out, c)
		}
	}
	return out
}

func techMenuLongestCommonPrefix(strs []string) string {
	if len(strs) == 0 {
		return ""
	}
	ref := strs[0]
	for i := range ref {
		rc := ref[i]
		for _, s := range strs[1:] {
			if i >= len(s) || s[i] != rc {
				return ref[:i]
			}
		}
	}
	return ref
}

func techMenuCompleteAddTrailingSpace(prefixLower []string, completed string) bool {
	if len(prefixLower) >= 1 && prefixLower[0] == "acl" {
		return techMenuACLCompleteAddSpace(prefixLower, completed)
	}
	c := strings.ToLower(completed)
	if c == "..." || c == "…" {
		return false
	}
	if len(prefixLower) == 0 {
		return true
	}
	if len(prefixLower) == 1 && prefixLower[0] == "cfg" {
		return true
	}
	if len(prefixLower) == 2 && prefixLower[0] == "cfg" && prefixLower[1] == "set" {
		return true
	}
	if len(prefixLower) == 1 && prefixLower[0] == "kb" {
		return true
	}
	return false
}

// techMenuTabCompleteLine returns an updated input line and whether to ring the terminal bell (no extension).
func techMenuTabCompleteLine(line string) (newLine string, bell bool) {
	prefix, partial, trail := techMenuSplitForComplete(line)
	pl := techMenuLowerPrefixSlice(prefix)

	var matches []string
	switch {
	case len(pl) >= 1 && pl[0] == "acl":
		var ok bool
		matches, ok = techMenuACLTabMatches(prefix, partial, trail)
		if !ok {
			matches = nil
		}
	case len(pl) == 0 && !trail:
		matches = techMenuFilterPrefixLower(techMenuRootCommands(), strings.ToLower(partial))
	case len(pl) == 1 && pl[0] == "cfg" && trail:
		matches = append([]string(nil), techMenuCfgSubcommands()...)
	case len(pl) == 1 && pl[0] == "cfg" && !trail:
		matches = techMenuFilterPrefixLower(techMenuCfgSubcommands(), strings.ToLower(partial))
	case len(pl) == 2 && pl[0] == "cfg" && pl[1] == "set" && trail:
		matches = append([]string(nil), techMenuCfgKeysForCompletion...)
	case len(pl) == 2 && pl[0] == "cfg" && pl[1] == "set" && !trail:
		matches = techMenuFilterPrefixLower(techMenuCfgKeysForCompletion, strings.ToLower(partial))
	case len(pl) == 1 && pl[0] == "kb" && trail:
		matches = []string{"all"}
	case len(pl) == 1 && pl[0] == "kb" && !trail:
		matches = techMenuFilterPrefixLower([]string{"all"}, strings.ToLower(partial))
	default:
		return line, true
	}

	if len(pl) >= 1 && pl[0] == "acl" && len(matches) == 0 {
		return line, true
	}

	if len(matches) == 0 {
		return line, true
	}

	lowPart := strings.ToLower(partial)
	if trail {
		lowPart = ""
	}

	var pick string
	addSpace := false

	if len(matches) == 1 {
		pick = matches[0]
		addSpace = techMenuCompleteAddTrailingSpace(pl, pick)
	} else {
		lcp := techMenuLongestCommonPrefix(matches)
		if !strings.HasPrefix(lcp, lowPart) || len(lcp) == len(lowPart) {
			return line, true
		}
		pick = lcp
		addSpace = false
	}

	newWords := append(append([]string{}, prefix...), pick)
	out := strings.Join(newWords, " ")
	if addSpace {
		out += " "
	}
	return out, false
}

func techMenuReadCSI(tty *os.File) ([]byte, error) {
	b := make([]byte, 1)
	if _, err := tty.Read(b); err != nil {
		return nil, err
	}
	if b[0] != '[' && b[0] != 'O' {
		return []byte{b[0]}, nil
	}
	out := []byte{b[0]}
	for {
		if _, err := tty.Read(b); err != nil {
			return out, err
		}
		out = append(out, b[0])
		if b[0] >= 0x40 && b[0] <= 0x7e {
			break
		}
	}
	return out, nil
}

func techMenuRedrawInputLine(line []byte) {
	techUILock.Lock()
	defer techUILock.Unlock()
	techMenuInputDraft = append([]byte(nil), line...)
	paintTechPromptRowUnlocked(os.Stdout)
	if len(line) > 0 {
		_, _ = os.Stdout.Write(line)
	}
}

// readTechMenuLine reads one line from /dev/tty with local echo, Up/Down history, and Backspace. Uses raw mode when possible.
func readTechMenuLine(ctx *AppContext, tty *os.File) (string, error) {
	fd := int(tty.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return readTechMenuLineFallback(tty)
	}
	defer func() {
		_ = term.Restore(fd, old)
		techUILock.Lock()
		techMenuInputDraft = nil
		techUILock.Unlock()
	}()

	var line []byte
	histIdx := -1
	redraw := func() { techMenuRedrawInputLine(line) }
	redraw()

	var upSeq = []byte("\x1b[A")
	var downSeq = []byte("\x1b[B")
	var upSS3 = []byte("\x1bOA")
	var downSS3 = []byte("\x1bOB")

	buf := make([]byte, 1)
	for {
		n, err := tty.Read(buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		b := buf[0]
		switch {
		case b == '\r' || b == '\n':
			techUILock.Lock()
			_, _ = fmt.Fprint(os.Stdout, "\n")
			techUILock.Unlock()
			return string(line), nil
		case b == 127 || b == 8:
			if len(line) > 0 {
				line = line[:len(line)-1]
				histIdx = -1
				redraw()
			}
		case b == '\t':
			histIdx = -1
			nl, bell := techMenuTabCompleteLine(string(line))
			line = []byte(nl)
			if bell {
				_, _ = tty.Write([]byte{'\a'})
			}
			redraw()
		case b == 27:
			csi, err := techMenuReadCSI(tty)
			if err != nil {
				return "", err
			}
			seq := append([]byte{27}, csi...)
			ctx.techHistMu.Lock()
			hist := append([]string(nil), ctx.techHist...)
			ctx.techHistMu.Unlock()
			nh := len(hist)
			switch {
			case bytes.Equal(seq, upSeq) || bytes.Equal(seq, upSS3):
				if nh == 0 {
					redraw()
					continue
				}
				if histIdx < 0 {
					histIdx = nh - 1
				} else if histIdx > 0 {
					histIdx--
				}
				line = append([]byte(nil), hist[histIdx]...)
				redraw()
			case bytes.Equal(seq, downSeq) || bytes.Equal(seq, downSS3):
				if histIdx < 0 {
					continue
				}
				if histIdx < nh-1 {
					histIdx++
					line = append([]byte(nil), hist[histIdx]...)
				} else {
					histIdx = -1
					line = nil
				}
				redraw()
			default:
				// ignore other escape sequences (arrows left/right, etc.)
			}
		case b >= 32 && b < 127:
			histIdx = -1
			line = append(line, b)
			redraw()
		case b == 3:
			line = nil
			histIdx = -1
			redraw()
		default:
			// ignore other control characters
		}
	}
}

func readTechMenuLineFallback(tty *os.File) (string, error) {
	r := bufio.NewReader(tty)
	s, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	s = strings.TrimSuffix(s, "\r")
	s = strings.TrimSuffix(s, "\n")
	return s, nil
}

// runTechnicianMenu reads from /dev/tty; menu text and logs use stdout. Bottom line is reserved for the configured prompt.
// shutdownNotify receives when the user enters "..." to exit the whole program (same shutdown path as SIGTERM).
func runTechnicianMenu(ctx *AppContext, shutdownNotify chan<- struct{}) {
	time.Sleep(800 * time.Millisecond)
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		releaseStartupLogBuffer(os.Stdout)
		log.Printf("DEBUG: Menu skipped (no /dev/tty: %v). Use -notechmenu to silence.", err)
		return
	}
	defer tty.Close()

	enableTechBottomTerminalLayout()

	const banner = `
--------------------------------------------------------------------------------
  Installation/Configuration & Service Main Menu 
  VirtualKeyz Version 2.0.0 by Suvir Kumar <suvir@dits.co.th>
--------------------------------------------------------------------------------
  Up/Down       Recall previous commands (see tech_menu_history_max in config)
  Tab           Complete commands (acl, cfg, cfg subcommands, cfg set keys, kb all)
  h   Redraw Main Menu
  c   Clear Screen 
  z Clear command history 
  -------------------------------------------------------------------------------
  1   Show configuration & GPIO map
  2   Door sensor: read state once
  3   Watch door sensor (~2s, sample every 200ms)
  4   Test pulse: door relay
  5   Test pulse: buzzer relay
  6   Show wrong-PIN streak counter
  7   Reset wrong-PIN streak counter
  8   Play test sound (key.wav)
  9   Play PIN Correct sound
  i   Network: Ethernet & Wi-Fi (IPv4, mask, gateway, DNS)
  p   System listening ports (all TCP + UDP, all processes)
  occ Dual USB keypad: show in-memory area occupancy (masked PINs + labels)
  kb  List USB keypads → stable USE_PATH (by-id/by-path; same as: go run ./tools/listkeypads -usb)
  kb all   List all keypad-related nodes (includes non-USB by-path)
  v   Software build version & release date (UTC)
  ch  Show changelog.txt (revision history)
--------------------------------------------------------------------------------
  acl help      SQLite access control: doors, PINs, groups, schedules, levels (Tab: acl …)
  cfg           Config help (same as: cfg keys)
  cfg list      Full settings (MQTT, log level, paths)
  cfg set K V   Set one key (snake_case); then: cfg apply | cfg save
  cfg apply     Live apply in-memory (log filter, prompt, MQTT reconnect)
  cfg save      Write virtualkeyz2.json (-config path)
  cfg reload    Read JSON from disk + live apply
 -------------------------------------------------------------------------------
  ... Quit Program (Shutdown)
 -------------------------------------------------------------------------------
 `
	// Startup: show status prompt only (enableTechBottomTerminalLayout already painted it); menu text on h/help.
	releaseStartupLogBuffer(os.Stdout)

	for {
		techUILock.Lock()
		paintTechPromptAndInputDraftUnlocked(os.Stdout)
		techUILock.Unlock()

		line, err := readTechMenuLine(ctx, tty)
		if err != nil {
			if errors.Is(err, io.EOF) {
				disableTechBottomTerminalLayout()
				return
			}
			log.Printf("DEBUG: Technician menu stdin closed: %v", err)
			disableTechBottomTerminalLayout()
			return
		}
		line = strings.TrimSpace(line)
		ctx.techHistoryAppend(line)
		if line == "" {
			continue
		}
		// Move cursor into the scrolling region so command output and logs do not land on the status row.
		key := strings.ToLower(line)
		if key != "..." && line != "…" && key != "c" && key != "cls" && key != "clear" {
			techUILock.Lock()
			if techBottomLineEnabled && techTerminalRows >= 2 {
				_, _ = fmt.Fprintf(os.Stdout, "\033[%d;1H\n", techTerminalRows-1)
			}
			techUILock.Unlock()
		}

		parts := strings.Fields(line)
		if len(parts) > 0 && strings.EqualFold(parts[0], "cfg") {
			techMenuHandleCfg(ctx, line, parts)
			continue
		}

		if len(parts) > 0 && strings.EqualFold(parts[0], "acl") {
			techMenuHandleACL(ctx, line, parts)
			continue
		}

		if key == "kb" || key == "kbd" || key == "keypads" || strings.HasPrefix(key, "kb ") {
			usbOnly := true
			kp := strings.Fields(key)
			if len(kp) >= 2 && (kp[1] == "all" || kp[1] == "-a") {
				usbOnly = false
			}
			techMenuSyncPrint(func(w io.Writer) {
				if err := keypadlist.Fprint(w, usbOnly); err != nil {
					fmt.Fprintf(w, "%v\n", err)
				}
			})
			log.Println("INFO: Technician menu: keypad / evdev list (kb).")
			continue
		}

		switch key {
		case "...", "…":
			disableTechBottomTerminalLayout()
			terminalHardReset()
			log.Println("INFO: Shutdown requested from technician menu (...); terminal reset.")
			select {
			case shutdownNotify <- struct{}{}:
			default:
			}
			return
		case "c", "cls", "clear":
			techMenuClearScreenAndRelayout()
			log.Println("INFO: Technician menu: screen cleared.")
		case "q", "quit", "exit":
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Technician menu closed.") })
			log.Println("INFO: Technician debug menu exited (service continues).")
			disableTechBottomTerminalLayout()
			return
		case "h", "?", "help", "m", "menu":
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprint(w, banner) })
		case "1":
			techMenuSyncPrint(func(w io.Writer) { techMenuShowConfig(w, ctx) })
			log.Println("INFO: Technician menu: printed configuration.")
		case "2":
			techMenuDoorSensorOnce(ctx)
		case "3":
			techMenuDoorSensorWatch(ctx)
		case "4":
			techMenuPulse(ctx, "door")
		case "5":
			techMenuPulse(ctx, "buzzer")
		case "6":
			n := ctx.WrongPINCount()
			ctx.configMu.RLock()
			thr := ctx.Config.PinRejectBuzzerAfterAttempts
			ctx.configMu.RUnlock()
			techMenuSyncPrint(func(w io.Writer) {
				fmt.Fprintf(w, "Wrong-PIN streak: %d (buzzer at %d)\n", n, thr)
			})
			log.Printf("INFO: Technician menu: wrong-PIN streak=%d", n)
		case "7":
			ctx.ResetWrongPINCount()
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Wrong-PIN streak reset.") })
			log.Println("INFO: Technician menu: wrong-PIN streak reset.")
		case "8":
			log.Println("INFO: Technician menu: playing key sound test.")
			ctx.configMu.RLock()
			cfg8 := ctx.Config
			ctx.configMu.RUnlock()
			playSoundSync(cfg8, cfg8.SoundKeypress)
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Sound finished (key.wav).") })
		case "9":
			log.Println("INFO: Technician menu: playing PIN OK sound test.")
			ctx.configMu.RLock()
			cfg9 := ctx.Config
			ctx.configMu.RUnlock()
			playSoundSync(cfg9, cfg9.SoundPinOK)
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Sound finished (pin_ok).") })
		case "i":
			techMenuSyncPrint(func(w io.Writer) { techMenuWriteNetworkDiag(w) })
			log.Println("INFO: Technician menu: printed network snapshot (Ethernet / Wi-Fi / DNS).")
		case "p":
			techMenuSyncPrint(func(w io.Writer) { techMenuWriteProcListenPorts(w) })
			log.Println("INFO: Technician menu: printed system-wide listening TCP/UDP ports.")
		case "occ":
			techMenuSyncPrint(func(w io.Writer) { techMenuWriteOccupancy(w, ctx) })
			log.Println("INFO: Technician menu: printed dual-keypad occupancy snapshot.")
		case "v":
			techMenuSyncPrint(func(w io.Writer) { techMenuShowSoftwareVersion(w) })
			log.Printf("INFO: Technician menu: software build %s (%s).", SoftwareVersion, SoftwareReleaseUTC)
		case "ch":
			techMenuSyncPrint(func(w io.Writer) { techMenuShowChangelog(w) })
			log.Println("INFO: Technician menu: printed changelog.")
		case "z":
			ctx.techHistoryClear()
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Command history cleared.") })
			log.Println("INFO: Technician menu: command history cleared (key z).")
		default:
			techMenuSyncPrint(func(w io.Writer) {
				fmt.Fprintf(w, "Unknown choice %q. Press h for menu.\n", line)
			})
		}
	}
}

// techMenuChangelogPath returns the first readable changelog.txt (executable directory, cwd, or relative).
func techMenuChangelogPath() string {
	seen := make(map[string]struct{})
	var ordered []string
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		ordered = append(ordered, p)
	}
	if exe, err := os.Executable(); err == nil {
		add(filepath.Join(filepath.Dir(exe), "changelog.txt"))
	}
	if wd, err := os.Getwd(); err == nil {
		add(filepath.Join(wd, "changelog.txt"))
	}
	add("changelog.txt")
	for _, p := range ordered {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func techMenuShowSoftwareVersion(w io.Writer) {
	fmt.Fprintf(w, "\n--- Software build ---\n")
	fmt.Fprintf(w, "  version:           %s\n", SoftwareVersion)
	fmt.Fprintf(w, "  release (UTC):     %s\n", SoftwareReleaseUTC)
	if t, err := time.Parse(time.RFC3339, SoftwareReleaseUTC); err == nil {
		fmt.Fprintf(w, "  release (local):   %s\n", t.Local().Format(time.RFC3339))
	}
	fmt.Fprintf(w, "  product:           VirtualKeyz 2.x by Suvir Kumar <suvir@dits.co.th>\n")
	fmt.Fprintf(w, "  bump script:       ./tools/bump-version.sh \"description\" (increments +0.01, updates changelog)\n\n")
}

func techMenuShowChangelog(w io.Writer) {
	p := techMenuChangelogPath()
	if p == "" {
		fmt.Fprintln(w, "\nchangelog.txt not found (place next to the binary, in cwd, or project root).\n")
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		fmt.Fprintf(w, "\nCould not read changelog: %v\n\n", err)
		return
	}
	fmt.Fprintf(w, "\n--- %s (%s) ---\n", filepath.Base(p), p)
	fmt.Fprint(w, string(b))
	if len(b) > 0 && b[len(b)-1] != '\n' {
		fmt.Fprint(w, "\n")
	}
	fmt.Fprintln(w, "")
}

func techMenuWriteOccupancy(w io.Writer, ctx *AppContext) {
	ctx.configMu.RLock()
	mode := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
	rejectExit := ctx.Config.DualKeypadRejectExitWithoutEntry
	ctx.configMu.RUnlock()

	fmt.Fprintf(w, "\n--- Dual keypad occupancy (in-memory until restart) ---\n")
	fmt.Fprintf(w, "  keypad_operation_mode: %s\n", mode)
	fmt.Fprintf(w, "  dual_keypad_reject_exit_without_entry: %v\n", rejectExit)
	if mode != ModeAccessDualUSBKeypad {
		fmt.Fprintf(w, "  (counts are only updated in access_dual_usb_keypad)\n\n")
		return
	}

	type occRow struct {
		pin string
		n   int
	}
	ctx.occupancyMu.Lock()
	var rows []occRow
	total := 0
	for p, n := range ctx.occupancyByPIN {
		if n <= 0 {
			continue
		}
		rows = append(rows, occRow{p, n})
		total += n
	}
	ctx.occupancyMu.Unlock()

	sort.Slice(rows, func(i, j int) bool { return rows[i].pin < rows[j].pin })

	fmt.Fprintf(w, "  people_in_area_total: %d\n", total)
	if len(rows) == 0 {
		fmt.Fprintf(w, "  (no credentials currently counted inside)\n\n")
		return
	}
	fmt.Fprintf(w, "  %-14s  %-36s  %s\n", "PIN (masked)", "label (access_pins)", "inside")
	for _, r := range rows {
		lbl := ""
		if ctx.DB != nil {
			var l sql.NullString
			_ = ctx.DB.QueryRow(`SELECT label FROM access_pins WHERE pin = ?`, r.pin).Scan(&l)
			lbl = strings.TrimSpace(l.String)
		}
		if lbl == "" {
			lbl = "(none / legacy)"
		}
		fmt.Fprintf(w, "  %-14s  %-36q  %d\n", maskPINForTechDisplay(r.pin), lbl, r.n)
	}
	fmt.Fprintln(w, "")
}

func techMenuShowConfig(w io.Writer, ctx *AppContext) {
	ctx.configMu.RLock()
	c := ctx.Config
	g := ctx.GPIOSettings
	prompt := ctx.TechMenuPrompt
	cfgPath := effectiveConfigPath(ctx)
	ctx.configMu.RUnlock()
	fmt.Fprintf(w, "\n--- Configuration ---\n")
	fmt.Fprintf(w, "  config_path (-config): %q\n", cfgPath)
	fmt.Fprintf(w, "  tech_menu_prompt: %q\n", prompt)
	fmt.Fprintf(w, "  log_level: %q\n", c.LogLevel)
	fmt.Fprintf(w, "  heartbeat_interval: %s\n", c.HeartbeatInterval)
	fmt.Fprintf(w, "  tech_menu_history_max: %d\n", c.TechMenuHistoryMax)
	fmt.Fprintf(w, "  pin_length: %d\n", c.PinLength)
	fmt.Fprintf(w, "  door_open_warning_after: %s\n", c.DoorOpenWarningAfter)
	fmt.Fprintf(w, "  door_sensor_closed_is_low: %v\n", c.DoorSensorClosedIsLow)
	fmt.Fprintf(w, "  relay_pulse_duration: %s\n", c.RelayPulseDuration)
	fmt.Fprintf(w, "  buzzer_relay_pulse_duration: %s\n", c.BuzzerRelayPulseDuration)
	fmt.Fprintf(w, "  pin_reject_buzzer_after_attempts: %d\n", c.PinRejectBuzzerAfterAttempts)
	fmt.Fprintf(w, "  keypad_inter_digit_timeout: %s\n", c.KeypadInterDigitTimeout)
	fmt.Fprintf(w, "  keypad_session_timeout: %s\n", c.KeypadSessionTimeout)
	fmt.Fprintf(w, "  pin_entry_feedback_delay: %s\n", c.PinEntryFeedbackDelay)
	fmt.Fprintf(w, "\n--- Operation mode ---\n")
	fmt.Fprintf(w, "  keypad_operation_mode: %s\n", NormalizeKeypadOperationMode(c.KeypadOperationMode))
	fmt.Fprintf(w, "  keypad_evdev_path: %q\n", c.KeypadEvdevPath)
	fmt.Fprintf(w, "  keypad_exit_evdev_path: %q\n", c.KeypadExitEvdevPath)
	fmt.Fprintf(w, "  pair_peer_role: %s\n", normalizePairPeerRole(c.PairPeerRole))
	fmt.Fprintf(w, "  mqtt_pair_peer_topic: %q\n", c.MQTTPairPeerTopic)
	pptok := `""`
	if strings.TrimSpace(c.PairPeerToken) != "" {
		pptok = "(set)"
	}
	fmt.Fprintf(w, "  pair_peer_token: %s\n", pptok)
	fmt.Fprintf(w, "  elevator_floor_wait_timeout: %s\n", c.ElevatorFloorWaitTimeout)
	if isElevatorWaitFloorMode(NormalizeKeypadOperationMode(c.KeypadOperationMode)) {
		fmt.Fprintf(w, "  elevator_wait_floor_cab_sense: %s\n", normalizeElevatorWaitFloorCabSense(c.ElevatorWaitFloorCabSense))
	}
	fmt.Fprintf(w, "  elevator_floor_input_pins: %q\n", c.ElevatorFloorInputPins)
	fmt.Fprintf(w, "  elevator_predefined_floor: %d\n", c.ElevatorPredefinedFloor)
	if s := formatIntList(c.ElevatorPredefinedFloors); s != "" {
		fmt.Fprintf(w, "  elevator_predefined_floors: %s\n", s)
	} else {
		fmt.Fprintf(w, "  elevator_predefined_floors: (unset; legacy single-floor label uses elevator_predefined_floor only)\n")
	}
	fmt.Fprintf(w, "  elevator_dispatch_pulse_duration: %s\n", c.ElevatorDispatchPulseDuration)
	if s := formatDurationList(c.ElevatorFloorDispatchPulseDurations); s != "" {
		fmt.Fprintf(w, "  elevator_floor_dispatch_pulse_durations: %s\n", s)
	} else {
		fmt.Fprintf(w, "  elevator_floor_dispatch_pulse_durations: (unset)\n")
	}
	if c.ElevatorEnablePulseDuration > 0 {
		fmt.Fprintf(w, "  elevator_enable_pulse_duration: %s (elevator_predefined_floor)\n", c.ElevatorEnablePulseDuration)
	} else {
		fmt.Fprintf(w, "  elevator_enable_pulse_duration: (unset; predefined mode uses dispatch pulse default)\n")
	}
	fmt.Fprintf(w, "  dual_keypad_reject_exit_without_entry: %v\n", c.DualKeypadRejectExitWithoutEntry)
	fmt.Fprintf(w, "\n--- Access schedule (SQLite) ---\n")
	fmt.Fprintf(w, "  access_control_door_id: %q\n", c.AccessControlDoorID)
	fmt.Fprintf(w, "  access_control_elevator_id: %q\n", c.AccessControlElevatorID)
	fmt.Fprintf(w, "  access_schedule_enforce: %v\n", c.AccessScheduleEnforce)
	fmt.Fprintf(w, "  access_schedule_apply_to_fallback_pin: %v\n", c.AccessScheduleApplyToFallbackPin)
	fmt.Fprintf(w, "  pin_lockout_enabled: %v\n", c.PinLockoutEnabled)
	fmt.Fprintf(w, "  pin_lockout_after_attempts: %d\n", c.PinLockoutAfterAttempts)
	fmt.Fprintf(w, "  pin_lockout_duration: %s\n", c.PinLockoutDuration)
	ov := `""`
	if strings.TrimSpace(c.PinLockoutOverridePin) != "" {
		ov = "(set)"
	}
	fmt.Fprintf(w, "  pin_lockout_override_pin: %s\n", ov)
	if !c.PinLockoutEnabled {
		fmt.Fprintf(w, "  keypad_lockout_remaining: disabled\n")
	} else if rem := ctx.keypadLockoutRemaining(); rem > 0 {
		fmt.Fprintf(w, "  keypad_lockout_remaining: %s\n", rem.Truncate(time.Second))
	} else {
		fmt.Fprintf(w, "  keypad_lockout_remaining: none\n")
	}
	fmt.Fprintf(w, "  sound_card_name: %q\n", c.SoundCardName)
	fmt.Fprintf(w, "  sound_startup: %q\n", c.SoundStartup)
	fmt.Fprintf(w, "  sound_shutdown: %q\n", c.SoundShutdown)
	fmt.Fprintf(w, "  sound_pin_ok: %q\n", c.SoundPinOK)
	fmt.Fprintf(w, "  sound_pin_reject: %q\n", c.SoundPinReject)
	fmt.Fprintf(w, "  sound_keypress: %q\n", c.SoundKeypress)
	fmt.Fprintf(w, "\n--- MQTT ---\n")
	fmt.Fprintf(w, "  mqtt_enabled: %v\n", c.MQTTEnabled)
	fmt.Fprintf(w, "  mqtt_broker: %q\n", c.MQTTBroker)
	fmt.Fprintf(w, "  mqtt_client_id: %q\n", c.MQTTClientID)
	fmt.Fprintf(w, "  mqtt_username: %q\n", c.MQTTUsername)
	fmt.Fprintf(w, "  mqtt_password: %q\n", c.MQTTPassword)
	fmt.Fprintf(w, "  mqtt_command_topic: %q\n", c.MQTTCommandTopic)
	fmt.Fprintf(w, "  mqtt_status_topic: %q\n", c.MQTTStatusTopic)
	mqttTok := `""`
	if strings.TrimSpace(c.MQTTCommandToken) != "" {
		mqttTok = "(set)"
	}
	fmt.Fprintf(w, "  mqtt_command_token: %s\n", mqttTok)
	fmt.Fprintf(w, "\n--- HTTP webhooks ---\n")
	fmt.Fprintf(w, "  webhook_event_enabled: %v\n", c.WebhookEventEnabled)
	fmt.Fprintf(w, "  webhook_event_url: %q\n", c.WebhookEventURL)
	fmt.Fprintf(w, "  webhook_event_token_enabled: %v\n", c.WebhookEventTokenEnabled)
	evTok := `""`
	if strings.TrimSpace(c.WebhookEventToken) != "" {
		evTok = "(set)"
	}
	fmt.Fprintf(w, "  webhook_event_token: %s\n", evTok)
	fmt.Fprintf(w, "  webhook_heartbeat_enabled: %v\n", c.WebhookHeartbeatEnabled)
	fmt.Fprintf(w, "  webhook_heartbeat_url: %q\n", c.WebhookHeartbeatURL)
	fmt.Fprintf(w, "  webhook_heartbeat_token_enabled: %v\n", c.WebhookHeartbeatTokenEnabled)
	hbTok := `""`
	if strings.TrimSpace(c.WebhookHeartbeatToken) != "" {
		hbTok = "(set)"
	}
	fmt.Fprintf(w, "  webhook_heartbeat_token: %s\n", hbTok)
	fmt.Fprintf(w, "\n--- GPIO ---\n")
	fmt.Fprintf(w, "  relay_output_mode: %s\n", normalizeRelayOutputMode(g.RelayOutputMode))
	fmt.Fprintf(w, "  mcp23017_i2c_bus: %d\n", g.MCP23017I2CBus)
	fmt.Fprintf(w, "  mcp23017_i2c_addr: %d\n", int(g.MCP23017I2CAddr))
	fmt.Fprintf(w, "  xl9535_i2c_bus: %d\n", g.XL9535I2CBus)
	fmt.Fprintf(w, "  xl9535_i2c_addr: %d\n", int(g.XL9535I2CAddr))
	fmt.Fprintf(w, "  door_relay_pin: %d\n", g.DoorRelayPin)
	fmt.Fprintf(w, "  door_relay_active_low: %v\n", g.DoorRelayActiveLow)
	fmt.Fprintf(w, "  buzzer_relay_pin: %d\n", g.BuzzerRelayPin)
	fmt.Fprintf(w, "  buzzer_relay_active_low: %v\n", g.BuzzerRelayActiveLow)
	fmt.Fprintf(w, "  door_sensor_pin: %d\n", g.DoorSensorPin)
	fmt.Fprintf(w, "  heartbeat_led_pin: %d\n", g.HeartbeatLEDPin)
	fmt.Fprintf(w, "  exit_button_pin: %d\n", g.ExitButtonPin)
	fmt.Fprintf(w, "  exit_button_active_low: %v\n", g.ExitButtonActiveLow)
	fmt.Fprintf(w, "  entry_button_pin: %d\n", g.EntryButtonPin)
	fmt.Fprintf(w, "  entry_button_active_low: %v\n", g.EntryButtonActiveLow)
	fmt.Fprintf(w, "  elevator_dispatch_relay_pin: %d\n", g.ElevatorDispatchRelayPin)
	fmt.Fprintf(w, "  elevator_dispatch_active_low: %v\n", g.ElevatorDispatchActiveLow)
	fmt.Fprintf(w, "  elevator_enable_relay_pin: %d\n", g.ElevatorEnableRelayPin)
	fmt.Fprintf(w, "  elevator_enable_active_low: %v\n", g.ElevatorEnableActiveLow)
	if strings.TrimSpace(g.ElevatorFloorDispatchPins) != "" {
		fmt.Fprintf(w, "  elevator_floor_dispatch_pins: %q\n", g.ElevatorFloorDispatchPins)
	} else {
		fmt.Fprintf(w, "  elevator_floor_dispatch_pins: (unset; use elevator_dispatch_relay_pin / door)\n")
	}
	if strings.TrimSpace(g.ElevatorPredefinedEnablePins) != "" {
		fmt.Fprintf(w, "  elevator_predefined_enable_pins: %q\n", g.ElevatorPredefinedEnablePins)
	} else {
		fmt.Fprintf(w, "  elevator_predefined_enable_pins: (unset; elevator_predefined_floor only)\n")
	}
	if strings.TrimSpace(g.ElevatorWaitFloorEnablePins) != "" {
		fmt.Fprintf(w, "  elevator_wait_floor_enable_pins: %q\n", g.ElevatorWaitFloorEnablePins)
	} else {
		fmt.Fprintf(w, "  elevator_wait_floor_enable_pins: (unset; use elevator_enable_relay_pin for wait-floor)\n")
	}
	if ctx.GPIO == nil {
		fmt.Fprintln(w, "  gpio_manager_available: false")
	} else {
		fmt.Fprintln(w, "  gpio_manager_available: true")
	}
	fmt.Fprintln(w, "")
}

func techMenuDoorSensorOnce(ctx *AppContext) {
	if ctx.GPIO == nil || !ctx.GPIO.DoorSensorConfigured() {
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "Door sensor unavailable (GPIO not ready).")
		})
		log.Println("WARNING: Technician menu: door sensor read skipped (no GPIO).")
		return
	}
	ctx.configMu.RLock()
	closedLow := ctx.Config.DoorSensorClosedIsLow
	pin := ctx.GPIOSettings.DoorSensorPin
	ctx.configMu.RUnlock()
	open := ctx.GPIO.DoorIsOpen(closedLow)
	state := "CLOSED"
	if open {
		state = "OPEN"
	}
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Door sensor (GPIO %d): %s\n", pin, state)
	})
	log.Printf("INFO: Technician menu: door sensor snapshot: %s", state)
}

func techMenuDoorSensorWatch(ctx *AppContext) {
	if ctx.GPIO == nil || !ctx.GPIO.DoorSensorConfigured() {
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "Door sensor unavailable (GPIO not ready).")
		})
		return
	}
	techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Watching door sensor (~2s)...") })
	for i := 0; i < 10; i++ {
		ctx.configMu.RLock()
		closedLow := ctx.Config.DoorSensorClosedIsLow
		ctx.configMu.RUnlock()
		open := ctx.GPIO.DoorIsOpen(closedLow)
		st := "CLOSED"
		if open {
			st = "OPEN"
		}
		ii, sst := i, st
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "  [%d] %s\n", ii, sst)
		})
		time.Sleep(200 * time.Millisecond)
	}
	log.Println("INFO: Technician menu: door sensor watch completed.")
}

func techMenuPulse(ctx *AppContext, name string) {
	ctx.configMu.RLock()
	var d time.Duration
	switch name {
	case "door":
		d = ctx.Config.RelayPulseDuration
	case "buzzer":
		d = ctx.Config.BuzzerRelayPulseDuration
	default:
		ctx.configMu.RUnlock()
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Unknown output %q\n", name) })
		return
	}
	ctx.configMu.RUnlock()
	if ctx.GPIO == nil {
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Cannot pulse %q: GPIO unavailable.\n", name)
		})
		log.Printf("WARNING: Technician menu: pulse %s skipped (no GPIO).", name)
		return
	}
	ctx.GPIO.ActionPulse(name, d)
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Pulsing %q for %s\n", name, d)
	})
	log.Printf("INFO: Technician menu: test pulse %q for %s", name, d)
}

// --- Technician menu: network diagnostics (Linux /proc) ---

func readResolvConfNameservers() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var ns []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 && strings.EqualFold(f[0], "nameserver") {
			ns = append(ns, f[1])
		}
	}
	return ns
}

func procLittleEndianHexIPv4(h8 string) string {
	if len(h8) != 8 {
		return ""
	}
	raw, err := hex.DecodeString(h8)
	if err != nil || len(raw) != 4 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", raw[3], raw[2], raw[1], raw[0])
}

// parseDefaultIPv4GatewaysByIface reads /proc/net/route (Linux): iface name -> gateway for default IPv4 routes.
func parseDefaultIPv4GatewaysByIface() map[string]string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	out := make(map[string]string)
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if f[1] != "00000000" {
			continue
		}
		gw := procLittleEndianHexIPv4(f[2])
		if gw == "" {
			continue
		}
		out[f[0]] = gw
	}
	return out
}

func ifaceLANWiFiBucket(name string) int {
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(n, "wlan") || strings.HasPrefix(n, "wl") {
		return 1
	}
	if strings.HasPrefix(n, "eth") || strings.HasPrefix(n, "en") || strings.HasPrefix(n, "end") || strings.HasPrefix(n, "usb") {
		return 0
	}
	return 2
}

func collectIPv4Nets(ifi *net.Interface) []*net.IPNet {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	var out []*net.IPNet
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() {
			continue
		}
		out = append(out, ipn)
	}
	return out
}

func techMenuWriteOneInterface(w io.Writer, ifi net.Interface, gwByIface map[string]string) {
	up := ifi.Flags&net.FlagUp != 0
	admin := "down"
	if up {
		admin = "up"
	}
	fmt.Fprintf(w, "  %s: admin=%s  MTU=%d\n", ifi.Name, admin, ifi.MTU)
	nets := collectIPv4Nets(&ifi)
	if len(nets) == 0 {
		fmt.Fprintln(w, "    IPv4: (none assigned)")
	} else {
		for _, ipn := range nets {
			mask := net.IP(ipn.Mask).String()
			fmt.Fprintf(w, "    IPv4: %s  subnet mask: %s\n", ipn.IP.String(), mask)
		}
	}
	gw := ""
	if gwByIface != nil {
		gw = gwByIface[ifi.Name]
	}
	if gw == "" {
		fmt.Fprintln(w, "    Default gateway (IPv4, this iface): (none in /proc/net/route)")
	} else {
		fmt.Fprintf(w, "    Default gateway (IPv4, this iface): %s\n", gw)
	}
}

func techMenuWriteNetworkDiag(w io.Writer) {
	fmt.Fprintln(w, "\n--- Network (snapshot) ---")
	if runtime.GOOS != "linux" {
		fmt.Fprintf(w, "  Full interface list is from Go (below); gateway/DNS use Linux-specific paths.\n\n")
	}
	dns := readResolvConfNameservers()
	if len(dns) == 0 {
		fmt.Fprintln(w, "  DNS (/etc/resolv.conf): (none found)")
	} else {
		fmt.Fprintf(w, "  DNS (/etc/resolv.conf): %s\n", strings.Join(dns, ", "))
	}

	var gwByIface map[string]string
	if runtime.GOOS == "linux" {
		gwByIface = parseDefaultIPv4GatewaysByIface()
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		fmt.Fprintf(w, "  error listing interfaces: %v\n\n", err)
		return
	}

	var ethList, wifiList, otherList []net.Interface
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		switch ifaceLANWiFiBucket(ifi.Name) {
		case 0:
			ethList = append(ethList, ifi)
		case 1:
			wifiList = append(wifiList, ifi)
		default:
			otherList = append(otherList, ifi)
		}
	}

	printGroup := func(title string, list []net.Interface) {
		fmt.Fprintf(w, "\n%s\n", title)
		if len(list) == 0 {
			fmt.Fprintln(w, "  (no interface in this category)")
			return
		}
		for _, ifi := range list {
			techMenuWriteOneInterface(w, ifi, gwByIface)
		}
	}

	printGroup("--- Ethernet / LAN ---", ethList)
	printGroup("--- Wi-Fi ---", wifiList)
	if len(otherList) > 0 {
		printGroup("--- Other interfaces ---", otherList)
	}
	fmt.Fprintln(w, "")
}

func procLocalAddrToHostPort(addr string, ipv6 bool) (string, bool) {
	colon := strings.LastIndex(addr, ":")
	if colon < 0 {
		return "", false
	}
	ipHex := addr[:colon]
	portHex := addr[colon+1:]
	portU, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return "", false
	}
	port := int(portU)
	if !ipv6 {
		if len(ipHex) != 8 {
			return "", false
		}
		ip := procLittleEndianHexIPv4(ipHex)
		if ip == "" {
			return "", false
		}
		return net.JoinHostPort(ip, strconv.Itoa(port)), true
	}
	if len(ipHex) != 32 {
		return "", false
	}
	raw, err := hex.DecodeString(ipHex)
	if err != nil || len(raw) != 16 {
		return "", false
	}
	ip := net.IP(raw)
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), true
}

// procNetSocketInode returns the socket inode from a /proc/net/{tcp,udp}{,6} data line (field index 9 on recent kernels).
func procNetSocketInode(f []string) (string, bool) {
	if len(f) <= 9 {
		return "", false
	}
	inode := f[9]
	if _, err := strconv.ParseUint(inode, 10, 64); err != nil {
		return "", false
	}
	return inode, true
}

func isProcNetSocketDataLine(f []string) bool {
	return len(f) >= 10 && strings.Contains(f[1], ":")
}

// buildGlobalSocketInodeToProcs maps socket inode -> "pid/comm" list (all processes holding that inode).
func buildGlobalSocketInodeToProcs() map[string]string {
	sets := make(map[string]map[string]struct{})
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	const pref = "socket:["
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join("/proc", pid, "comm"))
		name := strings.TrimSpace(string(comm))
		if name == "" {
			name = "?"
		}
		fdDir := filepath.Join("/proc", pid, "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if !strings.HasPrefix(link, pref) || !strings.HasSuffix(link, "]") {
				continue
			}
			ino := strings.TrimSuffix(strings.TrimPrefix(link, pref), "]")
			if sets[ino] == nil {
				sets[ino] = make(map[string]struct{})
			}
			sets[ino][fmt.Sprintf("%s/%s", pid, name)] = struct{}{}
		}
	}
	out := make(map[string]string, len(sets))
	for ino, s := range sets {
		var xs []string
		for x := range s {
			xs = append(xs, x)
		}
		sort.Strings(xs)
		out[ino] = strings.Join(xs, ", ")
	}
	return out
}

type procListenRow struct {
	proto string
	addr  string
	inode string
}

func scanProcNetAllTCPListeners(path, proto string, out *[]procListenRow) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ipv6 := strings.Contains(path, "tcp6")
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "local_address") {
			continue
		}
		f := strings.Fields(line)
		if !isProcNetSocketDataLine(f) {
			continue
		}
		if f[3] != "0A" { // TCP_LISTEN
			continue
		}
		inode, ok := procNetSocketInode(f)
		if !ok {
			continue
		}
		hostport, ok := procLocalAddrToHostPort(f[1], ipv6)
		if !ok {
			continue
		}
		*out = append(*out, procListenRow{proto: proto, addr: hostport, inode: inode})
	}
	return nil
}

// UDP "listening" / bound unconnected sockets (st 07) in /proc/net/udp{,6}.
func scanProcNetAllUDPBinds(path, proto string, out *[]procListenRow) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ipv6 := strings.Contains(path, "udp6")
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "local_address") {
			continue
		}
		f := strings.Fields(line)
		if !isProcNetSocketDataLine(f) {
			continue
		}
		if f[3] != "07" { // UDP unconnected (typically bound / accepting datagrams)
			continue
		}
		inode, ok := procNetSocketInode(f)
		if !ok {
			continue
		}
		hostport, ok := procLocalAddrToHostPort(f[1], ipv6)
		if !ok {
			continue
		}
		*out = append(*out, procListenRow{proto: proto, addr: hostport, inode: inode})
	}
	return nil
}

func collectSystemListenRows() ([]procListenRow, error) {
	var rows []procListenRow
	if err := scanProcNetAllTCPListeners("/proc/net/tcp", "tcp", &rows); err != nil {
		return nil, err
	}
	_ = scanProcNetAllTCPListeners("/proc/net/tcp6", "tcp6", &rows)
	_ = scanProcNetAllUDPBinds("/proc/net/udp", "udp", &rows)
	_ = scanProcNetAllUDPBinds("/proc/net/udp6", "udp6", &rows)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].proto != rows[j].proto {
			return rows[i].proto < rows[j].proto
		}
		return rows[i].addr < rows[j].addr
	})
	return rows, nil
}

func techMenuWriteProcListenPorts(w io.Writer) {
	fmt.Fprintln(w, "\n--- System listening / bound ports (all processes) ---")
	fmt.Fprintln(w, "  TCP: sockets in LISTEN. UDP: unconnected bound sockets (typical servers).")
	if runtime.GOOS != "linux" {
		fmt.Fprintln(w, "  (only implemented on Linux via /proc)")
		fmt.Fprintln(w, "")
		return
	}
	ino2p := buildGlobalSocketInodeToProcs()
	rows, err := collectSystemListenRows()
	if err != nil {
		fmt.Fprintf(w, "  error: %v\n\n", err)
		return
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "  (none parsed)")
	} else {
		for _, r := range rows {
			who := ino2p[r.inode]
			if who == "" {
				who = fmt.Sprintf("inode %s (owner not resolved)", r.inode)
			}
			fmt.Fprintf(w, "  %-4s  %-40s  %s\n", r.proto, r.addr, who)
		}
	}
	fmt.Fprintln(w, "\n  Source: /proc/net/tcp, tcp6, udp, udp6; owners from /proc/*/fd (needs permission to scan all PIDs).")
	fmt.Fprintln(w, "")
}

// --- Structures ---

// i2cRelayExpander drives relay outputs 0–15 on an MCP23017 or XL9535 (see gpio.relay_output_mode).
type i2cRelayExpander interface {
	SetPin(pin uint8, high bool) error
}

// OutputConfig defines an output pin, like a door relay or buzzer
type OutputConfig struct {
	PinNumber uint8
	ActiveLow bool // True if the relay triggers on ground/0V (common for opto-relays)
	Pin       rpio.Pin
	// UseI2CRelay: when true, PinNumber is expander index 0-15 (MCP23017 or XL9535 per relay_output_mode).
	UseI2CRelay bool
	mu          sync.Mutex // Prevents overlapping pulse routines
}

// InputConfig defines an input pin, like a door sensor or egress button
type InputConfig struct {
	PinNumber    uint8
	PullUp       bool          // Enable internal pull-up resistor
	DebounceTime time.Duration // Time to ignore subsequent triggers
	Pin          rpio.Pin
	Action       func()    // The function to call when triggered
	lastTrigger  time.Time // Used for debouncing
}

// elevatorFloorPin holds a BCM input wired to a cab floor button (active low when pressed).
type elevatorFloorPin struct {
	Pin rpio.Pin
	BCM uint8
}

// GPIOManager holds the state of all physical IO
type GPIOManager struct {
	Outputs map[string]*OutputConfig
	Inputs  map[string]*InputConfig

	i2cRelay i2cRelayExpander

	doorSensorPin   rpio.Pin
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

// SetI2CRelayExpander attaches an MCP23017 or XL9535 for outputs registered with UseI2CRelay true.
func (m *GPIOManager) SetI2CRelayExpander(d i2cRelayExpander) {
	m.i2cRelay = d
}

// ConfigureDoorSensor sets up a BCM GPIO as digital input with pull-up for a door contact (call once after rpio.Open).
// Typical for DoorSensorClosedIsLow wiring; if your "closed" state is active-high only, adjust hardware pull resistors as needed.
func (m *GPIOManager) ConfigureDoorSensor(bcm uint8) {
	p := rpio.Pin(bcm)
	p.Input()
	p.PullUp()
	m.doorSensorPin = p
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
	isLow := m.doorSensorPin.Read() == rpio.Low
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
		p := rpio.Pin(bcm)
		p.Input()
		p.PullUp()
		m.elevatorFloorPins = append(m.elevatorFloorPins, elevatorFloorPin{Pin: p, BCM: bcm})
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
		if fp.Pin.Read() == rpio.Low {
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
		p := rpio.Pin(pin)
		p.Output()
		cfg.Pin = p
	}
	m.Outputs[name] = cfg

	// Ensure it starts in the "Off" state
	m.ActionOff(name)
}

// AddInput registers a new input pin and its callback function
func (m *GPIOManager) AddInput(name string, pin uint8, pullUp bool, action func()) {
	p := rpio.Pin(pin)
	p.Input()

	if pullUp {
		p.PullUp()
		p.Detect(rpio.FallEdge) // Detect when pulled to ground
	} else {
		p.PullDown()
		p.Detect(rpio.RiseEdge) // Detect when voltage is applied
	}

	m.Inputs[name] = &InputConfig{
		PinNumber:    pin,
		PullUp:       pullUp,
		DebounceTime: 300 * time.Millisecond,
		Pin:          p,
		Action:       action,
		lastTrigger:  time.Now(),
	}
}

// --- Output Actions ---

// ActionOn turns the output on continuously
func (m *GPIOManager) ActionOn(name string) {
	out, exists := m.Outputs[name]
	if !exists {
		log.Printf("ERROR: Output '%s' not found", name)
		return
	}
	if out.UseI2CRelay {
		if m.i2cRelay == nil {
			log.Printf("ERROR: Output '%s' uses I2C relay expander but device is not initialized", name)
			return
		}
		// Energized: active-low => drive low; active-high => drive high.
		logicHigh := !out.ActiveLow
		if err := m.i2cRelay.SetPin(out.PinNumber, logicHigh); err != nil {
			log.Printf("ERROR: I2C relay output %q pin %d: %v", name, out.PinNumber, err)
		}
		return
	}
	if out.ActiveLow {
		out.Pin.Low()
	} else {
		out.Pin.High()
	}
}

// ActionOff turns the output off continuously
func (m *GPIOManager) ActionOff(name string) {
	out, exists := m.Outputs[name]
	if !exists {
		return
	}
	if out.UseI2CRelay {
		if m.i2cRelay == nil {
			return
		}
		logicHigh := out.ActiveLow
		_ = m.i2cRelay.SetPin(out.PinNumber, logicHigh)
		return
	}
	if out.ActiveLow {
		out.Pin.High()
	} else {
		out.Pin.Low()
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

		m.ActionOn(name)
		time.Sleep(duration)
		m.ActionOff(name)
	}()
}

// --- Input Listener ---

// StartListening begins polling for edge detections on configured inputs
func (m *GPIOManager) StartListening() {
	log.Println("INFO: Starting GPIO Input Listener...")

	for {
		for name, in := range m.Inputs {
			if in.Pin.EdgeDetected() {
				// Software Debouncing logic
				if time.Since(in.lastTrigger) > in.DebounceTime {
					in.lastTrigger = time.Now()
					log.Printf("DEBUG: GPIO Input '%s' triggered", name)

					// Execute the defined action in a new goroutine
					// so a slow callback doesn't block other inputs
					go in.Action()
				}
			}
		}
		// A small sleep prevents the loop from consuming 100% CPU
		time.Sleep(10 * time.Millisecond)
	}
}

// parseBCMPinList parses comma-separated BCM numbers (e.g. "17,27,22") for elevator floor sense inputs.
func parseBCMPinList(s string) ([]uint8, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	var out []uint8
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid BCM %q: %w", p, err)
		}
		u, err := bcmUint8("elevator_floor_input", n)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func setupOperationModeGPIOInputs(ctx *AppContext, gpio *GPIOManager) {
	ctx.configMu.RLock()
	mode := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
	ex := ctx.GPIOSettings.ExitButtonPin
	exLow := ctx.GPIOSettings.ExitButtonActiveLow
	en := ctx.GPIOSettings.EntryButtonPin
	enLow := ctx.GPIOSettings.EntryButtonActiveLow
	ctx.configMu.RUnlock()

	if ex != 0 && modeUsesExitGPIOButton(mode) {
		gpio.AddInput("exit_button", ex, exLow, func() {
			ctx.configMu.RLock()
			m := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
			d := ctx.Config.RelayPulseDuration
			ctx.configMu.RUnlock()
			if !modeUsesExitGPIOButton(m) {
				return
			}
			log.Println("INFO: Exit button (GPIO REX); pulsing door relay.")
			fireEventWebhook(ctx, "exit_button_rex", map[string]any{"operation_mode": m})
			if ctx.GPIO != nil {
				ctx.GPIO.ActionPulse("door", d)
			}
		})
	}
	if en != 0 && modeUsesEntryGPIOButton(mode) {
		gpio.AddInput("entry_button", en, enLow, func() {
			ctx.configMu.RLock()
			m := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
			d := ctx.Config.RelayPulseDuration
			ctx.configMu.RUnlock()
			if !modeUsesEntryGPIOButton(m) {
				return
			}
			log.Println("INFO: Entry request button (GPIO); pulsing door relay.")
			fireEventWebhook(ctx, "entry_button_rex", map[string]any{"operation_mode": m})
			if ctx.GPIO != nil {
				ctx.GPIO.ActionPulse("door", d)
			}
		})
	}
}

func startKeypadListeners(ctx *AppContext) {
	ctx.configMu.RLock()
	mode := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
	p1 := strings.TrimSpace(ctx.Config.KeypadEvdevPath)
	p2 := strings.TrimSpace(ctx.Config.KeypadExitEvdevPath)
	ctx.configMu.RUnlock()
	if p1 == "" {
		p1 = "/dev/input/event1"
	}
	if isDualUSBKeypadMode(mode) {
		if p2 == "" || p2 == p1 {
			log.Printf("CRITICAL: access_dual_usb_keypad requires keypad_exit_evdev_path distinct from keypad_evdev_path; using single listener on %q", p1)
			runKeypadListener(ctx, p1, "")
			return
		}
		go runKeypadListener(ctx, p1, "entry")
		runKeypadListener(ctx, p2, "exit")
		return
	}
	runKeypadListener(ctx, p1, "")
}

func clearElevatorGrantState(ctx *AppContext) {
	ctx.elevatorMu.Lock()
	ctx.elevatorGrantUntil = time.Time{}
	ctx.elevatorGrantStartedAt = time.Time{}
	ctx.elevatorCabFloorDebounceHeld = nil
	ctx.elevatorCabFloorDebounceTick = 0
	ctx.elevatorGrantPIN = ""
	ctx.elevatorGrantViaFallback = false
	ctx.elevatorMu.Unlock()
	if ctx.GPIO == nil {
		return
	}
	for i := range ctx.elevatorWaitFloorEnablePins {
		name := elevatorWaitFloorEnableOutputName(i)
		if ctx.GPIO.HasOutput(name) {
			ctx.GPIO.ActionOff(name)
		}
	}
	if ctx.GPIO.HasOutput("elevator_enable") {
		ctx.GPIO.ActionOff("elevator_enable")
	}
}

func startElevatorFloorWaitGrant(ctx *AppContext, cfg DeviceConfig) {
	ctx.elevatorMu.Lock()
	pin := ctx.elevatorGrantPIN
	via := ctx.elevatorGrantViaFallback
	ctx.elevatorMu.Unlock()

	elevID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
	now := time.Now()

	ctx.elevatorMu.Lock()
	ctx.elevatorGrantUntil = now.Add(cfg.ElevatorFloorWaitTimeout)
	ctx.elevatorGrantStartedAt = now
	ctx.elevatorCabFloorDebounceHeld = nil
	ctx.elevatorCabFloorDebounceTick = 0
	ctx.elevatorMu.Unlock()
	if ctx.GPIO == nil {
		return
	}
	// Hold ground-return / enable relays for the full wait window (until clearElevatorGrantState on
	// floor press or timeout). elevator_enable_pulse_duration does not apply here—only elevator_predefined_floor uses it.
	if len(ctx.elevatorWaitFloorEnablePins) > 0 {
		for i := range ctx.elevatorWaitFloorEnablePins {
			if !ctx.elevatorFloorChannelAllowed(pin, elevID, i, via, now) {
				continue
			}
			name := elevatorWaitFloorEnableOutputName(i)
			if ctx.GPIO.HasOutput(name) {
				ctx.GPIO.ActionOn(name)
			}
		}
		return
	}
	// Legacy single shared enable relay: hardware cannot isolate per floor; PIN/time rules are enforced when a floor is selected.
	if ctx.GPIO.HasOutput("elevator_enable") {
		ctx.GPIO.ActionOn("elevator_enable")
	}
}

func pulseElevatorOrDoorOutput(ctx *AppContext, cfg DeviceConfig) bool {
	if ctx.GPIO == nil {
		return false
	}
	dur := cfg.ElevatorDispatchPulseDuration
	if ctx.GPIO.HasOutput("elevator_dispatch") {
		ctx.GPIO.ActionPulse("elevator_dispatch", dur)
		return true
	}
	ctx.GPIO.ActionPulse("door", dur)
	return true
}

// pulseElevatorFloorSelections pulses per-floor dispatch outputs for each cab index, or one legacy dispatch/door pulse.
func pulseElevatorFloorSelections(ctx *AppContext, cfg DeviceConfig, floorIndices []int) bool {
	if ctx.GPIO == nil || len(floorIndices) == 0 {
		return false
	}
	perFloor := false
	for _, idx := range floorIndices {
		if ctx.GPIO.HasOutput(elevatorFloorDispatchOutputName(idx)) {
			perFloor = true
			break
		}
	}
	if !perFloor {
		return pulseElevatorOrDoorOutput(ctx, cfg)
	}
	for _, idx := range floorIndices {
		name := elevatorFloorDispatchOutputName(idx)
		if ctx.GPIO.HasOutput(name) {
			ctx.GPIO.ActionPulse(name, dispatchPulseDurationForFloor(cfg, idx))
		}
	}
	return true
}

func pulseElevatorPredefinedDispatchAtIndex(ctx *AppContext, cfg DeviceConfig, idx int) (outName string, pin int, ok bool) {
	if ctx.GPIO == nil {
		return "", 0, false
	}
	n := len(ctx.elevatorFloorDispatchPins)
	if n == 0 {
		ok = pulseElevatorOrDoorOutput(ctx, cfg)
		if ctx.GPIO.HasOutput("elevator_dispatch") {
			return "elevator_dispatch", int(ctx.GPIOSettings.ElevatorDispatchRelayPin), ok
		}
		return "door", int(ctx.GPIOSettings.DoorRelayPin), ok
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	name := elevatorFloorDispatchOutputName(idx)
	if ctx.GPIO.HasOutput(name) {
		ctx.GPIO.ActionPulse(name, dispatchPulseDurationForFloor(cfg, idx))
		return name, int(ctx.elevatorFloorDispatchPins[idx]), true
	}
	ok = pulseElevatorOrDoorOutput(ctx, cfg)
	if ctx.GPIO.HasOutput("elevator_dispatch") {
		return "elevator_dispatch", int(ctx.GPIOSettings.ElevatorDispatchRelayPin), ok
	}
	return "door", int(ctx.GPIOSettings.DoorRelayPin), ok
}

// activateElevatorPredefinedFloor pulses the enable relay (if configured) and dispatch for the selected predefined floor; returns webhook detail fields.
// The second return is false when elevatorFloorChannelAllowed denies (PIN floor list / floor groups, or timed lock).
func activateElevatorPredefinedFloor(ctx *AppContext, cfg DeviceConfig, kTag, credTag, pin string, viaFallback bool) (map[string]any, bool) {
	extra := map[string]any{}
	aclIdx := ctx.elevatorPredefinedDispatchIndexForACL(cfg)
	elevID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
	if !ctx.elevatorFloorChannelAllowed(pin, elevID, aclIdx, viaFallback, time.Now()) {
		log.Printf("INFO: Elevator predefined floor denied (%s not permitted for credential or schedule; %s keypad; credential=%s).", elevatorFloorLogLabel(ctx.DB, elevID, aclIdx), kTag, credTag)
		return nil, false
	}
	if ctx.GPIO == nil {
		extra["gpio"] = "unavailable"
		log.Printf("INFO: PIN accepted (elevator predefined; %s keypad; credential=%s); GPIO unavailable.", kTag, credTag)
		return extra, true
	}
	nf := len(cfg.ElevatorPredefinedFloors)
	if nf == 0 {
		idx := cfg.ElevatorPredefinedFloor
		if len(ctx.elevatorFloorDispatchPins) > 0 {
			if idx < 0 {
				idx = 0
			}
			if idx >= len(ctx.elevatorFloorDispatchPins) {
				log.Printf("WARNING: elevator_predefined_floor %d out of range for %d dispatch relay(s); using index %d.", cfg.ElevatorPredefinedFloor, len(ctx.elevatorFloorDispatchPins), len(ctx.elevatorFloorDispatchPins)-1)
				idx = len(ctx.elevatorFloorDispatchPins) - 1
			}
		}
		dOut, dPin, dOK := pulseElevatorPredefinedDispatchAtIndex(ctx, cfg, idx)
		log.Printf("INFO: PIN accepted (elevator predefined legacy; %s keypad; credential=%s); configured_predefined_floors=[] logical_floor_label=%d dispatch_output=%q dispatch_relay_pin=%d dispatch_pulsed=%v",
			kTag, credTag, cfg.ElevatorPredefinedFloor, dOut, dPin, dOK)
		extra["elevator_predefined_logical_floor"] = cfg.ElevatorPredefinedFloor
		extra["dispatch_output"] = dOut
		extra["dispatch_relay_pin"] = dPin
		extra["dispatch_pulsed"] = dOK
		return extra, true
	}
	idx := cfg.ElevatorPredefinedFloor
	if idx < 0 {
		idx = 0
	}
	if idx >= nf {
		log.Printf("WARNING: elevator_predefined_floor index %d out of range for %d configured floors; using index %d.", cfg.ElevatorPredefinedFloor, nf, nf-1)
		idx = nf - 1
	}
	logical := cfg.ElevatorPredefinedFloors[idx]
	enOut, enPin := "", 0
	enName := elevatorPredefinedEnableOutputName(idx)
	if ctx.GPIO.HasOutput(enName) {
		enOut = enName
		if idx < len(ctx.elevatorPredefinedEnablePins) {
			enPin = int(ctx.elevatorPredefinedEnablePins[idx])
		}
		enDur := cfg.ElevatorEnablePulseDuration
		if enDur <= 0 {
			enDur = dispatchPulseDurationForFloor(cfg, idx)
			if enDur <= 0 {
				enDur = cfg.ElevatorDispatchPulseDuration
			}
		}
		ctx.GPIO.ActionPulse(enName, enDur)
	}
	dOut, dPin, dOK := pulseElevatorPredefinedDispatchAtIndex(ctx, cfg, idx)
	log.Printf("INFO: PIN accepted (elevator predefined; %s keypad; credential=%s); configured_logical_floors=%v selected_index=%d activated_logical_floor=%d enable_output=%q enable_relay_pin=%d dispatch_output=%q dispatch_relay_pin=%d dispatch_pulsed=%v",
		kTag, credTag, cfg.ElevatorPredefinedFloors, idx, logical, enOut, enPin, dOut, dPin, dOK)
	extra["elevator_predefined_floors_configured"] = cfg.ElevatorPredefinedFloors
	extra["elevator_predefined_selected_index"] = idx
	extra["elevator_predefined_logical_floor"] = logical
	if enOut != "" {
		extra["elevator_enable_output"] = enOut
		extra["elevator_enable_relay_pin"] = enPin
	}
	extra["dispatch_output"] = dOut
	extra["dispatch_relay_pin"] = dPin
	extra["dispatch_pulsed"] = dOK
	return extra, true
}

func monitorElevatorFloorSelection(ctx *AppContext) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for range tick.C {
		ctx.configMu.RLock()
		mode := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
		senseCab := elevatorWaitFloorSenseCabInputs(ctx.Config)
		ctx.configMu.RUnlock()
		if mode != ModeElevatorWaitFloorButtons {
			continue
		}
		ctx.elevatorMu.Lock()
		until := ctx.elevatorGrantUntil
		ctx.elevatorMu.Unlock()
		if until.IsZero() {
			continue
		}
		if time.Now().After(until) {
			clearElevatorGrantState(ctx)
			if senseCab {
				log.Println("WARNING: Elevator floor-button wait window expired (cab sense enabled).")
				fireEventWebhook(ctx, "elevator_floor_timeout", map[string]any{"operation_mode": mode, "elevator_wait_floor_cab_sense": ElevatorWaitFloorCabSenseSense})
			} else {
				log.Println("INFO: Elevator wait-floor enable window ended (cab sense disabled; no floor GPIO).")
				fireEventWebhook(ctx, "elevator_floor_timeout", map[string]any{"operation_mode": mode, "elevator_wait_floor_cab_sense": ElevatorWaitFloorCabSenseIgnore})
			}
			continue
		}
		if !senseCab {
			continue
		}
		if ctx.GPIO == nil || !ctx.GPIO.HasElevatorFloorPins() {
			continue
		}
		ctx.elevatorMu.Lock()
		if ctx.elevatorGrantUntil.IsZero() || time.Now().After(ctx.elevatorGrantUntil) {
			ctx.elevatorMu.Unlock()
			continue
		}
		if ctx.elevatorGrantStartedAt.IsZero() || time.Since(ctx.elevatorGrantStartedAt) < elevatorCabSenseArmDelay {
			ctx.elevatorMu.Unlock()
			continue
		}
		ctx.elevatorMu.Unlock()

		pressed := ctx.GPIO.ElevatorCabFloorsPressed()
		var toDispatch []int
		ctx.elevatorMu.Lock()
		if ctx.elevatorGrantUntil.IsZero() || time.Now().After(ctx.elevatorGrantUntil) {
			ctx.elevatorCabFloorDebounceHeld = nil
			ctx.elevatorCabFloorDebounceTick = 0
			ctx.elevatorMu.Unlock()
			continue
		}
		if len(pressed) == 0 {
			ctx.elevatorCabFloorDebounceHeld = nil
			ctx.elevatorCabFloorDebounceTick = 0
			ctx.elevatorMu.Unlock()
			continue
		}
		if slices.Equal(ctx.elevatorCabFloorDebounceHeld, pressed) {
			ctx.elevatorCabFloorDebounceTick++
		} else {
			ctx.elevatorCabFloorDebounceHeld = slices.Clone(pressed)
			ctx.elevatorCabFloorDebounceTick = 1
		}
		floorDenied := false
		var deniedIndices []int
		if ctx.elevatorCabFloorDebounceTick >= elevatorCabSenseStableTicks {
			held := slices.Clone(ctx.elevatorCabFloorDebounceHeld)
			pin := ctx.elevatorGrantPIN
			via := ctx.elevatorGrantViaFallback
			elevID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
			for _, fi := range held {
				if !ctx.elevatorFloorChannelAllowed(pin, elevID, fi, via, time.Now()) {
					floorDenied = true
					deniedIndices = held
					break
				}
			}
			if !floorDenied {
				toDispatch = held
			}
		}
		ctx.elevatorMu.Unlock()
		if floorDenied {
			clearElevatorGrantState(ctx)
			ctx.configMu.RLock()
			cfg := ctx.Config
			ctx.configMu.RUnlock()
			log.Printf("INFO: Elevator cab floor input(s) rejected (not permitted for credential or schedule): %v", deniedIndices)
			playSoundSync(cfg, cfg.SoundPinReject)
			elevID := strings.TrimSpace(ctx.effectiveAccessElevatorID())
			denyEx := map[string]any{
				"operation_mode":             mode,
				"elevator_floor_indices":     deniedIndices,
				"access_control_elevator_id": elevID,
			}
			if ctx.DB != nil && elevID != "" && len(deniedIndices) > 0 {
				denyEx["elevator_floor_labels"] = elevatorFloorLogLabels(ctx.DB, elevID, deniedIndices)
			}
			fireEventWebhook(ctx, "elevator_floor_denied", denyEx)
			continue
		}
		if len(toDispatch) == 0 {
			continue
		}
		clearElevatorGrantState(ctx)
		ctx.configMu.RLock()
		cfg := ctx.Config
		ctx.configMu.RUnlock()
		pulseElevatorFloorSelections(ctx, cfg, toDispatch)
		log.Printf("INFO: Elevator cab floor input(s) active %v; dispatch pulse sent.", toDispatch)
		fireEventWebhook(ctx, "elevator_floor_selected", map[string]any{"operation_mode": mode, "elevator_floor_indices": toDispatch})
	}
}

type mqttPairPeerMsg struct {
	Cmd   string `json:"cmd"`
	Token string `json:"token,omitempty"`
}

var mqttPairPeerMu sync.Mutex

func mqttPairPeerMessageHandler(ctx *AppContext) mqtt.MessageHandler {
	return func(_ mqtt.Client, m mqtt.Message) {
		handleMQTTPairPeerPayload(ctx, m.Payload())
	}
}

func handleMQTTPairPeerPayload(ctx *AppContext, payload []byte) {
	mqttPairPeerMu.Lock()
	defer mqttPairPeerMu.Unlock()
	ctx.configMu.RLock()
	tokExpect := strings.TrimSpace(ctx.Config.PairPeerToken)
	mode := NormalizeKeypadOperationMode(ctx.Config.KeypadOperationMode)
	role := normalizePairPeerRole(ctx.Config.PairPeerRole)
	cfg := ctx.Config
	ctx.configMu.RUnlock()
	if !pairedExitSubscribesToPeer(mode, role) {
		return
	}
	var msg mqttPairPeerMsg
	if json.Unmarshal(bytes.TrimSpace(payload), &msg) != nil || strings.TrimSpace(msg.Cmd) == "" {
		log.Println("WARNING: pair-peer MQTT: invalid JSON payload")
		return
	}
	if tokExpect != "" && msg.Token != tokExpect {
		log.Println("WARNING: pair-peer MQTT: rejected (bad token)")
		return
	}
	cmd := strings.ToLower(strings.TrimSpace(msg.Cmd))
	if cmd != "pulse_paired_exit" && cmd != "unlock_peer_exit" {
		return
	}
	log.Println("INFO: pair-peer MQTT: entry station requested coordinated exit unlock; pulsing local door relay.")
	fireEventWebhook(ctx, "mqtt_pair_peer_exit_pulse", map[string]any{"cmd": cmd, "operation_mode": mode})
	if ctx.GPIO != nil {
		ctx.GPIO.ActionPulse("door", cfg.RelayPulseDuration)
	}
}

func publishMQTTPairPeerPulse(ctx *AppContext, cfg DeviceConfig) {
	ctx.mqttMu.RLock()
	client := ctx.MQTTClient
	ctx.mqttMu.RUnlock()
	if client == nil || !client.IsConnected() {
		log.Println("WARNING: pair-peer MQTT publish skipped (client not connected)")
		return
	}
	topic := strings.TrimSpace(cfg.MQTTPairPeerTopic)
	if topic == "" {
		return
	}
	body, err := json.Marshal(mqttPairPeerMsg{Cmd: "pulse_paired_exit", Token: cfg.PairPeerToken})
	if err != nil {
		return
	}
	if t := client.Publish(topic, 1, false, body); t.Wait() && t.Error() != nil {
		log.Printf("WARNING: pair-peer MQTT publish: %v", t.Error())
		return
	}
	log.Printf("INFO: pair-peer MQTT: published exit-unlock hint to %q", topic)
}

// aclDBMu serializes interactive ACL mutations against SQLite (avoids SQLITE_BUSY vs audit log inserts).
var aclDBMu sync.Mutex

func techMenuACLTopLevel() []string {
	return []string{
		"bind", "door", "door_group", "elevator", "elevator_group", "group", "help", "level", "pin", "profile", "summary", "target", "window",
	}
}

// techMenuACLSecondLevel returns verbs or nouns after "acl <category> ".
func techMenuACLSecondLevel(category string) []string {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "bind":
		return []string{"door", "elevator"}
	case "door", "elevator", "door_group", "elevator_group":
		return []string{"add", "list"}
	case "pin":
		return []string{"add", "disable", "enable", "list"}
	case "group":
		return []string{"add", "join", "leave", "list"}
	case "profile":
		return []string{"add", "list"}
	case "window":
		return []string{"add"}
	case "level":
		return []string{"add", "disable", "enable", "list"}
	case "target":
		return []string{"door", "door_group", "elevator", "elevator_group", "list"}
	default:
		return nil
	}
}

// techMenuACLCompleteAddSpace returns whether Tab should append a space after completing `completed`.
func techMenuACLCompleteAddSpace(prefixLower []string, completed string) bool {
	if len(prefixLower) == 0 || prefixLower[0] != "acl" {
		return false
	}
	if len(prefixLower) == 1 {
		return true
	}
	cat := prefixLower[1]
	if sub := techMenuACLSecondLevel(cat); len(prefixLower) == 2 && len(sub) > 0 {
		return true
	}
	// acl <cat> <verb>
	if len(prefixLower) == 3 {
		switch cat {
		case "door", "elevator", "door_group", "elevator_group":
			if prefixLower[2] == "add" || prefixLower[2] == "list" {
				return true
			}
		case "pin":
			if prefixLower[2] == "add" || prefixLower[2] == "list" ||
				prefixLower[2] == "enable" || prefixLower[2] == "disable" {
				return true
			}
		case "group":
			if prefixLower[2] == "add" || prefixLower[2] == "list" ||
				prefixLower[2] == "join" || prefixLower[2] == "leave" {
				return true
			}
		case "profile":
			if prefixLower[2] == "add" || prefixLower[2] == "list" {
				return true
			}
		case "window":
			if prefixLower[2] == "add" {
				return true
			}
		case "level":
			if prefixLower[2] == "add" || prefixLower[2] == "list" ||
				prefixLower[2] == "enable" || prefixLower[2] == "disable" {
				return true
			}
		case "target":
			if prefixLower[2] == "door" || prefixLower[2] == "elevator" ||
				prefixLower[2] == "door_group" || prefixLower[2] == "elevator_group" ||
				prefixLower[2] == "list" {
				return true
			}
		case "bind":
			if prefixLower[2] == "door" || prefixLower[2] == "elevator" {
				return true
			}
		}
	}
	return false
}

func techMenuACLTabMatches(prefix []string, partial string, trailingSpace bool) (matches []string, ok bool) {
	pl := techMenuLowerPrefixSlice(prefix)
	if len(pl) < 1 || pl[0] != "acl" {
		return nil, false
	}
	lowPart := strings.ToLower(partial)
	if trailingSpace {
		lowPart = ""
	}
	switch len(pl) {
	case 1:
		if trailingSpace {
			return append([]string(nil), techMenuACLTopLevel()...), true
		}
		return techMenuFilterPrefixLower(techMenuACLTopLevel(), lowPart), true
	case 2:
		if trailingSpace {
			s := techMenuACLSecondLevel(pl[1])
			if s == nil {
				return nil, true
			}
			return append([]string(nil), s...), true
		}
		s := techMenuACLSecondLevel(pl[1])
		if s == nil {
			return nil, true
		}
		return techMenuFilterPrefixLower(s, lowPart), true
	default:
		// Deeper tokens are user data (ids, numbers); no completion.
		return nil, true
	}
}

func techMenuPrintACLHelp(w io.Writer) {
	fmt.Fprint(w, `
Access control (SQLite access_control.db + device binding)
  Use Tab after "acl " and "acl door " (etc.) to see subcommands.

Binding (which logical door/elevator this controller enforces — saved with cfg save):
  acl bind door <id>              → same as: cfg set access_control_door_id <id>
  acl bind elevator <id>          → same as: cfg set access_control_elevator_id <id>
  Then: cfg save                  persist JSON; door/elevator rows must exist in DB (see below).

Discover:
  acl summary                     current bind ids + row counts
  acl door list | acl elevator list | acl pin list | acl group list
  acl profile list | acl level list | acl target list

Typical setup (door + schedule + PIN + group + level + target):
  1) acl door add east Main_Entrance
  2) acl pin add 123456 Alice
  3) acl group add staff Staff
  4) acl group join staff 123456
  5) acl profile add biz Business_Hours
  6) acl window add biz 1 525 1020        (Mon 08:45–17:00; weekday 0=Sun … 6=Sat, 7=any)
  7) acl level add L1 biz staff L1_label  (time_profile user_group [display_name])
  8) acl target door L1 east
  9) acl bind door east
 10) cfg set access_schedule_enforce true
 11) cfg save

Notes:
  • Use underscores instead of spaces in display names (e.g. Main_Entrance).
  • Times are minutes from midnight (0–1439). Profile timezone: acl profile add id name Asia/Bangkok
  • Enforce schedules only when access_control_*_id matches a row and access_levels target that door/elevator.
  • PINs are stored in access_pins; they are never echoed by these list commands beyond what you typed.

Commands (detail):
  acl help                        this text
  acl summary
  acl bind door|elevator <id>
  acl door add <id> [display_name]
  acl door_group add <id> [display_name]
  acl elevator add <id> [display_name]
  acl elevator_group add <id> [display_name]
  acl pin add <pin> [label]       label optional; enabled by default
  acl pin enable|disable <pin>
  acl group add <id> [display_name]
  acl group join|leave <group_id> <pin>
  acl profile add <id> [display_name [iana_timezone]]
  acl window add <profile_id> <weekday> <start_minute> <end_minute>
  acl level add <level_id> <time_profile_id> <user_group_id> [display_name]
  acl level enable|disable <level_id>
  acl target door|elevator|door_group|elevator_group <level_id> <target_id>
  acl target list
`)
}

func techMenuHandleACL(ctx *AppContext, line string, parts []string) {
	if ctx == nil {
		return
	}
	if len(parts) < 2 {
		techMenuSyncPrint(func(w io.Writer) { techMenuPrintACLHelp(w) })
		return
	}
	sub := strings.ToLower(strings.TrimSpace(parts[1]))
	switch sub {
	case "help", "h", "?":
		techMenuSyncPrint(func(w io.Writer) { techMenuPrintACLHelp(w) })
	case "summary":
		techMenuACLSummary(ctx)
	default:
		if err := techMenuACLDispatch(ctx, parts); err != nil {
			log.Printf("WARNING: acl: %v", err)
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "acl: %v\n", err) })
		}
	}
}

func techMenuACLSummary(ctx *AppContext) {
	ctx.configMu.RLock()
	door := strings.TrimSpace(ctx.Config.AccessControlDoorID)
	elev := strings.TrimSpace(ctx.Config.AccessControlElevatorID)
	enforce := ctx.Config.AccessScheduleEnforce
	ctx.configMu.RUnlock()

	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Device binding: access_control_door_id=%q access_control_elevator_id=%q access_schedule_enforce=%v\n",
			door, elev, enforce)
		if ctx.DB == nil {
			fmt.Fprintln(w, "SQLite: (no database)")
			return
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		type pair struct {
			label string
			table string
		}
		counts := []pair{
			{"doors", "access_doors"},
			{"door_groups", "access_door_groups"},
			{"elevators", "access_elevators"},
			{"elevator_groups", "access_elevator_groups"},
			{"pins", "access_pins"},
			{"user_groups", "access_user_groups"},
			{"user_group_members", "access_user_group_members"},
			{"time_profiles", "access_time_profiles"},
			{"time_windows", "access_time_windows"},
			{"access_levels", "access_levels"},
			{"level_targets", "access_level_targets"},
			{"audit_logs", "logs"},
		}
		for _, c := range counts {
			var n int
			_ = ctx.DB.QueryRow(`SELECT COUNT(*) FROM ` + c.table).Scan(&n)
			fmt.Fprintf(w, "  %-20s %d\n", c.label+":", n)
		}
	})
	log.Println("INFO: Technician menu: acl summary")
}

func techMenuACLDispatch(ctx *AppContext, parts []string) error {
	if len(parts) < 2 {
		return nil
	}
	if ctx.DB == nil {
		return fmt.Errorf("database not available")
	}
	cat := strings.ToLower(strings.TrimSpace(parts[1]))
	switch cat {
	case "bind":
		return techMenuACLCmdBind(ctx, parts)
	case "door":
		return techMenuACLCmdDoor(ctx, parts)
	case "door_group":
		return techMenuACLCmdDoorGroup(ctx, parts)
	case "elevator":
		return techMenuACLCmdElevator(ctx, parts)
	case "elevator_group":
		return techMenuACLCmdElevatorGroup(ctx, parts)
	case "pin":
		return techMenuACLCmdPin(ctx, parts)
	case "group":
		return techMenuACLCmdGroup(ctx, parts)
	case "profile":
		return techMenuACLCmdProfile(ctx, parts)
	case "window":
		return techMenuACLCmdWindow(ctx, parts)
	case "level":
		return techMenuACLCmdLevel(ctx, parts)
	case "target":
		return techMenuACLCmdTarget(ctx, parts)
	default:
		return fmt.Errorf("unknown acl %q — try: acl help (Tab completes subcommands)", parts[1])
	}
}

func techMenuACLCmdBind(ctx *AppContext, parts []string) error {
	if len(parts) < 4 {
		return fmt.Errorf(`usage: acl bind door <id>  |  acl bind elevator <id>
same as cfg set access_control_*_id — use cfg save to persist JSON`)
	}
	kind := strings.ToLower(parts[2])
	id := strings.TrimSpace(parts[3])
	if id == "" {
		return fmt.Errorf("id must not be empty")
	}
	var key string
	switch kind {
	case "door":
		key = "access_control_door_id"
	case "elevator":
		key = "access_control_elevator_id"
	default:
		return fmt.Errorf("bind: want door or elevator, got %q", parts[2])
	}
	if err := techMenuCfgSetValue(ctx, key, id); err != nil {
		return err
	}
	log.Printf("INFO: Technician menu: acl bind %s %q (in memory; cfg save to persist)", kind, id)
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Set %s=%q in memory. Run: cfg save\n", key, id)
		if kind == "door" {
			fmt.Fprintln(w, "Hint: create row if missing: acl door add <id> [display_name]")
		} else {
			fmt.Fprintln(w, "Hint: create row if missing: acl elevator add <id> [display_name]")
		}
	})
	return nil
}

func techMenuACLCmdDoor(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl door add <id> [display_name] | acl door list")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_doors", "id", "display_name")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl door add <id> [display_name]")
		}
		id := strings.TrimSpace(parts[3])
		name := ""
		if len(parts) > 4 {
			name = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if id == "" {
			return fmt.Errorf("door id must not be empty")
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_doors (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(name))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl door add %q", id)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Door %q saved. Bind with: acl bind door %s\n", id, id) })
		return nil
	default:
		return fmt.Errorf("door: use add or list (Tab after 'acl door ')")
	}
}

func techMenuACLCmdDoorGroup(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl door_group add <id> [display_name] | acl door_group list")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_door_groups", "id", "display_name")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl door_group add <id> [display_name]")
		}
		id := strings.TrimSpace(parts[3])
		name := ""
		if len(parts) > 4 {
			name = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if id == "" {
			return fmt.Errorf("door_group id must not be empty")
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_door_groups (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(name))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl door_group add %q", id)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Door group %q saved. acl target door_group <level_id> %s\n", id, id) })
		return nil
	default:
		return fmt.Errorf("door_group: use add or list")
	}
}

func techMenuACLCmdElevator(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl elevator add <id> [display_name] | acl elevator list")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_elevators", "id", "display_name")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl elevator add <id> [display_name]")
		}
		id := strings.TrimSpace(parts[3])
		name := ""
		if len(parts) > 4 {
			name = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if id == "" {
			return fmt.Errorf("elevator id must not be empty")
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_elevators (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(name))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl elevator add %q", id)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Elevator %q saved. Bind with: acl bind elevator %s\n", id, id) })
		return nil
	default:
		return fmt.Errorf("elevator: use add or list")
	}
}

func techMenuACLCmdElevatorGroup(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl elevator_group add <id> [display_name] | acl elevator_group list")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_elevator_groups", "id", "display_name")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl elevator_group add <id> [display_name]")
		}
		id := strings.TrimSpace(parts[3])
		name := ""
		if len(parts) > 4 {
			name = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if id == "" {
			return fmt.Errorf("elevator_group id must not be empty")
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_elevator_groups (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(name))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl elevator_group add %q", id)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Elevator group %q saved. acl target elevator_group <level_id> %s\n", id, id) })
		return nil
	default:
		return fmt.Errorf("elevator_group: use add or list")
	}
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func techMenuACLCmdPin(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl pin add|list|enable|disable …")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_pins", "pin", "label", "enabled")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl pin add <pin> [label]")
		}
		pin := strings.TrimSpace(parts[3])
		label := ""
		if len(parts) > 4 {
			label = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if pin == "" {
			return fmt.Errorf("pin must not be empty")
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_pins (pin, label, enabled) VALUES (?, ?, 1)`, pin, nullIfEmpty(label))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl pin add (enabled)")
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "PIN saved (enabled). Add to a user group: acl group join <group_id> <pin>")
		})
		return nil
	case "enable", "disable":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl pin %s <pin>", verb)
		}
		pin := strings.TrimSpace(parts[3])
		if pin == "" {
			return fmt.Errorf("pin must not be empty")
		}
		en := 1
		if verb == "disable" {
			en = 0
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		res, err := ctx.DB.Exec(`UPDATE access_pins SET enabled = ? WHERE pin = ?`, en, pin)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("no access_pins row for pin %q — use: acl pin add %s", pin, pin)
		}
		log.Printf("INFO: Technician menu: acl pin %s", verb)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "PIN %q enabled=%d\n", pin, en) })
		return nil
	default:
		return fmt.Errorf("pin: use add, list, enable, or disable")
	}
}

func techMenuACLCmdGroup(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl group add|list|join|leave …")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		rows, err := ctx.DB.Query(`SELECT g.id, g.display_name, COUNT(m.pin) FROM access_user_groups g LEFT JOIN access_user_group_members m ON m.group_id = g.id GROUP BY g.id ORDER BY g.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "user_group id | display_name | member_pins")
			for rows.Next() {
				var id, dn sql.NullString
				var cnt int
				if err := rows.Scan(&id, &dn, &cnt); err != nil {
					fmt.Fprintf(w, "(scan error: %v)\n", err)
					return
				}
				disp := ""
				if dn.Valid {
					disp = dn.String
				}
				fmt.Fprintf(w, "  %s | %s | %d\n", id.String, disp, cnt)
			}
		})
		log.Println("INFO: Technician menu: acl group list")
		return rows.Err()
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl group add <id> [display_name]")
		}
		id := strings.TrimSpace(parts[3])
		name := ""
		if len(parts) > 4 {
			name = strings.TrimSpace(strings.Join(parts[4:], " "))
		}
		if id == "" {
			return fmt.Errorf("group id must not be empty")
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_user_groups (id, display_name) VALUES (?, ?)`, id, nullIfEmpty(name))
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl group add %q", id)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "User group %q saved. acl group join %s <pin>\n", id, id) })
		return nil
	case "join":
		if len(parts) < 5 {
			return fmt.Errorf("usage: acl group join <group_id> <pin>")
		}
		gid := strings.TrimSpace(parts[3])
		pin := strings.TrimSpace(parts[4])
		if gid == "" || pin == "" {
			return fmt.Errorf("group_id and pin must not be empty")
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(ctx, "access_user_groups", "id", gid, "user group"); err != nil {
			return err
		}
		if err := techMenuACLEnsureFK(ctx, "access_pins", "pin", pin, "PIN"); err != nil {
			return err
		}
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_user_group_members (group_id, pin) VALUES (?, ?)`, gid, pin)
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl group join %q %q", gid, pin)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "PIN %q added to group %q\n", pin, gid) })
		return nil
	case "leave":
		if len(parts) < 5 {
			return fmt.Errorf("usage: acl group leave <group_id> <pin>")
		}
		gid := strings.TrimSpace(parts[3])
		pin := strings.TrimSpace(parts[4])
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := ctx.DB.Exec(`DELETE FROM access_user_group_members WHERE group_id = ? AND pin = ?`, gid, pin)
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl group leave %q %q", gid, pin)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Removed PIN %q from group %q (if it was present)\n", pin, gid) })
		return nil
	default:
		return fmt.Errorf("group: use add, list, join, or leave")
	}
}

func techMenuACLCreateHint(table string) string {
	switch table {
	case "access_doors":
		return "acl door add <id>"
	case "access_door_groups":
		return "acl door_group add <id>"
	case "access_elevators":
		return "acl elevator add <id>"
	case "access_elevator_groups":
		return "acl elevator_group add <id>"
	case "access_pins":
		return "acl pin add <pin>"
	case "access_user_groups":
		return "acl group add <id>"
	case "access_time_profiles":
		return "acl profile add <id>"
	case "access_levels":
		return "acl level add <level_id> <time_profile_id> <user_group_id>"
	default:
		return "acl help"
	}
}

func techMenuACLEnsureFK(ctx *AppContext, table, col, id, what string) error {
	var dummy string
	err := ctx.DB.QueryRow(`SELECT `+col+` FROM `+table+` WHERE `+col+` = ? LIMIT 1`, id).Scan(&dummy)
	if err == sql.ErrNoRows {
		return fmt.Errorf("unknown %s %q — create it first (%s)", what, id, techMenuACLCreateHint(table))
	}
	return err
}

func techMenuACLCmdProfile(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl profile add|list …")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		return techMenuACLQueryStrings(ctx, "access_time_profiles", "id", "display_name", "iana_timezone")
	case "add":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl profile add <id> [display_name [iana_timezone]]")
		}
		id := strings.TrimSpace(parts[3])
		if id == "" {
			return fmt.Errorf("profile id must not be empty")
		}
		display := ""
		tz := ""
		switch len(parts) {
		case 4:
			break
		case 5:
			display = strings.TrimSpace(parts[4])
		default:
			display = strings.TrimSpace(parts[4])
			tz = strings.TrimSpace(strings.Join(parts[5:], " "))
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_time_profiles (id, display_name, description, iana_timezone) VALUES (?, ?, ?, ?)`,
			id, nullIfEmpty(display), nil, tz)
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl profile add %q", id)
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Time profile %q saved. Add windows: acl window add %s <weekday> <start_min> <end_min>\n", id, id)
			fmt.Fprintln(w, "  weekday: 0=Sun … 6=Sat, 7=any day; minutes 0–1439 (start>end crosses midnight)")
		})
		return nil
	default:
		return fmt.Errorf("profile: use add or list")
	}
}

func techMenuACLCmdWindow(ctx *AppContext, parts []string) error {
	if len(parts) < 7 {
		return fmt.Errorf(`usage: acl window add <profile_id> <weekday> <start_minute> <end_minute>
example: acl window add biz 1 525 1020   (Mon 08:45–17:00)
hint: acl profile list — use existing profile_id`)
	}
	if strings.ToLower(parts[2]) != "add" {
		return fmt.Errorf("window: only add is supported")
	}
	pid := strings.TrimSpace(parts[3])
	wd, err := strconv.Atoi(parts[4])
	if err != nil {
		return fmt.Errorf("weekday: integer 0–7: %w", err)
	}
	sm, err := strconv.Atoi(parts[5])
	if err != nil {
		return fmt.Errorf("start_minute: %w", err)
	}
	em, err := strconv.Atoi(parts[6])
	if err != nil {
		return fmt.Errorf("end_minute: %w", err)
	}
	if wd < 0 || wd > 7 || sm < 0 || sm > 1439 || em < 0 || em > 1439 {
		return fmt.Errorf("weekday must be 0–7, minutes 0–1439")
	}
	aclDBMu.Lock()
	defer aclDBMu.Unlock()
	if err := techMenuACLEnsureFK(ctx, "access_time_profiles", "id", pid, "time profile"); err != nil {
		return err
	}
	_, err = ctx.DB.Exec(`INSERT INTO access_time_windows (time_profile_id, weekday, start_minute, end_minute) VALUES (?, ?, ?, ?)`, pid, wd, sm, em)
	if err != nil {
		return err
	}
	log.Printf("INFO: Technician menu: acl window add profile=%s weekday=%d %d-%d", pid, wd, sm, em)
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Time window added for profile %q. Next: acl level add <level_id> %s <user_group_id> [name]\n", pid, pid)
	})
	return nil
}

func techMenuACLCmdLevel(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl level add|list|enable|disable …")
	}
	verb := strings.ToLower(parts[2])
	switch verb {
	case "list":
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		rows, err := ctx.DB.Query(`SELECT id, display_name, time_profile_id, user_group_id, enabled FROM access_levels ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "id | display_name | time_profile_id | user_group_id | enabled")
			for rows.Next() {
				var id, dn, tp, ug string
				var en int
				if err := rows.Scan(&id, &dn, &tp, &ug, &en); err != nil {
					fmt.Fprintf(w, "(scan error: %v)\n", err)
					return
				}
				fmt.Fprintf(w, "  %s | %s | %s | %s | %d\n", id, dn, tp, ug, en)
			}
		})
		log.Println("INFO: Technician menu: acl level list")
		return rows.Err()
	case "add":
		if len(parts) < 6 {
			return fmt.Errorf(`usage: acl level add <level_id> <time_profile_id> <user_group_id> [display_name]
hint: acl profile list | acl group list`)
		}
		lid := strings.TrimSpace(parts[3])
		tpid := strings.TrimSpace(parts[4])
		ugid := strings.TrimSpace(parts[5])
		dname := ""
		if len(parts) > 6 {
			dname = strings.TrimSpace(strings.Join(parts[6:], " "))
		}
		if lid == "" || tpid == "" || ugid == "" {
			return fmt.Errorf("level_id, time_profile_id, and user_group_id must not be empty")
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		if err := techMenuACLEnsureFK(ctx, "access_time_profiles", "id", tpid, "time profile"); err != nil {
			return err
		}
		if err := techMenuACLEnsureFK(ctx, "access_user_groups", "id", ugid, "user group"); err != nil {
			return err
		}
		_, err := ctx.DB.Exec(`INSERT OR REPLACE INTO access_levels (id, display_name, time_profile_id, user_group_id, enabled) VALUES (?, ?, ?, ?, 1)`,
			lid, nullIfEmpty(dname), tpid, ugid)
		if err != nil {
			return err
		}
		log.Printf("INFO: Technician menu: acl level add %q", lid)
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Access level %q saved (enabled). Grant door/elevator: acl target door %s <door_id>\n", lid, lid)
		})
		return nil
	case "enable", "disable":
		if len(parts) < 4 {
			return fmt.Errorf("usage: acl level %s <level_id>", verb)
		}
		lid := strings.TrimSpace(parts[3])
		en := 1
		if verb == "disable" {
			en = 0
		}
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		res, err := ctx.DB.Exec(`UPDATE access_levels SET enabled = ? WHERE id = ?`, en, lid)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("no access_levels row for id %q — acl level list", lid)
		}
		log.Printf("INFO: Technician menu: acl level %s %q", verb, lid)
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Level %q enabled=%d\n", lid, en) })
		return nil
	default:
		return fmt.Errorf("level: use add, list, enable, or disable")
	}
}

func techMenuACLCmdTarget(ctx *AppContext, parts []string) error {
	if len(parts) < 3 {
		return fmt.Errorf("usage: acl target door|elevator|door_group|elevator_group <level_id> <id> | acl target list")
	}
	verb := strings.ToLower(parts[2])
	if verb == "list" {
		aclDBMu.Lock()
		defer aclDBMu.Unlock()
		rows, err := ctx.DB.Query(`
			SELECT t.id, t.access_level_id, t.door_id, t.door_group_id, t.elevator_id, t.elevator_group_id
			FROM access_level_targets t ORDER BY t.access_level_id, t.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "row_id | level_id | door_id | door_group_id | elevator_id | elevator_group_id")
			for rows.Next() {
				var rid int
				var lid string
				var did, dgid, eid, egid sql.NullString
				if err := rows.Scan(&rid, &lid, &did, &dgid, &eid, &egid); err != nil {
					fmt.Fprintf(w, "(scan error: %v)\n", err)
					return
				}
				fmt.Fprintf(w, "  %d | %s | %v | %v | %v | %v\n", rid, lid, ns(did), ns(dgid), ns(eid), ns(egid))
			}
		})
		log.Println("INFO: Technician menu: acl target list")
		return rows.Err()
	}
	if len(parts) < 5 {
		return fmt.Errorf("usage: acl target %s <level_id> <target_id>", verb)
	}
	lid := strings.TrimSpace(parts[3])
	tid := strings.TrimSpace(parts[4])
	if lid == "" || tid == "" {
		return fmt.Errorf("level_id and target id must not be empty")
	}
	aclDBMu.Lock()
	defer aclDBMu.Unlock()
	if err := techMenuACLEnsureFK(ctx, "access_levels", "id", lid, "access level"); err != nil {
		return err
	}
	var err error
	switch verb {
	case "door":
		if err := techMenuACLEnsureFK(ctx, "access_doors", "id", tid, "door"); err != nil {
			return err
		}
		_, err = ctx.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, ?, NULL, NULL, NULL)`, lid, tid)
	case "door_group":
		if err := techMenuACLEnsureFK(ctx, "access_door_groups", "id", tid, "door group"); err != nil {
			return err
		}
		_, err = ctx.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, NULL, ?, NULL, NULL)`, lid, tid)
	case "elevator":
		if err := techMenuACLEnsureFK(ctx, "access_elevators", "id", tid, "elevator"); err != nil {
			return err
		}
		_, err = ctx.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, NULL, NULL, ?, NULL)`, lid, tid)
	case "elevator_group":
		if err := techMenuACLEnsureFK(ctx, "access_elevator_groups", "id", tid, "elevator group"); err != nil {
			return err
		}
		_, err = ctx.DB.Exec(`INSERT INTO access_level_targets (access_level_id, door_id, door_group_id, elevator_id, elevator_group_id) VALUES (?, NULL, NULL, NULL, ?)`, lid, tid)
	default:
		return fmt.Errorf("target: use door, elevator, door_group, elevator_group, or list")
	}
	if err != nil {
		return err
	}
	log.Printf("INFO: Technician menu: acl target %s level=%q target=%q", verb, lid, tid)
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Target row added. Bind device: acl bind door|elevator <id> (must match this target)\n")
	})
	return nil
}

func ns(s sql.NullString) string {
	if s.Valid {
		return s.String
	}
	return ""
}

func techMenuACLQueryStrings(ctx *AppContext, table string, cols ...string) error {
	if len(cols) == 0 {
		return fmt.Errorf("internal: no columns")
	}
	aclDBMu.Lock()
	defer aclDBMu.Unlock()
	sb := strings.Builder{}
	for i, c := range cols {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(c)
	}
	q := `SELECT ` + sb.String() + ` FROM ` + table + ` ORDER BY 1`
	rows, err := ctx.DB.Query(q)
	if err != nil {
		return err
	}
	defer rows.Close()
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintln(w, strings.Join(cols, " | "))
		for rows.Next() {
			scans := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cols {
				ptrs[i] = &scans[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				fmt.Fprintf(w, "(scan error: %v)\n", err)
				return
			}
			for i, v := range scans {
				if i > 0 {
					fmt.Fprint(w, " | ")
				}
				fmt.Fprint(w, techMenuACLFormatCell(v))
			}
			fmt.Fprintln(w)
		}
	})
	log.Printf("INFO: Technician menu: acl list %s", table)
	return rows.Err()
}

func techMenuACLFormatCell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case int64:
		return strconv.FormatInt(x, 10)
	default:
		return fmt.Sprint(x)
	}
}
