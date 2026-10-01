package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// virtualkeyz2LCDDisplayJSON is the device.lcd_display object in virtualkeyz2.json.
type virtualkeyz2LCDDisplayJSON struct {
	Enabled                 *bool `json:"enabled,omitempty"`
	I2CBus                  *int  `json:"i2c_bus,omitempty"`
	I2CAddress              *int  `json:"i2c_address,omitempty"`
	BacklightTimeoutSeconds *int  `json:"backlight_timeout_seconds,omitempty"`
	I2CDebugEnabled         *bool `json:"i2c_debug_enabled,omitempty"`
}

// virtualkeyz2JSON is the on-disk shape of virtualkeyz2.json (see default file in repo).
type virtualkeyz2JSON struct {
	Device                 virtualkeyz2DeviceJSON `json:"device"`
	GPIO                   virtualkeyz2GPIOJSON   `json:"gpio"`
	TechMenuPrompt         *string                `json:"tech_menu_prompt"`
	ElevatorParameterModes json.RawMessage        `json:"elevator_parameter_modes,omitempty"`
}

type virtualkeyz2DeviceJSON struct {
	HeartbeatInterval                   *string                     `json:"heartbeat_interval"`
	DoorOpenWarningAfter                *string                     `json:"door_open_warning_after"`
	DoorOpenAlarmInterval               *string                     `json:"door_open_alarm_interval"`
	DoorOpenAlarmMaxCount               *int                        `json:"door_open_alarm_max_count"`
	DoorForcedAfterWarnings             *int                        `json:"door_forced_after_warnings"`
	DoorSensorClosedIsLow               *bool                       `json:"door_sensor_closed_is_low"`
	SoundCardName                       *string                     `json:"sound_card_name"`
	SoundStartup                        *string                     `json:"sound_startup"`
	SoundShutdown                       *string                     `json:"sound_shutdown"`
	SoundPinOK                          *string                     `json:"sound_pin_ok"`
	SoundAccessGranted                  *string                     `json:"sound_access_granted,omitempty"`
	SoundPinReject                      *string                     `json:"sound_pin_reject"`
	SoundKeypress                       *string                     `json:"sound_keypress"`
	SoundLightingTimerSet               *string                     `json:"sound_lighting_timer_set,omitempty"`
	SoundLightingTimerExpired           *string                     `json:"sound_lighting_timer_expired,omitempty"`
	SoundDoorOpen                       *string                     `json:"sound_door_open,omitempty"`
	SoundStartupEnabled                 *bool                       `json:"sound_startup_enabled,omitempty"`
	SoundShutdownEnabled                *bool                       `json:"sound_shutdown_enabled,omitempty"`
	SoundPinOKEnabled                   *bool                       `json:"sound_pin_ok_enabled,omitempty"`
	SoundAccessGrantedEnabled           *bool                       `json:"sound_access_granted_enabled,omitempty"`
	SoundPinRejectEnabled               *bool                       `json:"sound_pin_reject_enabled,omitempty"`
	SoundKeypressEnabled                *bool                       `json:"sound_keypress_enabled,omitempty"`
	SoundLightingTimerSetEnabled        *bool                       `json:"sound_lighting_timer_set_enabled,omitempty"`
	SoundLightingTimerExpiredEnabled    *bool                       `json:"sound_lighting_timer_expired_enabled,omitempty"`
	SoundDoorOpenEnabled                *bool                       `json:"sound_door_open_enabled,omitempty"`
	SoundStartupBlocking                *bool                       `json:"sound_startup_blocking,omitempty"`
	SoundShutdownBlocking               *bool                       `json:"sound_shutdown_blocking,omitempty"`
	SoundPinOKBlocking                  *bool                       `json:"sound_pin_ok_blocking,omitempty"`
	SoundAccessGrantedBlocking          *bool                       `json:"sound_access_granted_blocking,omitempty"`
	SoundPinRejectBlocking              *bool                       `json:"sound_pin_reject_blocking,omitempty"`
	SoundKeypressBlocking               *bool                       `json:"sound_keypress_blocking,omitempty"`
	SoundLightingTimerSetBlocking       *bool                       `json:"sound_lighting_timer_set_blocking,omitempty"`
	SoundLightingTimerExpiredBlocking   *bool                       `json:"sound_lighting_timer_expired_blocking,omitempty"`
	SoundDoorOpenBlocking               *bool                       `json:"sound_door_open_blocking,omitempty"`
	LogLevel                            *string                     `json:"log_level"`
	PinLength                           *int                        `json:"pin_length"`
	RelayPulseDuration                  *string                     `json:"relay_pulse_duration"`
	PinRejectBuzzerAfterAttempts        *int                        `json:"pin_reject_buzzer_after_attempts"`
	BuzzerRelayPulseDuration            *string                     `json:"buzzer_relay_pulse_duration"`
	MQTTEnabled                         *bool                       `json:"mqtt_enabled"`
	MQTTBroker                          *string                     `json:"mqtt_broker"`
	MQTTClientID                        *string                     `json:"mqtt_client_id"`
	MQTTUsername                        *string                     `json:"mqtt_username"`
	MQTTPassword                        *string                     `json:"mqtt_password"`
	MQTTCommandTopic                    *string                     `json:"mqtt_command_topic"`
	MQTTStatusTopic                     *string                     `json:"mqtt_status_topic"`
	MQTTCommandToken                    *string                     `json:"mqtt_command_token"`
	TechMenuHistoryMax                  *int                        `json:"tech_menu_history_max"`
	KeypadInterDigitTimeout             *string                     `json:"keypad_inter_digit_timeout"`
	KeypadSessionTimeout                *string                     `json:"keypad_session_timeout"`
	PinEntryFeedbackDelay               *string                     `json:"pin_entry_feedback_delay"`
	PinLockoutEnabled                   *bool                       `json:"pin_lockout_enabled"`
	PinLockoutAfterAttempts             *int                        `json:"pin_lockout_after_attempts"`
	PinLockoutDuration                  *string                     `json:"pin_lockout_duration"`
	PinLockoutOverridePin               *string                     `json:"pin_lockout_override_pin"`
	FallbackAccessPin                   *string                     `json:"fallback_access_pin"`
	WebhookEventEnabled                 *bool                       `json:"webhook_event_enabled"`
	WebhookEventURL                     *string                     `json:"webhook_event_url"`
	WebhookEventTokenEnabled            *bool                       `json:"webhook_event_token_enabled"`
	WebhookEventToken                   *string                     `json:"webhook_event_token"`
	WebhookEventTypes                   *map[string]bool            `json:"webhook_event_types,omitempty"`
	WebhookEventEndpoints               *[]WebhookEventEndpoint     `json:"webhook_event_endpoints,omitempty"`
	WebhookHeartbeatEnabled             *bool                       `json:"webhook_heartbeat_enabled"`
	WebhookHeartbeatURL                 *string                     `json:"webhook_heartbeat_url"`
	WebhookHeartbeatTokenEnabled        *bool                       `json:"webhook_heartbeat_token_enabled"`
	WebhookHeartbeatToken               *string                     `json:"webhook_heartbeat_token"`
	WebhookHTTPTimeout                  *string                     `json:"webhook_http_timeout,omitempty"`
	WebhookMaxConcurrent                *int                        `json:"webhook_max_concurrent,omitempty"`
	WebhookCircuitBreakerEnabled        *bool                       `json:"webhook_circuit_breaker_enabled,omitempty"`
	WebhookCircuitFailureThreshold      *int                        `json:"webhook_circuit_failure_threshold,omitempty"`
	WebhookCircuitOpenDuration          *string                     `json:"webhook_circuit_open_duration,omitempty"`
	KeypadOperationMode                 *string                     `json:"keypad_operation_mode"`
	KeypadEvdevPath                     *string                     `json:"keypad_evdev_path"`
	KeypadExitEvdevPath                 *string                     `json:"keypad_exit_evdev_path"`
	ScannerDevicePath                   *string                     `json:"scanner_device_path,omitempty"`
	ScannerEnabled                      *bool                       `json:"scanner_enabled,omitempty"`
	MaxDevicesPerUser                   *int                        `json:"max_devices_per_user,omitempty"`
	QRTimeWindowSeconds                 *int                        `json:"qr_time_window_seconds,omitempty"`
	StaticTestQRCode                    *string                     `json:"static_test_qr_code,omitempty"`
	StaticTestQRCodeEnabled             *bool                       `json:"static_test_qr_code_enabled,omitempty"`
	PairPeerRole                        *string                     `json:"pair_peer_role"`
	MQTTPairPeerTopic                   *string                     `json:"mqtt_pair_peer_topic"`
	PairPeerToken                       *string                     `json:"pair_peer_token"`
	ElevatorFloorWaitTimeout            *string                     `json:"elevator_floor_wait_timeout"`
	ElevatorWaitFloorCabSense           *string                     `json:"elevator_wait_floor_cab_sense"`
	ElevatorFloorInputPins              *string                     `json:"elevator_floor_input_pins"`
	ElevatorPredefinedFloor             *int                        `json:"elevator_predefined_floor"`
	ElevatorPredefinedFloors            *string                     `json:"elevator_predefined_floors"`
	ElevatorDispatchPulseDuration       *string                     `json:"elevator_dispatch_pulse_duration"`
	ElevatorFloorDispatchPulseDurations *string                     `json:"elevator_floor_dispatch_pulse_durations"`
	ElevatorEnablePulseDuration         *string                     `json:"elevator_enable_pulse_duration"`
	DualKeypadRejectExitWithoutEntry    *bool                       `json:"dual_keypad_reject_exit_without_entry"`
	AccessControlDoorID                 *string                     `json:"access_control_door_id,omitempty"`
	AccessControlElevatorID             *string                     `json:"access_control_elevator_id,omitempty"`
	AccessScheduleEnforce               *bool                       `json:"access_schedule_enforce,omitempty"`
	AccessScheduleApplyToFallbackPin    *bool                       `json:"access_schedule_apply_to_fallback_pin,omitempty"`
	AccessExceptionSiteTimezone         *string                     `json:"access_exception_site_timezone,omitempty"`
	LightingTimeout                     *string                     `json:"lighting_timeout,omitempty"`
	FiremansServiceEnabled              *bool                       `json:"firemans_service_enabled,omitempty"`
	SoundFiremansActivated              *string                     `json:"sound_firemans_activated,omitempty"`
	SoundFiremansDeactivated            *string                     `json:"sound_firemans_deactivated,omitempty"`
	SoundFiremansActivatedEnabled       *bool                       `json:"sound_firemans_activated_enabled,omitempty"`
	SoundFiremansDeactivatedEnabled     *bool                       `json:"sound_firemans_deactivated_enabled,omitempty"`
	SoundFiremansActivatedBlocking      *bool                       `json:"sound_firemans_activated_blocking,omitempty"`
	SoundFiremansDeactivatedBlocking    *bool                       `json:"sound_firemans_deactivated_blocking,omitempty"`
	AutomaticDoorOperatorPulseDuration  *string                     `json:"automatic_door_operator_pulse_duration,omitempty"`
	IntercomCameraTriggerPulseDuration  *string                     `json:"intercom_camera_trigger_pulse_duration,omitempty"`
	LCDDisplay                          *virtualkeyz2LCDDisplayJSON `json:"lcd_display,omitempty"`
}

