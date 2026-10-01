package app

import (
	"fmt"
	"io"
	"log"
	"strings"
)

func techMenuHandleFiremans(ctx *AppContext, parts []string) {
	if len(parts) < 2 {
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "Fireman's service (emergency bypass):")
			fmt.Fprintln(w, "  firemans on|off|status")
			fmt.Fprintln(w, "Requires device.firemans_service_enabled true (JSON or cfg set). GPIO optional: gpio.firemans_service_input_pin.")
		})
		return
	}
	sub := strings.ToLower(strings.TrimSpace(parts[1]))
	ctx.configMu.RLock()
	en := ctx.Config.FiremansServiceEnabled
	ctx.configMu.RUnlock()
	switch sub {
	case "on", "1", "true", "activate":
		if !en {
			techMenuSyncPrint(func(w io.Writer) {
				fmt.Fprintln(w, "firemans_service_enabled is false — enable in JSON or: cfg set firemans_service_enabled true")
			})
			log.Println("WARNING: Technician menu: firemans on ignored (feature disabled in configuration).")
			return
		}
		ctx.applyFiremansServiceTransition(true, "tech_menu")
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Fireman's service set ON (see logs / DEBUG for relay actions).") })
		log.Println("INFO: Technician menu: fireman's service ON.")
	case "off", "0", "false", "deactivate":
		ctx.applyFiremansServiceTransition(false, "tech_menu")
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Fireman's service set OFF.") })
		log.Println("INFO: Technician menu: fireman's service OFF.")
	case "status", "stat", "?":
		active := ctx.FiremansServiceActive()
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "firemans_service_enabled=%v  runtime_active=%v\n", en, active)
		})
		log.Printf("INFO: Technician menu: fireman's service status enabled=%v active=%v", en, active)
	default:
		techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "Unknown firemans subcommand %q (use on|off|status).\n", parts[1]) })
	}
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
	case "restart", "reboot":
		log.Println("INFO: Technician menu: cfg restart — replacing process (same binary and arguments; GPIO re-inits on next run).")
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintln(w, "Restarting: this process will be replaced by exec (no graceful HTTP shutdown).")
		})
		disableTechBottomTerminalLayout()
		terminalHardReset()
		if err := restartCurrentProgram(); err != nil {
			log.Printf("CRITICAL: cfg restart: %v", err)
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintf(w, "cfg restart failed: %v\n", err) })
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
	set, ok := techMenuCfgSetters[key]
	if !ok {
		return fmt.Errorf("unknown key %q (try: cfg keys)", key)
	}
	err = set(ctx, key, value)
	if err == nil && key == "tech_menu_history_max" {
		trimHistoryAfter = true
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
  door_open_warning_after           duration string (base before first door_open_timeout)
  door_open_alarm_interval          repeat interval for door_open_timeout after the first (default 30s)
  door_open_alarm_max_count         max door_open_timeout per open period (0=unlimited)
  door_forced_after_warnings        emit door_forced after N timeouts in one open period (0=never)
  door_sensor_closed_is_low         true|false
  relay_pulse_duration              e.g. 400ms
  buzzer_relay_pulse_duration       e.g. 800ms
  automatic_door_operator_pulse_duration  optional; empty/0 = same as relay_pulse_duration for automatic_door_operator relay
  intercom_camera_trigger_pulse_duration  optional; default 800ms after normalize (device JSON)
  pin_length                        0 = Enter to submit
  pin_reject_buzzer_after_attempts  0 disables buzzer
  sound_card_name                   ALSA -D e.g. plughw:1,0
  sound_startup                     WAV path
  sound_shutdown                    WAV path
  sound_pin_ok                      WAV path
  sound_access_granted              WAV path (entry/exit GPIO button unlock)
  sound_door_open                   WAV path (first door_open_timeout + each repeat while open)
  sound_pin_reject                  WAV path
  sound_keypress                    WAV path
  sound_lighting_timer_set          WAV path (optional; lighting timer armed/reset, relay ON)
  sound_lighting_timer_expired      WAV path (optional; lighting auto-off fired, relay OFF)
  sound_startup_enabled             true|false (false = never play sound_startup)
  sound_shutdown_enabled            true|false
  sound_pin_ok_enabled              true|false
  sound_access_granted_enabled      true|false
  sound_pin_reject_enabled          true|false
  sound_keypress_enabled            true|false
  sound_lighting_timer_set_enabled  true|false
  sound_lighting_timer_expired_enabled true|false
  sound_door_open_enabled           true|false
  firemans_service_enabled          true|false — master enable for fireman's / emergency bypass (GPIO, MQTT, or menu)
  sound_firemans_activated          WAV when emergency bypass turns ON
  sound_firemans_deactivated        WAV when emergency bypass turns OFF
  sound_firemans_activated_enabled  true|false
  sound_firemans_deactivated_enabled true|false
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
  lighting_timeout                  lighting relay hold after manual button or accepted PIN (default 30m; each resets full duration; relay off only when timer expires)
  lcd_display_enabled               true|false enable I2C HD44780 20x4 (see device.lcd_display JSON block)
  lcd_i2c_bus                       Linux I2C bus number (default 1 → /dev/i2c-1)
  lcd_i2c_address                   decimal device address (default 39 = 0x27)
  lcd_backlight_timeout_seconds     0=always on after activity; else min 5s auto backlight off
  lcd_i2c_debug_enabled             true|false verbose I2C/LCD library logs (default false; not tied to log_level)
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
  webhook_http_timeout              per outbound webhook POST (5s–120s, default 25s)
  webhook_max_concurrent            max simultaneous outbound webhook requests (1–256, default 16)
  webhook_circuit_breaker_enabled   true|false trip breaker on repeated failures (default true)
  webhook_circuit_failure_threshold consecutive failures (network/timeout or HTTP 5xx) before open (1–100, default 5)
  webhook_circuit_open_duration     how long POSTs are rejected after open (1s–1h, default 60s)
  keypad_operation_mode             access_* modes | elevator_wait_floor_buttons (see elevator_wait_floor_cab_sense) | elevator_predefined_floor (one relay pulse simulates floor call; cab buttons not used)
  keypad_evdev_path                 e.g. /dev/input/event1
  keypad_exit_evdev_path            second keypad for access_dual_usb_keypad
  scanner_device_path               dedicated HID scanner evdev path; opened with EVIOCGRAB (exclusive)
  max_devices_per_user              max mobile UUIDs linked to one PIN (default 3)
  qr_time_window_seconds            max clock drift for QR datetime_stamp (default 30)
  static_test_qr_code               secret payload when static_test_qr_code_enabled is true
  static_test_qr_code_enabled       true = only that exact QR grants (no other checks); any other QR denied; false = normal UUID QR
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
  access_exception_site_timezone    IANA zone for exception-calendar civil dates (holidays / early close); empty = system local
  relay_output_mode                 gpio|mcp23017|xl9535 (relays on BCM vs I2C expander; sensors/LED stay BCM)
  mcp23017_i2c_bus                  MCP23017: Linux I2C bus (default 1)
  mcp23017_i2c_addr                 MCP23017: 7-bit address, default 32 (0x20)
  xl9535_i2c_bus                    XL9535: Linux I2C bus (default 1)
  xl9535_i2c_addr                   XL9535: 7-bit address, default 32 (0x20)
  i2c_bus_recovery_scl_bcm          optional BCM wired to I2C SCL for stuck-bus recovery (9 clock pulses after /dev/i2c-* close); 0=skip bit-bang
  exit_button_pin                   REX GPIO (access_entry_with_exit_button)
  exit_button_active_low            true|false
  entry_button_pin                  GPIO (access_exit_with_entry_button)
  entry_button_active_low           true|false
  lighting_button_pin               BCM manual lighting push button (0=disabled)
  lighting_button_active_low        true|false
  lighting_relay_pin                lighting controller relay (BCM or expander 0–15; 0=disabled)
  lighting_relay_active_low         true|false
  firemans_service_input_pin        BCM maintained fireman's / emergency input (0=disabled; use MQTT/menu only)
  firemans_service_active_low       true = emergency active when pin reads low (pull-up wiring)
  automatic_door_operator_relay_pin  optional; pulse with authorized access (door operator / gate opener); BCM or expander 0–15
  automatic_door_operator_relay_active_low  true|false
  intercom_camera_trigger_relay_pin  optional; short pulse on authorized access (intercom / camera trigger)
  intercom_camera_trigger_relay_active_low  true|false
  fire_alarm_interface_pin          BCM opto: when active, door relay held open (evacuation); 0=disabled
  fire_alarm_interface_active_low   true = alarm active when pin reads low (pull-up wiring)
  tamper_switch_pin                 BCM opto: enclosure tamper (both edges logged); 0=disabled
  tamper_switch_active_low          secure/normal state wiring (see DEBUG logs on change)
  motion_sensor_pin                 BCM opto: presence / PIR assert edge (DEBUG on detection); 0=disabled
  motion_sensor_active_low          true = assert on falling edge (pull-up, active low)
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
  cfg restart                       replace process via exec (same argv/env; re-inits GPIO — use after pin map changes)
  cfg history clear                 clear command history
`)
}
