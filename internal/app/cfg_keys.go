package app

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cfgSetter applies one `cfg set <key> <value>` to ctx (caller holds configMu for writing).
// Like the original per-key switch, a setter may leave a zero value in place when parsing fails.
type cfgSetter func(ctx *AppContext, key, value string) error

// techMenuCfgKeys lists every key accepted by `cfg set`, in Tab-completion order. It is the single
// source for techMenuCfgSetValue and techMenuCfgKeysForCompletion; add new device/gpio keys here.
var techMenuCfgKeys = []struct {
	name string
	set  cfgSetter
}{
	{"access_control_door_id", cfgStr(func(c *AppContext) *string { return &c.Config.AccessControlDoorID })},
	{"access_control_elevator_id", cfgStr(func(c *AppContext) *string { return &c.Config.AccessControlElevatorID })},
	{"access_exception_site_timezone", cfgStr(func(c *AppContext) *string { return &c.Config.AccessExceptionSiteTimezone })},
	{"access_schedule_apply_to_fallback_pin", cfgBool(func(c *AppContext) *bool { return &c.Config.AccessScheduleApplyToFallbackPin })},
	{"access_schedule_enforce", cfgBool(func(c *AppContext) *bool { return &c.Config.AccessScheduleEnforce })},
	{"automatic_door_operator_pulse_duration", cfgOptDur(func(c *AppContext) *time.Duration { return &c.Config.AutomaticDoorOperatorPulseDuration })},
	{"automatic_door_operator_relay_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.AutomaticDoorOperatorRelayActiveLow })},
	{"automatic_door_operator_relay_pin", cfgRelayPin(func(c *AppContext) *uint8 { return &c.GPIOSettings.AutomaticDoorOperatorRelayPin })},
	{"buzzer_relay_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.BuzzerRelayActiveLow })},
	{"buzzer_relay_pin", cfgRelayPin(func(c *AppContext) *uint8 { return &c.GPIOSettings.BuzzerRelayPin })},
	{"door_forced_after_warnings", cfgInt(func(c *AppContext) *int { return &c.Config.DoorForcedAfterWarnings })},
	{"door_open_alarm_interval", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.DoorOpenAlarmInterval })},
	{"door_open_alarm_max_count", cfgInt(func(c *AppContext) *int { return &c.Config.DoorOpenAlarmMaxCount })},
	{"door_open_warning_after", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.DoorOpenWarningAfter })},
	{"door_relay_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.DoorRelayActiveLow })},
	{"door_relay_pin", cfgRelayPin(func(c *AppContext) *uint8 { return &c.GPIOSettings.DoorRelayPin })},
	{"door_sensor_closed_is_low", cfgBool(func(c *AppContext) *bool { return &c.Config.DoorSensorClosedIsLow })},
	{"door_sensor_pin", cfgBCM(func(c *AppContext) *uint8 { return &c.GPIOSettings.DoorSensorPin })},
	{"dual_keypad_reject_exit_without_entry", cfgBool(func(c *AppContext) *bool { return &c.Config.DualKeypadRejectExitWithoutEntry })},
	{"elevator_dispatch_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.ElevatorDispatchActiveLow })},
	{"elevator_enable_relay_pin", cfgRelayPin(func(c *AppContext) *uint8 { return &c.GPIOSettings.ElevatorEnableRelayPin })},
	{"elevator_dispatch_pulse_duration", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.ElevatorDispatchPulseDuration })},
	{"elevator_dispatch_relay_pin", cfgRelayPin(func(c *AppContext) *uint8 { return &c.GPIOSettings.ElevatorDispatchRelayPin })},
	{"elevator_enable_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.ElevatorEnableActiveLow })},
	{"elevator_enable_pulse_duration", cfgOptDur(func(c *AppContext) *time.Duration { return &c.Config.ElevatorEnablePulseDuration })},
	{"elevator_floor_dispatch_pins", func(ctx *AppContext, _, value string) (err error) {
		ctx.GPIOSettings.ElevatorFloorDispatchPins = strings.TrimSpace(value)
		mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
		ctx.elevatorFloorDispatchPins, err = parseRelayPinUint8List("elevator_floor_dispatch_pins", ctx.GPIOSettings.ElevatorFloorDispatchPins, mode)
		return err
	}},
	{"elevator_floor_dispatch_pulse_durations", func(ctx *AppContext, _, value string) (err error) {
		ds, perr := parseCommaDurationList("device", "elevator_floor_dispatch_pulse_durations", value)
		if perr != nil {
			err = perr
		} else {
			ctx.Config.ElevatorFloorDispatchPulseDurations = ds
		}
		return err
	}},
	{"elevator_floor_input_pins", cfgStr(func(c *AppContext) *string { return &c.Config.ElevatorFloorInputPins })},
	{"elevator_floor_wait_timeout", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.ElevatorFloorWaitTimeout })},
	{"elevator_predefined_enable_pins", func(ctx *AppContext, _, value string) (err error) {
		ctx.GPIOSettings.ElevatorPredefinedEnablePins = strings.TrimSpace(value)
		mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
		ctx.elevatorPredefinedEnablePins, err = parseRelayPinUint8List("elevator_predefined_enable_pins", ctx.GPIOSettings.ElevatorPredefinedEnablePins, mode)
		return err
	}},
	{"elevator_predefined_floor", cfgInt(func(c *AppContext) *int { return &c.Config.ElevatorPredefinedFloor })},
	{"elevator_predefined_floors", func(ctx *AppContext, _, value string) (err error) {
		fl, perr := parseCommaIntList("device", "elevator_predefined_floors", value)
		if perr != nil {
			err = perr
		} else {
			ctx.Config.ElevatorPredefinedFloors = fl
		}
		return err
	}},
	{"elevator_wait_floor_cab_sense", func(ctx *AppContext, _, value string) (err error) {
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
		return err
	}},
	{"elevator_wait_floor_enable_pins", func(ctx *AppContext, _, value string) (err error) {
		ctx.GPIOSettings.ElevatorWaitFloorEnablePins = strings.TrimSpace(value)
		mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
		ctx.elevatorWaitFloorEnablePins, err = parseRelayPinUint8List("elevator_wait_floor_enable_pins", ctx.GPIOSettings.ElevatorWaitFloorEnablePins, mode)
		return err
	}},
	{"entry_button_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.EntryButtonActiveLow })},
	{"entry_button_pin", cfgBCM(func(c *AppContext) *uint8 { return &c.GPIOSettings.EntryButtonPin })},
	{"exit_button_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.ExitButtonActiveLow })},
	{"exit_button_pin", cfgBCM(func(c *AppContext) *uint8 { return &c.GPIOSettings.ExitButtonPin })},
	{"fallback_access_pin", cfgStr(func(c *AppContext) *string { return &c.Config.FallbackAccessPin })},
	{"fire_alarm_interface_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.FireAlarmInterfaceActiveLow })},
	{"fire_alarm_interface_pin", cfgBCM(func(c *AppContext) *uint8 { return &c.GPIOSettings.FireAlarmInterfacePin })},
	{"firemans_service_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.FiremansServiceActiveLow })},
	{"firemans_service_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.FiremansServiceEnabled })},
	{"firemans_service_input_pin", cfgBCM(func(c *AppContext) *uint8 { return &c.GPIOSettings.FiremansServiceInputPin })},
	{"heartbeat_interval", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.HeartbeatInterval })},
	{"heartbeat_led_pin", cfgBCM(func(c *AppContext) *uint8 { return &c.GPIOSettings.HeartbeatLEDPin })},
	{"i2c_bus_recovery_scl_bcm", func(ctx *AppContext, _, value string) (err error) {
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			var u uint8
			u, err = bcmUint8("i2c_bus_recovery_scl_bcm", int(n))
			if err == nil {
				ctx.GPIOSettings.I2CBusRecoverySCLBCM = u
			}
		}
		return err
	}},
	{"intercom_camera_trigger_pulse_duration", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.IntercomCameraTriggerPulseDuration })},
	{"buzzer_relay_pulse_duration", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.BuzzerRelayPulseDuration })},
	{"intercom_camera_trigger_relay_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.IntercomCameraTriggerRelayActiveLow })},
	{"intercom_camera_trigger_relay_pin", cfgRelayPin(func(c *AppContext) *uint8 { return &c.GPIOSettings.IntercomCameraTriggerRelayPin })},
	{"keypad_doorbell_cooldown", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.KeypadDoorbellCooldown })},
	{"keypad_doorbell_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.KeypadDoorbellEnabled })},
	{"keypad_evdev_path", cfgStr(func(c *AppContext) *string { return &c.Config.KeypadEvdevPath })},
	{"keypad_exit_evdev_path", cfgStr(func(c *AppContext) *string { return &c.Config.KeypadExitEvdevPath })},
	{"scanner_device_path", cfgStr(func(c *AppContext) *string { return &c.Config.ScannerDevicePath })},
	{"scanner_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.ScannerEnabled })},
	{"max_devices_per_user", cfgInt(func(c *AppContext) *int { return &c.Config.MaxDevicesPerUser })},
	{"qr_time_window_seconds", cfgInt(func(c *AppContext) *int { return &c.Config.QRTimeWindowSeconds })},
	{"static_test_qr_code", cfgStr(func(c *AppContext) *string { return &c.Config.StaticTestQRCode })},
	{"static_test_qr_code_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.StaticTestQRCodeEnabled })},
	{"keypad_fn_duress_code", cfgStr(func(c *AppContext) *string { return &c.Config.KeypadFnDuressCode })},
	{"keypad_fn_extended_hold_extra", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.KeypadFnExtendedHoldExtra })},
	{"keypad_fn_extended_pulse", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.KeypadFnExtendedPulse })},
	{"keypad_fn_latch_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.KeypadFnLatchEnabled })},
	{"keypad_fn_latch_max", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.KeypadFnLatchMax })},
	{"keypad_function_codes_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.KeypadFunctionCodesEnabled })},
	{"keypad_inter_digit_timeout", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.KeypadInterDigitTimeout })},
	{"keypad_operation_mode", cfgStr(func(c *AppContext) *string { return &c.Config.KeypadOperationMode })},
	{"keypad_session_timeout", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.KeypadSessionTimeout })},
	{"lcd_backlight_timeout_seconds", func(ctx *AppContext, _, value string) (err error) {
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.Config.LCDDisplay.BacklightTimeout = time.Duration(n) * time.Second
			normalizeLCDDisplaySettings(&ctx.Config.LCDDisplay)
		}
		return err
	}},
	{"lcd_display_enabled", func(ctx *AppContext, _, value string) (err error) {
		ctx.Config.LCDDisplay.Enabled, err = strconv.ParseBool(value)
		if err == nil {
			normalizeLCDDisplaySettings(&ctx.Config.LCDDisplay)
		}
		return err
	}},
	{"lcd_i2c_address", func(ctx *AppContext, _, value string) (err error) {
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			if n < 0 || n > 255 {
				err = fmt.Errorf("lcd_i2c_address %d out of range 0-255", n)
			} else {
				ctx.Config.LCDDisplay.I2CAddr = uint8(n)
				normalizeLCDDisplaySettings(&ctx.Config.LCDDisplay)
			}
		}
		return err
	}},
	{"lcd_i2c_bus", func(ctx *AppContext, _, value string) (err error) {
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.Config.LCDDisplay.I2CBus = int(n)
			normalizeLCDDisplaySettings(&ctx.Config.LCDDisplay)
		}
		return err
	}},
	{"lcd_i2c_debug_enabled", func(ctx *AppContext, _, value string) (err error) {
		ctx.Config.LCDDisplay.I2CDebugEnabled, err = strconv.ParseBool(value)
		if err == nil {
			syncLCDTransportLibraryDebug(ctx.Config.LCDDisplay.I2CDebugEnabled)
		}
		return err
	}},
	{"lighting_button_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.LightingButtonActiveLow })},
	{"lighting_button_pin", cfgBCM(func(c *AppContext) *uint8 { return &c.GPIOSettings.LightingButtonPin })},
	{"lighting_relay_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.LightingRelayActiveLow })},
	{"lighting_relay_pin", cfgRelayPin(func(c *AppContext) *uint8 { return &c.GPIOSettings.LightingRelayPin })},
	{"lighting_timeout", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.LightingTimeout })},
	{"log_level", cfgStr(func(c *AppContext) *string { return &c.Config.LogLevel })},
	{"mcp23017_i2c_addr", func(ctx *AppContext, _, value string) (err error) {
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
		return err
	}},
	{"mcp23017_i2c_bus", func(ctx *AppContext, _, value string) (err error) {
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.GPIOSettings.MCP23017I2CBus = int(n)
			normalizeGPIORelaySettings(&ctx.GPIOSettings)
		}
		return err
	}},
	{"mqtt_broker", cfgStr(func(c *AppContext) *string { return &c.Config.MQTTBroker })},
	{"mqtt_client_id", cfgStr(func(c *AppContext) *string { return &c.Config.MQTTClientID })},
	{"mqtt_command_token", cfgStr(func(c *AppContext) *string { return &c.Config.MQTTCommandToken })},
	{"mqtt_command_topic", cfgStr(func(c *AppContext) *string { return &c.Config.MQTTCommandTopic })},
	{"mqtt_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.MQTTEnabled })},
	{"mqtt_pair_peer_topic", cfgStr(func(c *AppContext) *string { return &c.Config.MQTTPairPeerTopic })},
	{"mqtt_password", cfgStr(func(c *AppContext) *string { return &c.Config.MQTTPassword })},
	{"mqtt_status_topic", cfgStr(func(c *AppContext) *string { return &c.Config.MQTTStatusTopic })},
	{"mqtt_username", cfgStr(func(c *AppContext) *string { return &c.Config.MQTTUsername })},
	{"motion_sensor_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.MotionSensorActiveLow })},
	{"motion_sensor_pin", cfgBCM(func(c *AppContext) *uint8 { return &c.GPIOSettings.MotionSensorPin })},
	{"pair_peer_role", cfgStr(func(c *AppContext) *string { return &c.Config.PairPeerRole })},
	{"pair_peer_token", cfgStr(func(c *AppContext) *string { return &c.Config.PairPeerToken })},
	{"pin_entry_feedback_delay", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.PinEntryFeedbackDelay })},
	{"pin_length", cfgInt(func(c *AppContext) *int { return &c.Config.PinLength })},
	{"pin_lockout_after_attempts", cfgInt(func(c *AppContext) *int { return &c.Config.PinLockoutAfterAttempts })},
	{"pin_lockout_duration", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.PinLockoutDuration })},
	{"pin_lockout_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.PinLockoutEnabled })},
	{"pin_lockout_override_pin", cfgStr(func(c *AppContext) *string { return &c.Config.PinLockoutOverridePin })},
	{"pin_reject_buzzer_after_attempts", cfgInt(func(c *AppContext) *int { return &c.Config.PinRejectBuzzerAfterAttempts })},
	{"relay_output_mode", func(ctx *AppContext, _, value string) (err error) {
		ctx.GPIOSettings.RelayOutputMode = value
		normalizeGPIORelaySettings(&ctx.GPIOSettings)
		return err
	}},
	{"relay_pulse_duration", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.RelayPulseDuration })},
	{"sound_access_granted", cfgStr(func(c *AppContext) *string { return &c.Config.SoundAccessGranted })},
	{"sound_access_granted_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundAccessGrantedBlocking })},
	{"sound_access_granted_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundAccessGrantedEnabled })},
	{"sound_cancel", cfgStr(func(c *AppContext) *string { return &c.Config.SoundCancel })},
	{"sound_cancel_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundCancelBlocking })},
	{"sound_cancel_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundCancelEnabled })},
	{"sound_card_name", cfgStr(func(c *AppContext) *string { return &c.Config.SoundCardName })},
	{"sound_door_open", cfgStr(func(c *AppContext) *string { return &c.Config.SoundDoorOpen })},
	{"sound_door_open_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundDoorOpenBlocking })},
	{"sound_door_open_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundDoorOpenEnabled })},
	{"sound_doorbell", cfgStr(func(c *AppContext) *string { return &c.Config.SoundDoorbell })},
	{"sound_doorbell_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundDoorbellBlocking })},
	{"sound_doorbell_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundDoorbellEnabled })},
	{"sound_firemans_activated", cfgStr(func(c *AppContext) *string { return &c.Config.SoundFiremansActivated })},
	{"sound_firemans_activated_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundFiremansActivatedBlocking })},
	{"sound_firemans_activated_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundFiremansActivatedEnabled })},
	{"sound_firemans_deactivated", cfgStr(func(c *AppContext) *string { return &c.Config.SoundFiremansDeactivated })},
	{"sound_firemans_deactivated_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundFiremansDeactivatedBlocking })},
	{"sound_firemans_deactivated_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundFiremansDeactivatedEnabled })},
	{"sound_keypress", cfgStr(func(c *AppContext) *string { return &c.Config.SoundKeypress })},
	{"sound_keypress_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundKeypressBlocking })},
	{"sound_keypress_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundKeypressEnabled })},
	{"sound_lighting_timer_expired", cfgStr(func(c *AppContext) *string { return &c.Config.SoundLightingTimerExpired })},
	{"sound_lighting_timer_expired_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundLightingTimerExpiredBlocking })},
	{"sound_lighting_timer_expired_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundLightingTimerExpiredEnabled })},
	{"sound_lighting_timer_set", cfgStr(func(c *AppContext) *string { return &c.Config.SoundLightingTimerSet })},
	{"sound_lighting_timer_set_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundLightingTimerSetBlocking })},
	{"sound_lighting_timer_set_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundLightingTimerSetEnabled })},
	{"sound_pin_ok", cfgStr(func(c *AppContext) *string { return &c.Config.SoundPinOK })},
	{"sound_pin_ok_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundPinOKBlocking })},
	{"sound_pin_ok_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundPinOKEnabled })},
	{"sound_pin_reject", cfgStr(func(c *AppContext) *string { return &c.Config.SoundPinReject })},
	{"sound_pin_reject_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundPinRejectBlocking })},
	{"sound_pin_reject_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundPinRejectEnabled })},
	{"sound_shutdown", cfgStr(func(c *AppContext) *string { return &c.Config.SoundShutdown })},
	{"sound_shutdown_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundShutdownBlocking })},
	{"sound_shutdown_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundShutdownEnabled })},
	{"sound_startup", cfgStr(func(c *AppContext) *string { return &c.Config.SoundStartup })},
	{"sound_startup_blocking", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundStartupBlocking })},
	{"sound_startup_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.SoundStartupEnabled })},
	{"tamper_switch_active_low", cfgBool(func(c *AppContext) *bool { return &c.GPIOSettings.TamperSwitchActiveLow })},
	{"tamper_switch_pin", cfgBCM(func(c *AppContext) *uint8 { return &c.GPIOSettings.TamperSwitchPin })},
	{"tech_menu_history_max", func(ctx *AppContext, _, value string) (err error) {
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.Config.TechMenuHistoryMax = int(n)
		}
		return err
	}},
	{"tech_menu_prompt", func(ctx *AppContext, _, value string) (err error) {
		ctx.TechMenuPrompt = value
		return err
	}},
	{"webhook_circuit_breaker_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.WebhookCircuitBreakerEnabled })},
	{"webhook_circuit_failure_threshold", cfgInt(func(c *AppContext) *int { return &c.Config.WebhookCircuitFailureThreshold })},
	{"webhook_circuit_open_duration", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.WebhookCircuitOpenDuration })},
	{"webhook_event_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.WebhookEventEnabled })},
	{"webhook_event_token", cfgStr(func(c *AppContext) *string { return &c.Config.WebhookEventToken })},
	{"webhook_event_token_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.WebhookEventTokenEnabled })},
	{"webhook_event_url", cfgStr(func(c *AppContext) *string { return &c.Config.WebhookEventURL })},
	{"webhook_heartbeat_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.WebhookHeartbeatEnabled })},
	{"webhook_heartbeat_token", cfgStr(func(c *AppContext) *string { return &c.Config.WebhookHeartbeatToken })},
	{"webhook_heartbeat_token_enabled", cfgBool(func(c *AppContext) *bool { return &c.Config.WebhookHeartbeatTokenEnabled })},
	{"webhook_heartbeat_url", cfgStr(func(c *AppContext) *string { return &c.Config.WebhookHeartbeatURL })},
	{"webhook_http_timeout", cfgDur(func(c *AppContext) *time.Duration { return &c.Config.WebhookHTTPTimeout })},
	{"webhook_max_concurrent", cfgInt(func(c *AppContext) *int { return &c.Config.WebhookMaxConcurrent })},
	{"xl9535_i2c_addr", func(ctx *AppContext, _, value string) (err error) {
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
		return err
	}},
	{"xl9535_i2c_bus", func(ctx *AppContext, _, value string) (err error) {
		var n int64
		n, err = strconv.ParseInt(value, 10, 32)
		if err == nil {
			ctx.GPIOSettings.XL9535I2CBus = int(n)
			normalizeGPIORelaySettings(&ctx.GPIOSettings)
		}
		return err
	}},
}

var (
	techMenuCfgSetters = func() map[string]cfgSetter {
		m := make(map[string]cfgSetter, len(techMenuCfgKeys))
		for _, k := range techMenuCfgKeys {
			if _, dup := m[k.name]; dup {
				panic("duplicate cfg key " + k.name)
			}
			m[k.name] = k.set
		}
		return m
	}()

	// techMenuCfgKeysForCompletion is the Tab-completion list for `cfg set <key>`.
	techMenuCfgKeysForCompletion = func() []string {
		out := make([]string, len(techMenuCfgKeys))
		for i, k := range techMenuCfgKeys {
			out[i] = k.name
		}
		return out
	}()
)

func cfgDur(field func(*AppContext) *time.Duration) cfgSetter {
	return func(ctx *AppContext, key, value string) error {
		return applyJSONDuration(field(ctx), "device", key, &value)
	}
}

// cfgOptDur is cfgDur where an empty value means 0 (feature falls back to its default).
func cfgOptDur(field func(*AppContext) *time.Duration) cfgSetter {
	return func(ctx *AppContext, key, value string) error {
		if strings.TrimSpace(value) == "" {
			*field(ctx) = 0
			return nil
		}
		return applyJSONDuration(field(ctx), "device", key, &value)
	}
}

func cfgBool(field func(*AppContext) *bool) cfgSetter {
	return func(ctx *AppContext, _, value string) (err error) {
		*field(ctx), err = strconv.ParseBool(value)
		return err
	}
}

func cfgStr(field func(*AppContext) *string) cfgSetter {
	return func(ctx *AppContext, _, value string) error {
		*field(ctx) = value
		return nil
	}
}

func cfgInt(field func(*AppContext) *int) cfgSetter {
	return func(ctx *AppContext, _, value string) error {
		n, err := strconv.ParseInt(value, 10, 32)
		if err == nil {
			*field(ctx) = int(n)
		}
		return err
	}
}

// cfgBCM sets a SoC GPIO (BCM) input/output pin.
func cfgBCM(field func(*AppContext) *uint8) cfgSetter {
	return func(ctx *AppContext, key, value string) error {
		n, err := strconv.ParseInt(value, 10, 32)
		if err == nil {
			*field(ctx), err = bcmUint8(key, int(n))
		}
		return err
	}
}

// cfgRelayPin sets a relay output pin, validated for the current relay_output_mode (BCM or expander).
func cfgRelayPin(field func(*AppContext) *uint8) cfgSetter {
	return func(ctx *AppContext, key, value string) error {
		n, err := strconv.ParseInt(value, 10, 32)
		if err == nil {
			mode := normalizeRelayOutputMode(ctx.GPIOSettings.RelayOutputMode)
			*field(ctx), err = relayPinUint8(key, int(n), mode)
		}
		return err
	}
}