type virtualkeyz2GPIOJSON struct {
	RelayOutputMode                     *string `json:"relay_output_mode"`
	MCP23017I2CBus                      *int    `json:"mcp23017_i2c_bus"`
	MCP23017I2CAddr                     *int    `json:"mcp23017_i2c_addr"`
	XL9535I2CBus                        *int    `json:"xl9535_i2c_bus"`
	XL9535I2CAddr                       *int    `json:"xl9535_i2c_addr"`
	I2CBusRecoverySCLBCM                *int    `json:"i2c_bus_recovery_scl_bcm,omitempty"`
	DoorRelayPin                        *int    `json:"door_relay_pin"`
	DoorRelayActiveLow                  *bool   `json:"door_relay_active_low"`
	BuzzerRelayPin                      *int    `json:"buzzer_relay_pin"`
	BuzzerRelayActiveLow                *bool   `json:"buzzer_relay_active_low"`
	DoorSensorPin                       *int    `json:"door_sensor_pin"`
	HeartbeatLEDPin                     *int    `json:"heartbeat_led_pin"`
	ExitButtonPin                       *int    `json:"exit_button_pin"`
	ExitButtonActiveLow                 *bool   `json:"exit_button_active_low"`
	EntryButtonPin                      *int    `json:"entry_button_pin"`
	EntryButtonActiveLow                *bool   `json:"entry_button_active_low"`
	ElevatorDispatchRelayPin            *int    `json:"elevator_dispatch_relay_pin"`
	ElevatorDispatchActiveLow           *bool   `json:"elevator_dispatch_active_low"`
	ElevatorEnableRelayPin              *int    `json:"elevator_enable_relay_pin"`
	ElevatorEnableActiveLow             *bool   `json:"elevator_enable_active_low"`
	ElevatorFloorDispatchPins           *string `json:"elevator_floor_dispatch_pins"`
	ElevatorPredefinedEnablePins        *string `json:"elevator_predefined_enable_pins"`
	ElevatorWaitFloorEnablePins         *string `json:"elevator_wait_floor_enable_pins"`
	LightingButtonPin                   *int    `json:"lighting_button_pin"`
	LightingButtonActiveLow             *bool   `json:"lighting_button_active_low"`
	LightingRelayPin                    *int    `json:"lighting_relay_pin"`
	LightingRelayActiveLow              *bool   `json:"lighting_relay_active_low"`
	FiremansServiceInputPin             *int    `json:"firemans_service_input_pin,omitempty"`
	FiremansServiceActiveLow            *bool   `json:"firemans_service_active_low,omitempty"`
	AutomaticDoorOperatorRelayPin       *int    `json:"automatic_door_operator_relay_pin,omitempty"`
	AutomaticDoorOperatorRelayActiveLow *bool   `json:"automatic_door_operator_relay_active_low,omitempty"`
	IntercomCameraTriggerRelayPin       *int    `json:"intercom_camera_trigger_relay_pin,omitempty"`
	IntercomCameraTriggerRelayActiveLow *bool   `json:"intercom_camera_trigger_relay_active_low,omitempty"`
	FireAlarmInterfacePin               *int    `json:"fire_alarm_interface_pin,omitempty"`
	FireAlarmInterfaceActiveLow         *bool   `json:"fire_alarm_interface_active_low,omitempty"`
	TamperSwitchPin                     *int    `json:"tamper_switch_pin,omitempty"`
	TamperSwitchActiveLow               *bool   `json:"tamper_switch_active_low,omitempty"`
	MotionSensorPin                     *int    `json:"motion_sensor_pin,omitempty"`
	MotionSensorActiveLow               *bool   `json:"motion_sensor_active_low,omitempty"`
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
	return parseCommaList(s, func(p string) (uint8, error) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, fmt.Errorf("gpio.%s: invalid integer %q: %w", field, p, err)
		}
		return relayPinUint8(field, n, relayMode)
	})
}

