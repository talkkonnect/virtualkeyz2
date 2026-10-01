package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// virtualkeyz2PersistFile is the full JSON document written by cfg save.
type virtualkeyz2PersistFile struct {
	Device                 virtualkeyz2PersistDevice `json:"device"`
	GPIO                   virtualkeyz2PersistGPIO   `json:"gpio"`
	TechMenuPrompt         string                    `json:"tech_menu_prompt"`
	ElevatorParameterModes json.RawMessage           `json:"elevator_parameter_modes,omitempty"`
}

type virtualkeyz2PersistDevice struct {
	HeartbeatInterval                   string                 `json:"heartbeat_interval"`
	DoorOpenWarningAfter                string                 `json:"door_open_warning_after"`
	DoorOpenAlarmInterval               string                 `json:"door_open_alarm_interval"`
	DoorOpenAlarmMaxCount               int                    `json:"door_open_alarm_max_count"`
	DoorForcedAfterWarnings             int                    `json:"door_forced_after_warnings"`
	DoorSensorClosedIsLow               bool                   `json:"door_sensor_closed_is_low"`
	SoundCardName                       string                 `json:"sound_card_name"`
	SoundStartup                        string                 `json:"sound_startup"`
	SoundShutdown                       string                 `json:"sound_shutdown"`
	SoundPinOK                          string                 `json:"sound_pin_ok"`
	SoundAccessGranted                  string                 `json:"sound_access_granted"`
	SoundPinReject                      string                 `json:"sound_pin_reject"`
	SoundKeypress                       string                 `json:"sound_keypress"`
	SoundLightingTimerSet               string                 `json:"sound_lighting_timer_set"`
	SoundLightingTimerExpired           string                 `json:"sound_lighting_timer_expired"`
	SoundDoorOpen                       string                 `json:"sound_door_open"`
	SoundDoorbell                       string                 `json:"sound_doorbell"`
	SoundCancel                         string                 `json:"sound_cancel"`
	SoundStartupEnabled                 bool                   `json:"sound_startup_enabled"`
	SoundShutdownEnabled                bool                   `json:"sound_shutdown_enabled"`
	SoundPinOKEnabled                   bool                   `json:"sound_pin_ok_enabled"`
	SoundAccessGrantedEnabled           bool                   `json:"sound_access_granted_enabled"`
	SoundPinRejectEnabled               bool                   `json:"sound_pin_reject_enabled"`
	SoundKeypressEnabled                bool                   `json:"sound_keypress_enabled"`
	SoundLightingTimerSetEnabled        bool                   `json:"sound_lighting_timer_set_enabled"`
	SoundLightingTimerExpiredEnabled    bool                   `json:"sound_lighting_timer_expired_enabled"`
	SoundDoorOpenEnabled                bool                   `json:"sound_door_open_enabled"`
	SoundDoorbellEnabled                bool                   `json:"sound_doorbell_enabled"`
	SoundCancelEnabled                  bool                   `json:"sound_cancel_enabled"`
	SoundStartupBlocking                bool                   `json:"sound_startup_blocking"`
	SoundShutdownBlocking               bool                   `json:"sound_shutdown_blocking"`
	SoundPinOKBlocking                  bool                   `json:"sound_pin_ok_blocking"`
	SoundAccessGrantedBlocking          bool                   `json:"sound_access_granted_blocking"`
	SoundPinRejectBlocking              bool                   `json:"sound_pin_reject_blocking"`
	SoundKeypressBlocking               bool                   `json:"sound_keypress_blocking"`
	SoundLightingTimerSetBlocking       bool                   `json:"sound_lighting_timer_set_blocking"`
	SoundLightingTimerExpiredBlocking   bool                   `json:"sound_lighting_timer_expired_blocking"`
	SoundDoorOpenBlocking               bool                   `json:"sound_door_open_blocking"`
	SoundDoorbellBlocking               bool                   `json:"sound_doorbell_blocking"`
	SoundCancelBlocking                 bool                   `json:"sound_cancel_blocking"`
	SoundFiremansActivated              string                 `json:"sound_firemans_activated"`
	SoundFiremansDeactivated            string                 `json:"sound_firemans_deactivated"`
	SoundFiremansActivatedEnabled       bool                   `json:"sound_firemans_activated_enabled"`
	SoundFiremansDeactivatedEnabled     bool                   `json:"sound_firemans_deactivated_enabled"`
	SoundFiremansActivatedBlocking      bool                   `json:"sound_firemans_activated_blocking"`
	SoundFiremansDeactivatedBlocking    bool                   `json:"sound_firemans_deactivated_blocking"`
	FiremansServiceEnabled              bool                   `json:"firemans_service_enabled"`
	AutomaticDoorOperatorPulseDuration  string                 `json:"automatic_door_operator_pulse_duration,omitempty"`
	IntercomCameraTriggerPulseDuration  string                 `json:"intercom_camera_trigger_pulse_duration,omitempty"`
	LogLevel                            string                 `json:"log_level"`
	PinLength                           int                    `json:"pin_length"`
	RelayPulseDuration                  string                 `json:"relay_pulse_duration"`
	PinRejectBuzzerAfterAttempts        int                    `json:"pin_reject_buzzer_after_attempts"`
	BuzzerRelayPulseDuration            string                 `json:"buzzer_relay_pulse_duration"`
	MQTTEnabled                         bool                   `json:"mqtt_enabled"`
	MQTTBroker                          string                 `json:"mqtt_broker"`
	MQTTClientID                        string                 `json:"mqtt_client_id"`
	MQTTUsername                        string                 `json:"mqtt_username"`
	MQTTPassword                        string                 `json:"mqtt_password"`
	MQTTCommandTopic                    string                 `json:"mqtt_command_topic"`
	MQTTStatusTopic                     string                 `json:"mqtt_status_topic"`
	MQTTCommandToken                    string                 `json:"mqtt_command_token"`
	TechMenuHistoryMax                  int                    `json:"tech_menu_history_max"`
	KeypadInterDigitTimeout             string                 `json:"keypad_inter_digit_timeout"`
	KeypadSessionTimeout                string                 `json:"keypad_session_timeout"`
	PinEntryFeedbackDelay               string                 `json:"pin_entry_feedback_delay"`
	PinLockoutEnabled                   bool                   `json:"pin_lockout_enabled"`
	PinLockoutAfterAttempts             int                    `json:"pin_lockout_after_attempts"`
	PinLockoutDuration                  string                 `json:"pin_lockout_duration"`
	PinLockoutOverridePin               string                 `json:"pin_lockout_override_pin"`
	FallbackAccessPin                   string                 `json:"fallback_access_pin"`
	KeypadDoorbellEnabled               bool                   `json:"keypad_doorbell_enabled"`
	KeypadDoorbellCooldown              string                 `json:"keypad_doorbell_cooldown"`
	KeypadFunctionCodesEnabled          bool                   `json:"keypad_function_codes_enabled"`
	KeypadFnExtendedPulse               string                 `json:"keypad_fn_extended_pulse"`
	KeypadFnExtendedHoldExtra           string                 `json:"keypad_fn_extended_hold_extra"`
	KeypadFnLatchEnabled                bool                   `json:"keypad_fn_latch_enabled"`
	KeypadFnLatchMax                    string                 `json:"keypad_fn_latch_max"`
	KeypadFnDuressCode                  string                 `json:"keypad_fn_duress_code"`
	WebhookEventEnabled                 bool                   `json:"webhook_event_enabled"`
	WebhookEventURL                     string                 `json:"webhook_event_url"`
	WebhookEventTokenEnabled            bool                   `json:"webhook_event_token_enabled"`
	WebhookEventToken                   string                 `json:"webhook_event_token"`
	WebhookEventTypes                   map[string]bool        `json:"webhook_event_types,omitempty"`
	WebhookEventEndpoints               []WebhookEventEndpoint `json:"webhook_event_endpoints,omitempty"`
	WebhookHeartbeatEnabled             bool                   `json:"webhook_heartbeat_enabled"`
	WebhookHeartbeatURL                 string                 `json:"webhook_heartbeat_url"`
	WebhookHeartbeatTokenEnabled        bool                   `json:"webhook_heartbeat_token_enabled"`
	WebhookHeartbeatToken               string                 `json:"webhook_heartbeat_token"`
	WebhookHTTPTimeout                  string                 `json:"webhook_http_timeout"`
	WebhookMaxConcurrent                int                    `json:"webhook_max_concurrent"`
	WebhookCircuitBreakerEnabled        bool                   `json:"webhook_circuit_breaker_enabled"`
	WebhookCircuitFailureThreshold      int                    `json:"webhook_circuit_failure_threshold"`
	WebhookCircuitOpenDuration          string                 `json:"webhook_circuit_open_duration"`
	KeypadOperationMode                 string                 `json:"keypad_operation_mode"`
	KeypadEvdevPath                     string                 `json:"keypad_evdev_path"`
	KeypadExitEvdevPath                 string                 `json:"keypad_exit_evdev_path"`
	ScannerDevicePath                   string                 `json:"scanner_device_path"`
	ScannerEnabled                      bool                   `json:"scanner_enabled"`
	MaxDevicesPerUser                   int                    `json:"max_devices_per_user"`
	QRTimeWindowSeconds                 int                    `json:"qr_time_window_seconds"`
	StaticTestQRCode                    string                 `json:"static_test_qr_code"`
	StaticTestQRCodeEnabled             bool                   `json:"static_test_qr_code_enabled"`
	PairPeerRole                        string                 `json:"pair_peer_role"`
	MQTTPairPeerTopic                   string                 `json:"mqtt_pair_peer_topic"`
	PairPeerToken                       string                 `json:"pair_peer_token"`
	ElevatorFloorWaitTimeout            string                 `json:"elevator_floor_wait_timeout"`
	ElevatorWaitFloorCabSense           string                 `json:"elevator_wait_floor_cab_sense,omitempty"`
	ElevatorFloorInputPins              string                 `json:"elevator_floor_input_pins"`
	ElevatorPredefinedFloor             int                    `json:"elevator_predefined_floor"`
	ElevatorPredefinedFloors            string                 `json:"elevator_predefined_floors"`
	ElevatorDispatchPulseDuration       string                 `json:"elevator_dispatch_pulse_duration"`
	ElevatorFloorDispatchPulseDurations string                 `json:"elevator_floor_dispatch_pulse_durations"`
	ElevatorEnablePulseDuration         string                 `json:"elevator_enable_pulse_duration"`
	DualKeypadRejectExitWithoutEntry    bool                   `json:"dual_keypad_reject_exit_without_entry"`
	AccessControlDoorID                 string                 `json:"access_control_door_id,omitempty"`
	AccessControlElevatorID             string                 `json:"access_control_elevator_id,omitempty"`
	AccessScheduleEnforce               bool                   `json:"access_schedule_enforce"`
	AccessScheduleApplyToFallbackPin    bool                   `json:"access_schedule_apply_to_fallback_pin"`
	AccessExceptionSiteTimezone         string                 `json:"access_exception_site_timezone,omitempty"`
	LightingTimeout                     string                 `json:"lighting_timeout"`
	LCDDisplay                          virtualkeyz2LCDPersist `json:"lcd_display,omitempty"`
}

