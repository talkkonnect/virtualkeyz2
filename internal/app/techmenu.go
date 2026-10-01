package app

import (
	"cmp"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"virtualkeyz2/internal/keypadlist"
)

// runTechnicianMenu reads from /dev/tty; menu text and logs use stdout. Bottom line is reserved for the configured prompt.
// shutdownNotify receives when the user enters "..." to exit the whole program (same shutdown path as SIGTERM).
func runTechnicianMenu(ctx *AppContext, shutdownNotify chan<- struct{}) {
	time.Sleep(800 * time.Millisecond)
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		releaseStartupLogBuffer(os.Stdout)
		debugf("Menu skipped (no /dev/tty: %v). Use -notechmenu to silence.", err)
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
  occ Dual USB keypad: show persisted area occupancy (masked PINs + labels)
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
  cfg restart   Exec same binary (GPIO re-init); use after reload when gpio.* changed
  firemans on|off|status   Fireman's service / emergency bypass (needs firemans_service_enabled)
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
			debugf("Technician menu stdin closed: %v", err)
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
		if len(parts) > 0 {
			switch strings.ToLower(parts[0]) {
			case "firemans", "fireman", "fs":
				techMenuHandleFiremans(ctx, parts)
				continue
			}
		}
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
			playSoundEnabled(cfg8, cfg8.SoundKeypress, cfg8.SoundKeypressEnabled, cfg8.SoundKeypressBlocking)
			techMenuSyncPrint(func(w io.Writer) { fmt.Fprintln(w, "Sound finished (key.wav).") })
		case "9":
			log.Println("INFO: Technician menu: playing PIN OK sound test.")
			ctx.configMu.RLock()
			cfg9 := ctx.Config
			ctx.configMu.RUnlock()
			playSoundEnabled(cfg9, cfg9.SoundPinOK, cfg9.SoundPinOKEnabled, cfg9.SoundPinOKBlocking)
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
		fmt.Fprintln(w, "\nchangelog.txt not found (place next to the binary, in cwd, or project root).")
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

	fmt.Fprintf(w, "\n--- Dual keypad occupancy (SQLite dual_keypad_zone_occupancy) ---\n")
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
	var rows []occRow
	total := 0
	if ctx.DB != nil {
		rq, err := ctx.DB.Query(`SELECT pin, inside_count FROM dual_keypad_zone_occupancy WHERE inside_count > 0 ORDER BY pin`)
		if err != nil {
			fmt.Fprintf(w, "  (occupancy query failed: %v)\n\n", err)
			return
		}
		for rq.Next() {
			var r occRow
			if err := rq.Scan(&r.pin, &r.n); err != nil {
				_ = rq.Close()
				fmt.Fprintf(w, "  (occupancy scan failed: %v)\n\n", err)
				return
			}
			rows = append(rows, r)
			total += r.n
		}
		_ = rq.Close()
	} else {
		ctx.occupancyCounters.Range(func(key, value any) bool {
			n := int(value.(*atomic.Int32).Load())
			if n <= 0 {
				return true
			}
			rows = append(rows, occRow{key.(string), n})
			total += n
			return true
		})
	}

	slices.SortFunc(rows, func(a, b occRow) int { return cmp.Compare(a.pin, b.pin) })

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
	fmt.Fprintf(w, "  door_open_alarm_interval: %s\n", c.DoorOpenAlarmInterval)
	fmt.Fprintf(w, "  door_open_alarm_max_count: %d (0=unlimited door_open_timeout per open period)\n", c.DoorOpenAlarmMaxCount)
	fmt.Fprintf(w, "  door_forced_after_warnings: %d (0=never)\n", c.DoorForcedAfterWarnings)
	fmt.Fprintf(w, "  door_sensor_closed_is_low: %v\n", c.DoorSensorClosedIsLow)
	fmt.Fprintf(w, "  relay_pulse_duration: %s\n", c.RelayPulseDuration)
	fmt.Fprintf(w, "  buzzer_relay_pulse_duration: %s\n", c.BuzzerRelayPulseDuration)
	if c.AutomaticDoorOperatorPulseDuration > 0 {
		fmt.Fprintf(w, "  automatic_door_operator_pulse_duration: %s (0 in file = use relay_pulse_duration)\n", c.AutomaticDoorOperatorPulseDuration)
	} else {
		fmt.Fprintln(w, "  automatic_door_operator_pulse_duration: (unset; uses relay_pulse_duration)")
	}
	fmt.Fprintf(w, "  intercom_camera_trigger_pulse_duration: %s\n", c.IntercomCameraTriggerPulseDuration)
	fmt.Fprintf(w, "  lighting_timeout: %s (manual button or accepted PIN; default 30m; full reset each trigger; relay off only at expiry)\n", c.LightingTimeout)
	lcd := c.LCDDisplay
	fmt.Fprintf(w, "  lcd_display: enabled=%v bus=%d addr=0x%02x backlight_idle_off=%s i2c_debug=%v\n",
		lcd.Enabled, lcd.I2CBus, lcd.I2CAddr, lcd.BacklightTimeout.String(), lcd.I2CDebugEnabled)
	fmt.Fprintf(w, "  pin_reject_buzzer_after_attempts: %d\n", c.PinRejectBuzzerAfterAttempts)
	fmt.Fprintf(w, "  keypad_inter_digit_timeout: %s\n", c.KeypadInterDigitTimeout)
	fmt.Fprintf(w, "  keypad_session_timeout: %s\n", c.KeypadSessionTimeout)
	fmt.Fprintf(w, "  pin_entry_feedback_delay: %s\n", c.PinEntryFeedbackDelay)
	fmt.Fprintf(w, "\n--- Operation mode ---\n")
	fmt.Fprintf(w, "  keypad_operation_mode: %s\n", NormalizeKeypadOperationMode(c.KeypadOperationMode))
	fmt.Fprintf(w, "  keypad_evdev_path: %q\n", c.KeypadEvdevPath)
	fmt.Fprintf(w, "  keypad_exit_evdev_path: %q\n", c.KeypadExitEvdevPath)
	fmt.Fprintf(w, "  scanner_device_path: %q\n", c.ScannerDevicePath)
	fmt.Fprintf(w, "  scanner_enabled: %v\n", c.ScannerEnabled)
	fmt.Fprintf(w, "  max_devices_per_user: %d\n", c.MaxDevicesPerUser)
	fmt.Fprintf(w, "  qr_time_window_seconds: %d\n", c.QRTimeWindowSeconds)
	if strings.TrimSpace(c.StaticTestQRCode) != "" {
		fmt.Fprintln(w, "  static_test_qr_code: (set)")
	} else {
		fmt.Fprintln(w, "  static_test_qr_code: \"\"")
	}
	fmt.Fprintf(w, "  static_test_qr_code_enabled: %v\n", c.StaticTestQRCodeEnabled)
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
	fmt.Fprintf(w, "  access_exception_site_timezone: %q\n", c.AccessExceptionSiteTimezone)
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
	fmt.Fprintf(w, "  sound_access_granted: %q\n", c.SoundAccessGranted)
	fmt.Fprintf(w, "  sound_door_open: %q\n", c.SoundDoorOpen)
	fmt.Fprintf(w, "  sound_pin_reject: %q\n", c.SoundPinReject)
	fmt.Fprintf(w, "  sound_keypress: %q\n", c.SoundKeypress)
	fmt.Fprintf(w, "  sound_lighting_timer_set: %q\n", c.SoundLightingTimerSet)
	fmt.Fprintf(w, "  sound_lighting_timer_expired: %q\n", c.SoundLightingTimerExpired)
	fmt.Fprintf(w, "  sound_startup_enabled: %v\n", c.SoundStartupEnabled)
	fmt.Fprintf(w, "  sound_shutdown_enabled: %v\n", c.SoundShutdownEnabled)
	fmt.Fprintf(w, "  sound_pin_ok_enabled: %v\n", c.SoundPinOKEnabled)
	fmt.Fprintf(w, "  sound_access_granted_enabled: %v\n", c.SoundAccessGrantedEnabled)
	fmt.Fprintf(w, "  sound_pin_reject_enabled: %v\n", c.SoundPinRejectEnabled)
	fmt.Fprintf(w, "  sound_keypress_enabled: %v\n", c.SoundKeypressEnabled)
	fmt.Fprintf(w, "  sound_lighting_timer_set_enabled: %v\n", c.SoundLightingTimerSetEnabled)
	fmt.Fprintf(w, "  sound_lighting_timer_expired_enabled: %v\n", c.SoundLightingTimerExpiredEnabled)
	fmt.Fprintf(w, "  sound_door_open_enabled: %v\n", c.SoundDoorOpenEnabled)
	fmt.Fprintf(w, "  sound_startup_blocking: %v\n", c.SoundStartupBlocking)
	fmt.Fprintf(w, "  sound_shutdown_blocking: %v\n", c.SoundShutdownBlocking)
	fmt.Fprintf(w, "  sound_pin_ok_blocking: %v\n", c.SoundPinOKBlocking)
	fmt.Fprintf(w, "  sound_access_granted_blocking: %v\n", c.SoundAccessGrantedBlocking)
	fmt.Fprintf(w, "  sound_pin_reject_blocking: %v\n", c.SoundPinRejectBlocking)
	fmt.Fprintf(w, "  sound_keypress_blocking: %v\n", c.SoundKeypressBlocking)
	fmt.Fprintf(w, "  sound_lighting_timer_set_blocking: %v\n", c.SoundLightingTimerSetBlocking)
	fmt.Fprintf(w, "  sound_lighting_timer_expired_blocking: %v\n", c.SoundLightingTimerExpiredBlocking)
	fmt.Fprintf(w, "  sound_door_open_blocking: %v\n", c.SoundDoorOpenBlocking)
	fmt.Fprintf(w, "  firemans_service_enabled: %v\n", c.FiremansServiceEnabled)
	fmt.Fprintf(w, "  sound_firemans_activated: %q\n", c.SoundFiremansActivated)
	fmt.Fprintf(w, "  sound_firemans_deactivated: %q\n", c.SoundFiremansDeactivated)
	fmt.Fprintf(w, "  sound_firemans_activated_enabled: %v\n", c.SoundFiremansActivatedEnabled)
	fmt.Fprintf(w, "  sound_firemans_deactivated_enabled: %v\n", c.SoundFiremansDeactivatedEnabled)
	fmt.Fprintf(w, "  sound_firemans_activated_blocking: %v\n", c.SoundFiremansActivatedBlocking)
	fmt.Fprintf(w, "  sound_firemans_deactivated_blocking: %v\n", c.SoundFiremansDeactivatedBlocking)
	fmt.Fprintf(w, "  firemans_service_runtime_active: %v\n", ctx.FiremansServiceActive())
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
	if len(c.WebhookEventTypes) > 0 {
		fmt.Fprintf(w, "  webhook_event_types: %v (non-empty = global allowlist; only true keys are sent)\n", c.WebhookEventTypes)
	} else {
		fmt.Fprintf(w, "  webhook_event_types: (unset = all event types)\n")
	}
	if n := len(c.WebhookEventEndpoints); n > 0 {
		fmt.Fprintf(w, "  webhook_event_endpoints: %d (when non-empty, replaces webhook_event_url for events; each may set enabled + event_types)\n", n)
		for i := range c.WebhookEventEndpoints {
			ep := c.WebhookEventEndpoints[i]
			u := strings.TrimSpace(ep.URL)
			if u == "" {
				u = "(no url)"
			}
			fmt.Fprintf(w, "    [%d] enabled=%v url=%q token_enabled=%v event_types=%v\n", i, ep.Enabled, u, ep.TokenEnabled, ep.EventTypes)
		}
	} else {
		fmt.Fprintf(w, "  webhook_event_endpoints: (unset = use webhook_event_url)\n")
	}
	fmt.Fprintf(w, "  webhook_heartbeat_enabled: %v\n", c.WebhookHeartbeatEnabled)
	fmt.Fprintf(w, "  webhook_heartbeat_url: %q\n", c.WebhookHeartbeatURL)
	fmt.Fprintf(w, "  webhook_heartbeat_token_enabled: %v\n", c.WebhookHeartbeatTokenEnabled)
	hbTok := `""`
	if strings.TrimSpace(c.WebhookHeartbeatToken) != "" {
		hbTok = "(set)"
	}
	fmt.Fprintf(w, "  webhook_heartbeat_token: %s\n", hbTok)
	fmt.Fprintf(w, "  webhook_http_timeout: %s\n", c.WebhookHTTPTimeout.String())
	fmt.Fprintf(w, "  webhook_max_concurrent: %d\n", c.WebhookMaxConcurrent)
	fmt.Fprintf(w, "  webhook_circuit_breaker_enabled: %v\n", c.WebhookCircuitBreakerEnabled)
	fmt.Fprintf(w, "  webhook_circuit_failure_threshold: %d\n", c.WebhookCircuitFailureThreshold)
	fmt.Fprintf(w, "  webhook_circuit_open_duration: %s\n", c.WebhookCircuitOpenDuration.String())
	fmt.Fprintf(w, "\n--- GPIO ---\n")
	fmt.Fprintf(w, "  relay_output_mode: %s\n", normalizeRelayOutputMode(g.RelayOutputMode))
	fmt.Fprintf(w, "  mcp23017_i2c_bus: %d\n", g.MCP23017I2CBus)
	fmt.Fprintf(w, "  mcp23017_i2c_addr: %d\n", int(g.MCP23017I2CAddr))
	fmt.Fprintf(w, "  xl9535_i2c_bus: %d\n", g.XL9535I2CBus)
	fmt.Fprintf(w, "  xl9535_i2c_addr: %d\n", int(g.XL9535I2CAddr))
	fmt.Fprintf(w, "  i2c_bus_recovery_scl_bcm: %d\n", g.I2CBusRecoverySCLBCM)
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
	fmt.Fprintf(w, "  lighting_button_pin: %d\n", g.LightingButtonPin)
	fmt.Fprintf(w, "  lighting_button_active_low: %v\n", g.LightingButtonActiveLow)
	fmt.Fprintf(w, "  lighting_relay_pin: %d\n", g.LightingRelayPin)
	fmt.Fprintf(w, "  lighting_relay_active_low: %v\n", g.LightingRelayActiveLow)
	fmt.Fprintf(w, "  firemans_service_input_pin: %d (0=not used)\n", g.FiremansServiceInputPin)
	fmt.Fprintf(w, "  firemans_service_active_low: %v\n", g.FiremansServiceActiveLow)
	fmt.Fprintf(w, "  automatic_door_operator_relay_pin: %d (0=disabled)\n", g.AutomaticDoorOperatorRelayPin)
	fmt.Fprintf(w, "  automatic_door_operator_relay_active_low: %v\n", g.AutomaticDoorOperatorRelayActiveLow)
	fmt.Fprintf(w, "  intercom_camera_trigger_relay_pin: %d (0=disabled)\n", g.IntercomCameraTriggerRelayPin)
	fmt.Fprintf(w, "  intercom_camera_trigger_relay_active_low: %v\n", g.IntercomCameraTriggerRelayActiveLow)
	fmt.Fprintf(w, "  fire_alarm_interface_pin: %d (0=disabled)\n", g.FireAlarmInterfacePin)
	fmt.Fprintf(w, "  fire_alarm_interface_active_low: %v\n", g.FireAlarmInterfaceActiveLow)
	fmt.Fprintf(w, "  fire_alarm_interface_runtime_active: %v\n", ctx.FireAlarmInterfaceActive())
	fmt.Fprintf(w, "  tamper_switch_pin: %d (0=disabled)\n", g.TamperSwitchPin)
	fmt.Fprintf(w, "  tamper_switch_active_low: %v\n", g.TamperSwitchActiveLow)
	fmt.Fprintf(w, "  motion_sensor_pin: %d (0=disabled)\n", g.MotionSensorPin)
	fmt.Fprintf(w, "  motion_sensor_active_low: %v\n", g.MotionSensorActiveLow)
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
	case "lighting":
		d = ctx.Config.RelayPulseDuration
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
	if name == "lighting" && !ctx.GPIO.HasOutput("lighting") {
		techMenuSyncPrint(func(w io.Writer) {
			fmt.Fprintf(w, "Cannot pulse %q: lighting_relay_pin is 0 (not configured).\n", name)
		})
		log.Printf("WARNING: Technician menu: pulse %s skipped (lighting relay not configured).", name)
		return
	}
	ctx.GPIO.ActionPulse(name, d)
	techMenuSyncPrint(func(w io.Writer) {
		fmt.Fprintf(w, "Pulsing %q for %s\n", name, d)
	})
	log.Printf("INFO: Technician menu: test pulse %q for %s", name, d)
}