// parseCommaList splits a comma-separated list, skipping empty items, and converts each trimmed
// item with parse. An empty or blank s yields nil.
func parseCommaList[T any](s string, parse func(string) (T, error)) ([]T, error) {
	var out []T
	for p := range strings.SplitSeq(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := parse(p)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func parseCommaDurationList(section, field, s string) ([]time.Duration, error) {
	return parseCommaList(s, func(p string) (time.Duration, error) {
		d, err := time.ParseDuration(p)
		if err != nil {
			return 0, fmt.Errorf("%s.%s: invalid duration %q: %w", section, field, p, err)
		}
		return d, nil
	})
}

func parseCommaIntList(section, field, s string) ([]int, error) {
	return parseCommaList(s, func(p string) (int, error) {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, fmt.Errorf("%s.%s: invalid integer %q: %w", section, field, p, err)
		}
		return n, nil
	})
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
	return min(max(d, minD), maxD)
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
	if c.DoorOpenWarningAfter <= 0 {
		c.DoorOpenWarningAfter = 10 * time.Second
	} else {
		c.DoorOpenWarningAfter = clampDuration(c.DoorOpenWarningAfter, 1*time.Second, 24*time.Hour)
	}
	if c.DoorOpenAlarmInterval <= 0 {
		c.DoorOpenAlarmInterval = 30 * time.Second
	} else {
		c.DoorOpenAlarmInterval = clampDuration(c.DoorOpenAlarmInterval, 1*time.Second, 24*time.Hour)
	}
	if c.DoorOpenAlarmMaxCount < 0 {
		c.DoorOpenAlarmMaxCount = 0
	} else if c.DoorOpenAlarmMaxCount > 10000 {
		c.DoorOpenAlarmMaxCount = 10000
	}
	if c.DoorForcedAfterWarnings < 0 {
		c.DoorForcedAfterWarnings = 0
	} else if c.DoorForcedAfterWarnings > 10000 {
		c.DoorForcedAfterWarnings = 10000
	}
	if c.LightingTimeout <= 0 {
		c.LightingTimeout = 30 * time.Minute
	} else {
		c.LightingTimeout = clampDuration(c.LightingTimeout, 5*time.Second, 24*time.Hour)
	}
	if c.IntercomCameraTriggerPulseDuration <= 0 {
		c.IntercomCameraTriggerPulseDuration = 800 * time.Millisecond
	} else {
		c.IntercomCameraTriggerPulseDuration = clampDuration(c.IntercomCameraTriggerPulseDuration, 50*time.Millisecond, 60*time.Second)
	}
	if c.AutomaticDoorOperatorPulseDuration < 0 {
		c.AutomaticDoorOperatorPulseDuration = 0
	} else if c.AutomaticDoorOperatorPulseDuration > 0 {
		c.AutomaticDoorOperatorPulseDuration = clampDuration(c.AutomaticDoorOperatorPulseDuration, 50*time.Millisecond, 60*time.Second)
	}
	normalizeWebhookOutboundHTTP(c)
	normalizeOperationModeConfig(c)
	normalizeLCDDisplaySettings(&c.LCDDisplay)
}

func normalizeWebhookOutboundHTTP(c *DeviceConfig) {
	if c.WebhookHTTPTimeout <= 0 {
		c.WebhookHTTPTimeout = 25 * time.Second
	} else {
		c.WebhookHTTPTimeout = clampDuration(c.WebhookHTTPTimeout, 5*time.Second, 120*time.Second)
	}
	if c.WebhookMaxConcurrent <= 0 {
		c.WebhookMaxConcurrent = 16
	} else if c.WebhookMaxConcurrent < 1 {
		c.WebhookMaxConcurrent = 1
	} else if c.WebhookMaxConcurrent > 256 {
		c.WebhookMaxConcurrent = 256
	}
	if !c.WebhookCircuitBreakerEnabled {
		return
	}
	if c.WebhookCircuitFailureThreshold <= 0 {
		c.WebhookCircuitFailureThreshold = 5
	} else if c.WebhookCircuitFailureThreshold > 100 {
		c.WebhookCircuitFailureThreshold = 100
	}
	if c.WebhookCircuitOpenDuration <= 0 {
		c.WebhookCircuitOpenDuration = 60 * time.Second
	} else {
		c.WebhookCircuitOpenDuration = clampDuration(c.WebhookCircuitOpenDuration, time.Second, time.Hour)
	}
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
	if strings.TrimSpace(c.ScannerDevicePath) == "" {
		c.ScannerDevicePath = "/dev/input/by-id/usb-YUREN_Yuren_HID_FS_Keyboard_SN_20190000-event-kbd"
	}
	if c.MaxDevicesPerUser <= 0 {
		c.MaxDevicesPerUser = 3
	} else if c.MaxDevicesPerUser > 128 {
		c.MaxDevicesPerUser = 128
	}
	if c.QRTimeWindowSeconds <= 0 {
		c.QRTimeWindowSeconds = 30
	} else if c.QRTimeWindowSeconds > 600 {
		c.QRTimeWindowSeconds = 600
	}
	c.StaticTestQRCode = strings.TrimSpace(c.StaticTestQRCode)
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
	if err := applyJSONDuration(&app.Config.DoorOpenAlarmInterval, "device", "door_open_alarm_interval", d.DoorOpenAlarmInterval); err != nil {
		return err
	}
	if d.DoorOpenAlarmMaxCount != nil {
		app.Config.DoorOpenAlarmMaxCount = *d.DoorOpenAlarmMaxCount
	}
	if d.DoorForcedAfterWarnings != nil {
		app.Config.DoorForcedAfterWarnings = *d.DoorForcedAfterWarnings
	}
	if err := applyJSONDuration(&app.Config.RelayPulseDuration, "device", "relay_pulse_duration", d.RelayPulseDuration); err != nil {
		return err
	}
	if err := applyJSONDuration(&app.Config.BuzzerRelayPulseDuration, "device", "buzzer_relay_pulse_duration", d.BuzzerRelayPulseDuration); err != nil {
		return err
	}
	if err := applyJSONDuration(&app.Config.AutomaticDoorOperatorPulseDuration, "device", "automatic_door_operator_pulse_duration", d.AutomaticDoorOperatorPulseDuration); err != nil {
		return err
	}
	if err := applyJSONDuration(&app.Config.IntercomCameraTriggerPulseDuration, "device", "intercom_camera_trigger_pulse_duration", d.IntercomCameraTriggerPulseDuration); err != nil {
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
	if d.SoundAccessGranted != nil {
		app.Config.SoundAccessGranted = *d.SoundAccessGranted
	}
	if d.SoundPinReject != nil {
		app.Config.SoundPinReject = *d.SoundPinReject
	}
	if d.SoundKeypress != nil {
		app.Config.SoundKeypress = *d.SoundKeypress
	}
	if d.SoundLightingTimerSet != nil {
		app.Config.SoundLightingTimerSet = *d.SoundLightingTimerSet
	}
	if d.SoundLightingTimerExpired != nil {
		app.Config.SoundLightingTimerExpired = *d.SoundLightingTimerExpired
	}
	if d.SoundDoorOpen != nil {
		app.Config.SoundDoorOpen = *d.SoundDoorOpen
	}
	if d.SoundStartupEnabled != nil {
		app.Config.SoundStartupEnabled = *d.SoundStartupEnabled
	}
	if d.SoundShutdownEnabled != nil {
		app.Config.SoundShutdownEnabled = *d.SoundShutdownEnabled
	}
	if d.SoundPinOKEnabled != nil {
		app.Config.SoundPinOKEnabled = *d.SoundPinOKEnabled
	}
	if d.SoundAccessGrantedEnabled != nil {
		app.Config.SoundAccessGrantedEnabled = *d.SoundAccessGrantedEnabled
	}
	if d.SoundPinRejectEnabled != nil {
		app.Config.SoundPinRejectEnabled = *d.SoundPinRejectEnabled
	}
	if d.SoundKeypressEnabled != nil {
		app.Config.SoundKeypressEnabled = *d.SoundKeypressEnabled
	}
	if d.SoundLightingTimerSetEnabled != nil {
		app.Config.SoundLightingTimerSetEnabled = *d.SoundLightingTimerSetEnabled
	}
	if d.SoundLightingTimerExpiredEnabled != nil {
		app.Config.SoundLightingTimerExpiredEnabled = *d.SoundLightingTimerExpiredEnabled
	}
	if d.SoundDoorOpenEnabled != nil {
		app.Config.SoundDoorOpenEnabled = *d.SoundDoorOpenEnabled
	}
	if d.SoundStartupBlocking != nil {
		app.Config.SoundStartupBlocking = *d.SoundStartupBlocking
	}
	if d.SoundShutdownBlocking != nil {
		app.Config.SoundShutdownBlocking = *d.SoundShutdownBlocking
	}
	if d.SoundPinOKBlocking != nil {
		app.Config.SoundPinOKBlocking = *d.SoundPinOKBlocking
	}
	if d.SoundAccessGrantedBlocking != nil {
		app.Config.SoundAccessGrantedBlocking = *d.SoundAccessGrantedBlocking
	}
	if d.SoundPinRejectBlocking != nil {
		app.Config.SoundPinRejectBlocking = *d.SoundPinRejectBlocking
	}
	if d.SoundKeypressBlocking != nil {
		app.Config.SoundKeypressBlocking = *d.SoundKeypressBlocking
	}
	if d.SoundLightingTimerSetBlocking != nil {
		app.Config.SoundLightingTimerSetBlocking = *d.SoundLightingTimerSetBlocking
	}
	if d.SoundLightingTimerExpiredBlocking != nil {
		app.Config.SoundLightingTimerExpiredBlocking = *d.SoundLightingTimerExpiredBlocking
	}
	if d.SoundDoorOpenBlocking != nil {
		app.Config.SoundDoorOpenBlocking = *d.SoundDoorOpenBlocking
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
	if d.WebhookEventTypes != nil {
		app.Config.WebhookEventTypes = cloneStringBoolMap(*d.WebhookEventTypes)
	}
	if d.WebhookEventEndpoints != nil {
		app.Config.WebhookEventEndpoints = cloneWebhookEventEndpoints(*d.WebhookEventEndpoints)
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
	if err := applyJSONDuration(&app.Config.WebhookHTTPTimeout, "device", "webhook_http_timeout", d.WebhookHTTPTimeout); err != nil {
		return err
	}
	if d.WebhookMaxConcurrent != nil {
		app.Config.WebhookMaxConcurrent = *d.WebhookMaxConcurrent
	}
	if d.WebhookCircuitBreakerEnabled != nil {
		app.Config.WebhookCircuitBreakerEnabled = *d.WebhookCircuitBreakerEnabled
	}
	if d.WebhookCircuitFailureThreshold != nil {
		app.Config.WebhookCircuitFailureThreshold = *d.WebhookCircuitFailureThreshold
	}
	if err := applyJSONDuration(&app.Config.WebhookCircuitOpenDuration, "device", "webhook_circuit_open_duration", d.WebhookCircuitOpenDuration); err != nil {
		return err
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
	if d.ScannerDevicePath != nil {
		app.Config.ScannerDevicePath = *d.ScannerDevicePath
	}
	if d.ScannerEnabled != nil {
		app.Config.ScannerEnabled = *d.ScannerEnabled
	}
	if d.MaxDevicesPerUser != nil {
		app.Config.MaxDevicesPerUser = *d.MaxDevicesPerUser
	}
	if d.QRTimeWindowSeconds != nil {
		app.Config.QRTimeWindowSeconds = *d.QRTimeWindowSeconds
	}
	if d.StaticTestQRCode != nil {
		app.Config.StaticTestQRCode = *d.StaticTestQRCode
	}
	if d.StaticTestQRCodeEnabled != nil {
		app.Config.StaticTestQRCodeEnabled = *d.StaticTestQRCodeEnabled
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
	if d.AccessExceptionSiteTimezone != nil {
		app.Config.AccessExceptionSiteTimezone = strings.TrimSpace(*d.AccessExceptionSiteTimezone)
	}
	if err := applyJSONDuration(&app.Config.LightingTimeout, "device", "lighting_timeout", d.LightingTimeout); err != nil {
		return err
	}
	if d.FiremansServiceEnabled != nil {
		app.Config.FiremansServiceEnabled = *d.FiremansServiceEnabled
	}
	if d.SoundFiremansActivated != nil {
		app.Config.SoundFiremansActivated = strings.TrimSpace(*d.SoundFiremansActivated)
	}
	if d.SoundFiremansDeactivated != nil {
		app.Config.SoundFiremansDeactivated = strings.TrimSpace(*d.SoundFiremansDeactivated)
	}
	if d.SoundFiremansActivatedEnabled != nil {
		app.Config.SoundFiremansActivatedEnabled = *d.SoundFiremansActivatedEnabled
	}
	if d.SoundFiremansDeactivatedEnabled != nil {
		app.Config.SoundFiremansDeactivatedEnabled = *d.SoundFiremansDeactivatedEnabled
	}
	if d.SoundFiremansActivatedBlocking != nil {
		app.Config.SoundFiremansActivatedBlocking = *d.SoundFiremansActivatedBlocking
	}
	if d.SoundFiremansDeactivatedBlocking != nil {
		app.Config.SoundFiremansDeactivatedBlocking = *d.SoundFiremansDeactivatedBlocking
	}
	if d.LCDDisplay != nil {
		ld := d.LCDDisplay
		if ld.Enabled != nil {
			app.Config.LCDDisplay.Enabled = *ld.Enabled
		}
		if ld.I2CBus != nil {
			app.Config.LCDDisplay.I2CBus = *ld.I2CBus
		}
		if ld.I2CAddress != nil {
			a := *ld.I2CAddress
			if a < 0 || a > 255 {
				return fmt.Errorf("device.lcd_display.i2c_address: %d out of range 0-255", a)
			}
			app.Config.LCDDisplay.I2CAddr = uint8(a)
		}
		if ld.BacklightTimeoutSeconds != nil {
			app.Config.LCDDisplay.BacklightTimeout = time.Duration(*ld.BacklightTimeoutSeconds) * time.Second
		}
		if ld.I2CDebugEnabled != nil {
			app.Config.LCDDisplay.I2CDebugEnabled = *ld.I2CDebugEnabled
		}
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
	if g.I2CBusRecoverySCLBCM != nil {
		u, err := bcmUint8("i2c_bus_recovery_scl_bcm", *g.I2CBusRecoverySCLBCM)
		if err != nil {
			return err
		}
		app.GPIOSettings.I2CBusRecoverySCLBCM = u
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
	if g.LightingButtonPin != nil {
		u, err := bcmUint8("lighting_button_pin", *g.LightingButtonPin)
		if err != nil {
			return err
		}
		app.GPIOSettings.LightingButtonPin = u
	}
	if g.LightingButtonActiveLow != nil {
		app.GPIOSettings.LightingButtonActiveLow = *g.LightingButtonActiveLow
	}
	if g.LightingRelayPin != nil {
		u, err := relayPinUint8("lighting_relay_pin", *g.LightingRelayPin, relayMode)
		if err != nil {
			return err
		}
		app.GPIOSettings.LightingRelayPin = u
	}
	if g.LightingRelayActiveLow != nil {
		app.GPIOSettings.LightingRelayActiveLow = *g.LightingRelayActiveLow
	}
	if g.FiremansServiceInputPin != nil {
		u, err := bcmUint8("firemans_service_input_pin", *g.FiremansServiceInputPin)
		if err != nil {
			return err
		}
		app.GPIOSettings.FiremansServiceInputPin = u
	}
	if g.FiremansServiceActiveLow != nil {
		app.GPIOSettings.FiremansServiceActiveLow = *g.FiremansServiceActiveLow
	}
	if g.AutomaticDoorOperatorRelayPin != nil {
		u, err := relayPinUint8("automatic_door_operator_relay_pin", *g.AutomaticDoorOperatorRelayPin, relayMode)
		if err != nil {
			return err
		}
		app.GPIOSettings.AutomaticDoorOperatorRelayPin = u
	}
	if g.AutomaticDoorOperatorRelayActiveLow != nil {
		app.GPIOSettings.AutomaticDoorOperatorRelayActiveLow = *g.AutomaticDoorOperatorRelayActiveLow
	}
	if g.IntercomCameraTriggerRelayPin != nil {
		u, err := relayPinUint8("intercom_camera_trigger_relay_pin", *g.IntercomCameraTriggerRelayPin, relayMode)
		if err != nil {
			return err
		}
		app.GPIOSettings.IntercomCameraTriggerRelayPin = u
	}
	if g.IntercomCameraTriggerRelayActiveLow != nil {
		app.GPIOSettings.IntercomCameraTriggerRelayActiveLow = *g.IntercomCameraTriggerRelayActiveLow
	}
	if g.FireAlarmInterfacePin != nil {
		u, err := bcmUint8("fire_alarm_interface_pin", *g.FireAlarmInterfacePin)
		if err != nil {
			return err
		}
		app.GPIOSettings.FireAlarmInterfacePin = u
	}
	if g.FireAlarmInterfaceActiveLow != nil {
		app.GPIOSettings.FireAlarmInterfaceActiveLow = *g.FireAlarmInterfaceActiveLow
	}
	if g.TamperSwitchPin != nil {
		u, err := bcmUint8("tamper_switch_pin", *g.TamperSwitchPin)
		if err != nil {
			return err
		}
		app.GPIOSettings.TamperSwitchPin = u
	}
	if g.TamperSwitchActiveLow != nil {
		app.GPIOSettings.TamperSwitchActiveLow = *g.TamperSwitchActiveLow
	}
	if g.MotionSensorPin != nil {
		u, err := bcmUint8("motion_sensor_pin", *g.MotionSensorPin)
		if err != nil {
			return err
		}
		app.GPIOSettings.MotionSensorPin = u
	}
	if g.MotionSensorActiveLow != nil {
		app.GPIOSettings.MotionSensorActiveLow = *g.MotionSensorActiveLow
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