// virtualkeyz2LCDPersist is written under device.lcd_display by cfg save.
type virtualkeyz2LCDPersist struct {
	Enabled                 bool `json:"enabled"`
	I2CBus                  int  `json:"i2c_bus"`
	I2CAddress              int  `json:"i2c_address"`
	BacklightTimeoutSeconds int  `json:"backlight_timeout_seconds"`
	I2CDebugEnabled         bool `json:"i2c_debug_enabled"`
}

type virtualkeyz2PersistGPIO struct {
	RelayOutputMode                     string `json:"relay_output_mode"`
	MCP23017I2CBus                      int    `json:"mcp23017_i2c_bus"`
	MCP23017I2CAddr                     int    `json:"mcp23017_i2c_addr"`
	XL9535I2CBus                        int    `json:"xl9535_i2c_bus"`
	XL9535I2CAddr                       int    `json:"xl9535_i2c_addr"`
	I2CBusRecoverySCLBCM                int    `json:"i2c_bus_recovery_scl_bcm"`
	DoorRelayPin                        int    `json:"door_relay_pin"`
	DoorRelayActiveLow                  bool   `json:"door_relay_active_low"`
	BuzzerRelayPin                      int    `json:"buzzer_relay_pin"`
	BuzzerRelayActiveLow                bool   `json:"buzzer_relay_active_low"`
	DoorSensorPin                       int    `json:"door_sensor_pin"`
	HeartbeatLEDPin                     int    `json:"heartbeat_led_pin"`
	ExitButtonPin                       int    `json:"exit_button_pin"`
	ExitButtonActiveLow                 bool   `json:"exit_button_active_low"`
	EntryButtonPin                      int    `json:"entry_button_pin"`
	EntryButtonActiveLow                bool   `json:"entry_button_active_low"`
	ElevatorDispatchRelayPin            int    `json:"elevator_dispatch_relay_pin"`
	ElevatorDispatchActiveLow           bool   `json:"elevator_dispatch_active_low"`
	ElevatorEnableRelayPin              int    `json:"elevator_enable_relay_pin"`
	ElevatorEnableActiveLow             bool   `json:"elevator_enable_active_low"`
	ElevatorFloorDispatchPins           string `json:"elevator_floor_dispatch_pins"`
	ElevatorPredefinedEnablePins        string `json:"elevator_predefined_enable_pins"`
	ElevatorWaitFloorEnablePins         string `json:"elevator_wait_floor_enable_pins"`
	LightingButtonPin                   int    `json:"lighting_button_pin"`
	LightingButtonActiveLow             bool   `json:"lighting_button_active_low"`
	LightingRelayPin                    int    `json:"lighting_relay_pin"`
	LightingRelayActiveLow              bool   `json:"lighting_relay_active_low"`
	FiremansServiceInputPin             int    `json:"firemans_service_input_pin"`
	FiremansServiceActiveLow            bool   `json:"firemans_service_active_low"`
	AutomaticDoorOperatorRelayPin       int    `json:"automatic_door_operator_relay_pin"`
	AutomaticDoorOperatorRelayActiveLow bool   `json:"automatic_door_operator_relay_active_low"`
	IntercomCameraTriggerRelayPin       int    `json:"intercom_camera_trigger_relay_pin"`
	IntercomCameraTriggerRelayActiveLow bool   `json:"intercom_camera_trigger_relay_active_low"`
	FireAlarmInterfacePin               int    `json:"fire_alarm_interface_pin"`
	FireAlarmInterfaceActiveLow         bool   `json:"fire_alarm_interface_active_low"`
	TamperSwitchPin                     int    `json:"tamper_switch_pin"`
	TamperSwitchActiveLow               bool   `json:"tamper_switch_active_low"`
	MotionSensorPin                     int    `json:"motion_sensor_pin"`
	MotionSensorActiveLow               bool   `json:"motion_sensor_active_low"`
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
	out.Device.DoorOpenAlarmInterval = c.DoorOpenAlarmInterval.String()
	out.Device.DoorOpenAlarmMaxCount = c.DoorOpenAlarmMaxCount
	out.Device.DoorForcedAfterWarnings = c.DoorForcedAfterWarnings
	out.Device.DoorSensorClosedIsLow = c.DoorSensorClosedIsLow
	out.Device.SoundCardName = c.SoundCardName
	out.Device.SoundStartup = c.SoundStartup
	out.Device.SoundShutdown = c.SoundShutdown
	out.Device.SoundPinOK = c.SoundPinOK
	out.Device.SoundAccessGranted = c.SoundAccessGranted
	out.Device.SoundPinReject = c.SoundPinReject
	out.Device.SoundKeypress = c.SoundKeypress
	out.Device.SoundLightingTimerSet = c.SoundLightingTimerSet
	out.Device.SoundLightingTimerExpired = c.SoundLightingTimerExpired
	out.Device.SoundDoorOpen = c.SoundDoorOpen
	out.Device.SoundDoorbell = c.SoundDoorbell
	out.Device.SoundCancel = c.SoundCancel
	out.Device.SoundStartupEnabled = c.SoundStartupEnabled
	out.Device.SoundShutdownEnabled = c.SoundShutdownEnabled
	out.Device.SoundPinOKEnabled = c.SoundPinOKEnabled
	out.Device.SoundAccessGrantedEnabled = c.SoundAccessGrantedEnabled
	out.Device.SoundPinRejectEnabled = c.SoundPinRejectEnabled
	out.Device.SoundKeypressEnabled = c.SoundKeypressEnabled
	out.Device.SoundLightingTimerSetEnabled = c.SoundLightingTimerSetEnabled
	out.Device.SoundLightingTimerExpiredEnabled = c.SoundLightingTimerExpiredEnabled
	out.Device.SoundDoorOpenEnabled = c.SoundDoorOpenEnabled
	out.Device.SoundDoorbellEnabled = c.SoundDoorbellEnabled
	out.Device.SoundCancelEnabled = c.SoundCancelEnabled
	out.Device.SoundStartupBlocking = c.SoundStartupBlocking
	out.Device.SoundShutdownBlocking = c.SoundShutdownBlocking
	out.Device.SoundPinOKBlocking = c.SoundPinOKBlocking
	out.Device.SoundAccessGrantedBlocking = c.SoundAccessGrantedBlocking
	out.Device.SoundPinRejectBlocking = c.SoundPinRejectBlocking
	out.Device.SoundKeypressBlocking = c.SoundKeypressBlocking
	out.Device.SoundLightingTimerSetBlocking = c.SoundLightingTimerSetBlocking
	out.Device.SoundLightingTimerExpiredBlocking = c.SoundLightingTimerExpiredBlocking
	out.Device.SoundDoorOpenBlocking = c.SoundDoorOpenBlocking
	out.Device.SoundDoorbellBlocking = c.SoundDoorbellBlocking
	out.Device.SoundCancelBlocking = c.SoundCancelBlocking
	out.Device.SoundFiremansActivated = c.SoundFiremansActivated
	out.Device.SoundFiremansDeactivated = c.SoundFiremansDeactivated
	out.Device.SoundFiremansActivatedEnabled = c.SoundFiremansActivatedEnabled
	out.Device.SoundFiremansDeactivatedEnabled = c.SoundFiremansDeactivatedEnabled
	out.Device.SoundFiremansActivatedBlocking = c.SoundFiremansActivatedBlocking
	out.Device.SoundFiremansDeactivatedBlocking = c.SoundFiremansDeactivatedBlocking
	out.Device.FiremansServiceEnabled = c.FiremansServiceEnabled
	if c.AutomaticDoorOperatorPulseDuration > 0 {
		out.Device.AutomaticDoorOperatorPulseDuration = c.AutomaticDoorOperatorPulseDuration.String()
	}
	if c.IntercomCameraTriggerPulseDuration > 0 {
		out.Device.IntercomCameraTriggerPulseDuration = c.IntercomCameraTriggerPulseDuration.String()
	}
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
	out.Device.KeypadDoorbellEnabled = c.KeypadDoorbellEnabled
	out.Device.KeypadDoorbellCooldown = c.KeypadDoorbellCooldown.String()
	out.Device.KeypadFunctionCodesEnabled = c.KeypadFunctionCodesEnabled
	out.Device.KeypadFnExtendedPulse = c.KeypadFnExtendedPulse.String()
	out.Device.KeypadFnExtendedHoldExtra = c.KeypadFnExtendedHoldExtra.String()
	out.Device.KeypadFnLatchEnabled = c.KeypadFnLatchEnabled
	out.Device.KeypadFnLatchMax = c.KeypadFnLatchMax.String()
	out.Device.KeypadFnDuressCode = c.KeypadFnDuressCode
	out.Device.WebhookEventEnabled = c.WebhookEventEnabled
	out.Device.WebhookEventURL = c.WebhookEventURL
	out.Device.WebhookEventTokenEnabled = c.WebhookEventTokenEnabled
	out.Device.WebhookEventToken = c.WebhookEventToken
	out.Device.WebhookEventTypes = cloneStringBoolMap(c.WebhookEventTypes)
	out.Device.WebhookEventEndpoints = cloneWebhookEventEndpoints(c.WebhookEventEndpoints)
	out.Device.WebhookHeartbeatEnabled = c.WebhookHeartbeatEnabled
	out.Device.WebhookHeartbeatURL = c.WebhookHeartbeatURL
	out.Device.WebhookHeartbeatTokenEnabled = c.WebhookHeartbeatTokenEnabled
	out.Device.WebhookHeartbeatToken = c.WebhookHeartbeatToken
	out.Device.WebhookHTTPTimeout = c.WebhookHTTPTimeout.String()
	out.Device.WebhookMaxConcurrent = c.WebhookMaxConcurrent
	out.Device.WebhookCircuitBreakerEnabled = c.WebhookCircuitBreakerEnabled
	out.Device.WebhookCircuitFailureThreshold = c.WebhookCircuitFailureThreshold
	out.Device.WebhookCircuitOpenDuration = c.WebhookCircuitOpenDuration.String()
	out.Device.KeypadOperationMode = c.KeypadOperationMode
	out.Device.KeypadEvdevPath = c.KeypadEvdevPath
	out.Device.KeypadExitEvdevPath = c.KeypadExitEvdevPath
	out.Device.ScannerDevicePath = c.ScannerDevicePath
	out.Device.ScannerEnabled = c.ScannerEnabled
	out.Device.MaxDevicesPerUser = c.MaxDevicesPerUser
	out.Device.QRTimeWindowSeconds = c.QRTimeWindowSeconds
	out.Device.StaticTestQRCode = c.StaticTestQRCode
	out.Device.StaticTestQRCodeEnabled = c.StaticTestQRCodeEnabled
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
	out.Device.AccessExceptionSiteTimezone = c.AccessExceptionSiteTimezone
	out.Device.LightingTimeout = c.LightingTimeout.String()
	blSec := int(c.LCDDisplay.BacklightTimeout / time.Second)
	if c.LCDDisplay.BacklightTimeout > 0 && blSec < 1 {
		blSec = 1
	}
	out.Device.LCDDisplay = virtualkeyz2LCDPersist{
		Enabled:                 c.LCDDisplay.Enabled,
		I2CBus:                  c.LCDDisplay.I2CBus,
		I2CAddress:              int(c.LCDDisplay.I2CAddr),
		BacklightTimeoutSeconds: blSec,
		I2CDebugEnabled:         c.LCDDisplay.I2CDebugEnabled,
	}
	out.GPIO.RelayOutputMode = normalizeRelayOutputMode(g.RelayOutputMode)
	out.GPIO.MCP23017I2CBus = g.MCP23017I2CBus
	out.GPIO.MCP23017I2CAddr = int(g.MCP23017I2CAddr)
	out.GPIO.XL9535I2CBus = g.XL9535I2CBus
	out.GPIO.XL9535I2CAddr = int(g.XL9535I2CAddr)
	out.GPIO.I2CBusRecoverySCLBCM = int(g.I2CBusRecoverySCLBCM)
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
	out.GPIO.LightingButtonPin = int(g.LightingButtonPin)
	out.GPIO.LightingButtonActiveLow = g.LightingButtonActiveLow
	out.GPIO.LightingRelayPin = int(g.LightingRelayPin)
	out.GPIO.LightingRelayActiveLow = g.LightingRelayActiveLow
	out.GPIO.FiremansServiceInputPin = int(g.FiremansServiceInputPin)
	out.GPIO.FiremansServiceActiveLow = g.FiremansServiceActiveLow
	out.GPIO.AutomaticDoorOperatorRelayPin = int(g.AutomaticDoorOperatorRelayPin)
	out.GPIO.AutomaticDoorOperatorRelayActiveLow = g.AutomaticDoorOperatorRelayActiveLow
	out.GPIO.IntercomCameraTriggerRelayPin = int(g.IntercomCameraTriggerRelayPin)
	out.GPIO.IntercomCameraTriggerRelayActiveLow = g.IntercomCameraTriggerRelayActiveLow
	out.GPIO.FireAlarmInterfacePin = int(g.FireAlarmInterfacePin)
	out.GPIO.FireAlarmInterfaceActiveLow = g.FireAlarmInterfaceActiveLow
	out.GPIO.TamperSwitchPin = int(g.TamperSwitchPin)
	out.GPIO.TamperSwitchActiveLow = g.TamperSwitchActiveLow
	out.GPIO.MotionSensorPin = int(g.MotionSensorPin)
	out.GPIO.MotionSensorActiveLow = g.MotionSensorActiveLow
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
