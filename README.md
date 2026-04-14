# VirtualKeyz2 — Operator Guide

Virtualkeyz2 is an opensource door and elevator PIN based access control using USB keypads and I2C expander boards for the hardware layer.

This document is for installers and operators who configure and run the service on a Raspberry Pi (or similar Linux host). It describes behaviour, configuration keys, SQLite access control, and practical wiring notes. Authoritative JSON field names match `virtualkeyz2.json`.

---

## 1. Process flags, configuration file, and applying changes

| Flag | Meaning |
|------|---------|
| `-config <path>` | JSON configuration file (default `virtualkeyz2.json` in the working directory). |
| `-daemon` | Declares daemon-style startup (logging only; no extra integration in-tree). |
| `-notechmenu` | Disable the interactive technician menu on `/dev/tty` when a TTY is present. |

**Applying changes:** edit JSON, then either **restart the process** or use the technician menu: `cfg reload` loads from disk; `cfg apply` / `cfg live` refreshes in-memory settings (MQTT, log level, durations, paths, and other keys handled by `cfg set`).  

**GPIO / relay mapping:** changing `gpio` pin numbers, `relay_output_mode`, or I2C settings **requires a process restart**; `cfg apply` does not re-initialise hardware.

**Database:** the service opens SQLite **`access_control.db`** in the current working directory (PINs and access-schedule tables). Ensure the service user can read/write this file.

---

## 2. Software build version

- On startup the log line includes the **build version** and **release timestamp** (UTC).
- From the technician menu: **`v`** shows version and release; **`ch`** prints `changelog.txt` if found (next to the binary, current working directory, or project root).
- Developers bump version and changelog with `./tools/bump-version.sh "description"`.

---

## 3. Technician menu (console / `/dev/tty`)

When the process has a TTY and `-notechmenu` is not used, a bottom-line prompt appears (`tech_menu_prompt`). Useful commands:

| Input | Action |
|-------|--------|
| `h` | Main menu help |
| `1` / `cfg list` | Full configuration (sensitive tokens shown as `(set)`) |
| `v` | Software build version and release |
| `ch` | Changelog text |
| `i` | Network snapshot (Ethernet / Wi‑Fi, DNS, gateways) |
| `p` | System-wide listening TCP/UDP ports |
| `occ` | Dual USB mode: in-memory area occupancy (masked PIN tails + `access_pins` labels) |
| `kb` / `kb all` | List **by-id/by-path** USE_PATH values (`kb` = USB only; `kb all` = include non-USB) |
| `cfg set <key> <value>` | Change one setting in memory |
| `cfg save` | Write current config to the `-config` JSON path |
| `cfg reload` | Load JSON from disk and apply |
| `cfg apply` / `cfg live` | Apply in-memory config (e.g. after `cfg set`) |
| `cfg keys` | All settable keys (same names as JSON); same text as inline help |
| `cfg history clear` | Clear command history |
| `...` | Shutdown request (same path as SIGTERM) |

---

## 4. Operation modes (`device.keypad_operation_mode`)

Set **exactly one** of the following string values.

### 4.1 `access_entry` (default)

- Single USB keypad on `device.keypad_evdev_path`.
- Valid PIN → pulse **door** relay (`gpio.door_relay_*`, duration `device.relay_pulse_duration`).

### 4.2 `access_exit`

- Same logic as **entry**; use when the keypad and relay are wired for an **exit** door or strike. Software behaviour matches `access_entry`; logs and webhooks record the mode.

### 4.3 `access_entry_with_exit_button`

- Valid PIN → door pulse.
- **Free egress:** `gpio.exit_button_pin` (non-zero) pulses the door; `gpio.exit_button_active_low` = contact pulls to ground when pressed.

### 4.4 `access_exit_with_entry_button`

- Keypad at **exit** side (valid PIN → door pulse).
- **`gpio.entry_button_pin`:** “request entry” input; **`gpio.entry_button_active_low`** same convention as exit.

### 4.5 `access_dual_usb_keypad`

- **`device.keypad_evdev_path`** — **entry** keypad; **`device.keypad_exit_evdev_path`** — **exit** keypad (must differ).
- **Credentials:** SQLite **`access_pins`** (`pin`, optional `label`, `enabled`). If no row matches, **`device.fallback_access_pin`** is accepted when non-empty.
- **Occupancy (RAM, until restart):** entry PIN increments that credential’s “inside” count; exit PIN decrements. Mismatch handling: **`device.dual_keypad_reject_exit_without_entry`** (`false` = warn, still open, webhook may include `occupancy_mismatch`; `true` = reject exit, no pulse).
- **Technician `occ`:** occupancy snapshot with masked PINs and labels.

### 4.6 `access_paired_remote_exit`

| Unit | `device.pair_peer_role` | Behaviour |
|------|-------------------------|-----------|
| **Entry** | `entry` | Valid PIN → pulse **local** door + publish JSON to **`device.mqtt_pair_peer_topic`**. |
| **Exit** | `exit` | Subscribes to **`device.mqtt_pair_peer_topic`**; on valid message, pulses **local** door. |

Requires **`device.mqtt_enabled`**, broker, topic, and optional **`device.pair_peer_token`** in JSON payloads. Local PIN on the exit unit still opens that unit’s door.

### 4.7 `elevator_wait_floor_buttons`

After a **valid PIN** (and optional access-schedule checks for the configured elevator):

1. **Per-floor wait enables:** if **`gpio.elevator_wait_floor_enable_pins`** is non-empty, each listed relay can be turned **ON** for the wait window (only for floors allowed by PIN/floor groups and **floor time rules** — see §8.3–§8.5).
2. **Legacy single enable:** if **`gpio.elevator_wait_floor_enable_pins`** is empty and **`gpio.elevator_enable_relay_pin`** is non-zero, one **elevator_enable** output holds **ON** for all cab buttons (hardware cannot split per floor; PIN/floor/time rules still apply when a floor is selected).
3. **`device.elevator_floor_wait_timeout`:** length of the grant window.
4. **`device.elevator_wait_floor_cab_sense`:**
   - **`sense` (default):** read **`device.elevator_floor_input_pins`** (comma BCM, active low). On stable press, pulse matching **`gpio.elevator_floor_dispatch_pins`** entry (or shared dispatch / door relay if unset).
   - **`ignore`:** leave **`elevator_floor_input_pins`** empty; no cab GPIO; timeout only clears enables (no floor selection from software).
5. Dispatch pulse length: **`device.elevator_dispatch_pulse_duration`**, with optional per-index **`device.elevator_floor_dispatch_pulse_durations`** aligned to dispatch pin list order.

Validation rules tie together counts of cab inputs, wait-enable pins, and dispatch pins; see sample `elevator_parameter_modes` in `virtualkeyz2.json` for a field legend.

### 4.8 `elevator_predefined_floor`

- Valid PIN → **dispatch** (and optional **predefined enable**) pulse; no in-cab floor GPIO.
- **`device.elevator_predefined_floors`:** comma-separated **logical floor labels** (at most one entry in typical single-floor setups); index **`device.elevator_predefined_floor`** selects the entry.
- **`gpio.elevator_predefined_enable_pins`:** optional relay(s) pulsed with **`device.elevator_enable_pulse_duration`** (or derived from dispatch durations).
- **`gpio.elevator_floor_dispatch_pins`:** per-floor dispatch when multiple predefined floors exist; otherwise **`gpio.elevator_dispatch_relay_pin`** or door relay.
- Per-floor access uses the same **floor_index** order as wait-floor mode for **`access_elevator_pin_floors`** and related tables when **`device.access_control_elevator_id`** is set.

---

## 5. Device settings reference (`device` section)

| Key | Purpose |
|-----|---------|
| `heartbeat_interval` | Interval for heartbeat webhook and debug tick (default on the order of 1 minute). |
| `door_open_warning_after` | After door sensor reports open this long, log WARNING and fire `door_open_timeout` webhook. |
| `door_sensor_closed_is_low` | Polarity for door contact vs `gpio.door_sensor_pin`. |
| `sound_card_name` | ALSA device for WAV playback (e.g. `plughw:0,0`). |
| `sound_startup` / `sound_shutdown` / `sound_pin_ok` / `sound_pin_reject` / `sound_keypress` | WAV paths (empty skips that sound). |
| `log_level` | e.g. `debug`, `info`, `warning`, `error`, `critical`, `all`. |
| `pin_length` | Required digit count for PIN entry. |
| `relay_pulse_duration` | Default door relay pulse. |
| `pin_reject_buzzer_after_attempts` | Every Nth wrong PIN can trigger buzzer (0 disables pattern). |
| `buzzer_relay_pulse_duration` | Buzzer relay pulse length. |
| `mqtt_enabled` | Master switch for MQTT client. |
| `mqtt_broker` | URL e.g. `tcp://host:1883`. |
| `mqtt_client_id` | Client identifier; included in webhook payloads as `device_client_id`. |
| `mqtt_username` / `mqtt_password` | Optional broker credentials. |
| `mqtt_command_topic` | Subscribe topic for remote commands (§8). |
| `mqtt_status_topic` | Publish topic for command acknowledgements (JSON). |
| `mqtt_command_token` | If set, commands must be JSON with matching `"token"`. |
| `mqtt_pair_peer_topic` | Pair-peer publish/subscribe topic (§4.6). |
| `pair_peer_role` | `none` \| `entry` \| `exit` for paired remote exit. |
| `pair_peer_token` | Optional secret in pair-peer JSON. |
| `tech_menu_history_max` | Technician command history size (bounded). |
| `keypad_inter_digit_timeout` | Max gap between digits while composing a PIN. |
| `keypad_session_timeout` | Max time from first digit to submit. |
| `pin_entry_feedback_delay` | Pause after PIN OK/reject sounds before listening again. |
| `pin_lockout_enabled` | Enable wrong-PIN lockout. |
| `pin_lockout_after_attempts` / `pin_lockout_duration` | Threshold and lockout duration. |
| `pin_lockout_override_pin` | If set, submitting this PIN clears lockout without opening. |
| `fallback_access_pin` | PIN accepted when no `access_pins` row matches (empty disables). |
| `webhook_event_*` / `webhook_heartbeat_*` | Event and heartbeat POST URLs and optional Bearer tokens (§10). |
| `keypad_operation_mode` | §4. |
| `keypad_evdev_path` / `keypad_exit_evdev_path` | §6 (keypad paths). |
| `elevator_floor_wait_timeout` | Wait-floor grant window. |
| `elevator_wait_floor_cab_sense` | `sense` or `ignore`. |
| `elevator_floor_input_pins` | BCM list (sense mode only). |
| `elevator_predefined_floor` / `elevator_predefined_floors` | §4.8. |
| `elevator_dispatch_pulse_duration` | Default elevator dispatch pulse. |
| `elevator_floor_dispatch_pulse_durations` | Comma durations, one per dispatch index (pads with dispatch duration). |
| `elevator_enable_pulse_duration` | Predefined-floor enable pulse (wait-floor ignores). |
| `dual_keypad_reject_exit_without_entry` | §4.5. |
| `access_control_door_id` | SQLite `access_doors.id` for this device’s door strike (empty = no door schedule binding). |
| `access_control_elevator_id` | SQLite `access_elevators.id` for elevator modes (empty = no elevator schedule binding). |
| `access_schedule_enforce` | When `true` and door/elevator id set, enforce `access_levels` + time windows for that target (default `true` in sample config). |
| `access_schedule_apply_to_fallback_pin` | When `true`, **`fallback_access_pin`** is also subject to schedules. |

Top-level JSON keys outside `device` / `gpio`:

| Key | Purpose |
|-----|---------|
| `tech_menu_prompt` | Short label shown on the technician prompt line. |
| `elevator_parameter_modes` | **Documentation only** — not read by control logic; preserved on `cfg save` to annotate which fields apply to which elevator sub-mode. |

---

## 6. Keypad device paths

Prefer stable symlinks under **`/dev/input/by-id/`** or **`/dev/input/by-path/`** (see §6.1). Bare **`/dev/input/eventN`** can change after reboot.

### 6.1 Installer tool: `listkeypads`

```bash
go run ./tools/listkeypads
go run ./tools/listkeypads -usb
go build -o listkeypads ./tools/listkeypads && sudo install -m755 listkeypads /usr/local/bin/
```

Use **`sudo evtest <USE_PATH>`** to confirm physical mapping. The running service exposes the same table via technician **`kb`** / **`kb all`**.

---

## 7. GPIO reference (`gpio` section)

### 7.1 Relay output backend

| Key | Values |
|-----|--------|
| `relay_output_mode` | `gpio` — relays on SoC BCM numbers. `mcp23017` / `xl9535` — relay outputs on I2C expander pins **0–15** (door, buzzer, elevator outputs as configured). |
| `mcp23017_i2c_bus` / `mcp23017_i2c_addr` | Linux I2C bus index (often `1`) and 7-bit address (often `32` = 0x20). |
| `xl9535_i2c_bus` / `xl9535_i2c_addr` | Same for XL9535 when `relay_output_mode` is `xl9535`. |

**Note:** Door sensor, heartbeat LED, exit/entry buttons, and **elevator cab floor sense inputs** (`elevator_floor_input_pins`) remain **BCM GPIO** on the SoC, not on the expander.

### 7.2 Pin map (typical roles)

| Field | Role |
|-------|------|
| `door_relay_pin` / `door_relay_active_low` | Main strike or door relay. |
| `buzzer_relay_pin` / `buzzer_relay_active_low` | Wrong-PIN buzzer. |
| `door_sensor_pin` | Door position (with `device.door_sensor_closed_is_low`). |
| `heartbeat_led_pin` | Activity LED (toggled by software). |
| `exit_button_pin` / `exit_button_active_low` | REX (`access_entry_with_exit_button`). |
| `entry_button_pin` / `entry_button_active_low` | Entry request (`access_exit_with_entry_button`). |
| `elevator_dispatch_relay_pin` / `elevator_dispatch_active_low` | Shared dispatch when `elevator_floor_dispatch_pins` empty. |
| `elevator_enable_relay_pin` / `elevator_enable_active_low` | Legacy single wait-floor enable (§4.7). |
| `elevator_floor_dispatch_pins` | Comma list: per-floor dispatch outputs (order matches mode rules in §4.7–4.8). |
| `elevator_wait_floor_enable_pins` | Comma list: per-floor “return ground” enables for wait-floor mode. |
| `elevator_predefined_enable_pins` | Predefined-floor call simulation relay(s). |

---

## 8. SQLite access control (`access_control.db`)

Created automatically on startup. Main concepts:

### 8.1 PINs

- **`access_pins`:** `pin` (primary key), optional `label`, `enabled` (1/0).

### 8.2 Time-based access (doors and elevators)

- **`access_doors`**, **`access_door_groups`**, **`access_door_group_members`** — logical doors and grouping.
- **`access_elevators`**, **`access_elevator_groups`**, **`access_elevator_group_members`** — logical elevators and grouping.
- **`access_user_groups`**, **`access_user_group_members`** — user groups (members are PINs).
- **`access_time_profiles`** — named schedule; optional **`iana_timezone`** (empty = host local time).
- **`access_time_windows`** — `weekday` 0–6 (Sun–Sat) or **7** = any day; `start_minute` / `end_minute` 0–1439; if start > end, window crosses midnight.
- **`access_levels`** — links `time_profile_id`, `user_group_id`, `enabled`.
- **`access_level_targets`** — each row grants **exactly one** of: door, door group, elevator, or elevator group.

Bind the device with **`device.access_control_door_id`** and/or **`device.access_control_elevator_id`**. When **`device.access_schedule_enforce`** is true and the id is set, a valid PIN must belong to an **enabled** level whose **time profile** matches **now**, and whose target includes that door/elevator (directly or via a group). If **no** enabled level targets that door/elevator, scheduling is not applied for that target (backward compatible).

### 8.3 Elevator per-floor permissions (floor index)

**`floor_index`** is 0-based in the same order as **`device.elevator_floor_input_pins`**, **`gpio.elevator_wait_floor_enable_pins`**, and **`gpio.elevator_floor_dispatch_pins`** (for the configured mode).

| Table | Role |
|-------|------|
| **`access_elevator_pin_floors`** | `(pin, elevator_id, floor_index)` — optional explicit allowed floors. **No rows** for a PIN+elevator ⇒ all floors allowed (PIN-only). **One or more rows** ⇒ only listed indices. |
| **`access_elevator_floor_groups`** | `(id, elevator_id, display_name)` — e.g. “Public”, “Executive”. |
| **`access_elevator_floor_group_members`** | `(group_id, floor_index)` — floors in a group. |
| **`access_elevator_pin_floor_groups`** | `(pin, group_id)` — PIN inherits the **union** of all member floors of its groups (for that elevator). |

### 8.4 Logical floor labels and relay documentation

- **`access_elevator_floor_labels`:** `(elevator_id, floor_index, floor_name, relay_pin optional)`. Used for clearer **logs** and **webhooks** (`elevator_floor_label` / `elevator_floor_labels`). **`relay_pin`** is optional metadata you set to match wiring (expander index or BCM); it is **not** synced from JSON.

### 8.5 Timed floor open / lock (per floor, reuse time profiles)

- **`access_elevator_floor_time_rules`:** `elevator_id`, `floor_index`, `time_profile_id`, **`action`** `'open'` or `'lock'`, `enabled`.

Semantics (after a valid credential and elevator schedule, if any):

- **`lock`** during an active window → that floor is **denied** (overrides PIN lists and open rules).
- **`open`** during an active window → that floor is **allowed** even if not listed for the PIN (still requires valid PIN / fallback and elevator-level schedule when enforced).

These rules are **independent** of **`access_schedule_enforce`**; they apply whenever matching enabled rows exist.

### 8.6 Example SQL (illustrative)

```sql
-- Elevator + labels
INSERT INTO access_elevators (id, display_name) VALUES ('cab_a','Lobby car A');
INSERT INTO access_elevator_floor_labels (elevator_id,floor_index,floor_name,relay_pin)
  VALUES ('cab_a',0,'Lobby',5);

-- PIN + floor group
INSERT INTO access_elevator_floor_groups (id,elevator_id,display_name) VALUES ('grp_public','cab_a','Public');
INSERT INTO access_elevator_floor_group_members (group_id,floor_index) VALUES ('grp_public',0),('grp_public',1);
INSERT INTO access_pins (pin,label,enabled) VALUES ('123456','Alice',1);
INSERT INTO access_elevator_pin_floor_groups (pin,group_id) VALUES ('123456','grp_public');

-- Night lock on floor 3 (reuse profile + windows)
INSERT INTO access_time_profiles (id,display_name,description,iana_timezone) VALUES ('nights','Nights','','');
INSERT INTO access_time_windows (time_profile_id,weekday,start_minute,end_minute) VALUES ('nights',0,0,1439);
INSERT INTO access_elevator_floor_time_rules (elevator_id,floor_index,time_profile_id,action,enabled)
  VALUES ('cab_a',3,'nights','lock',1);
```

Use `sqlite3 access_control.db` or your own tool to maintain data. Foreign keys are enabled (`_fk=1`).

---

## 9. MQTT

### 9.1 Remote commands (`mqtt_command_topic`)

Payload may be **plain text** command or **JSON** `{"cmd":"..."}`. If **`mqtt_command_token`** is set, payload **must** be JSON and include `"token"` matching the configured token.

| Command aliases | Action |
|-----------------|--------|
| `open_door`, `door_open`, `unlock` | Pulse door relay; webhook `mqtt_remote_door_open` if configured. |
| `buzzer`, `buzz`, `alarm` | Pulse buzzer relay; webhook `mqtt_remote_buzzer`. |
| `door_status`, `status_door` | ACK includes `door_open` bool if door sensor configured. |
| `ping`, `hello` | ACK `detail`: `pong`. |

Acknowledgements are published to **`mqtt_status_topic`** as JSON: `ok`, `cmd`, optional `error`, `detail`, optional `door_open`.

### 9.2 Pair-peer topic (`mqtt_pair_peer_topic`)

JSON `{"cmd":"pulse_paired_exit"}` or `{"cmd":"unlock_peer_exit"}`; optional `"token"` when **`pair_peer_token`** is set. Exit unit pulses local door (§4.6).

---

## 10. HTTP listener

The process starts an HTTP server on **`:8080`** (Gin):

- **`GET /admin`** — placeholder text (“Local Configuration Interface”).
- **`POST /api/remote-control`** — currently returns JSON `{"status":"door_opened"}`; middleware is a stub (no token enforcement in-tree).

Treat port 8080 as **local/debug** unless you firewall or proxy it explicitly.

---

## 11. HTTP webhooks

When enabled, the service POSTs JSON:

- **Events** (`webhook_event_*`): PIN accept/reject, door sensor, MQTT remote, elevator phases, wrong-PIN buzzer, lockout, pair-peer, etc. Payloads **never** include PIN digits. Type field: `"type":"event"`, `"event":"<name>"`, `timestamp`, `device_client_id`, plus mode-specific keys (e.g. `keypad_role`, `credential_label`, `elevator_floor_indices`, **`elevator_floor_label`** / **`elevator_floor_labels`** on some elevator deny events).
- **Heartbeat** (`webhook_heartbeat_*`): once per `heartbeat_interval`; `"type":"heartbeat"`.

Optional **Authorization: Bearer** when `*_token_enabled` and token string are set.

---

## 12. Keypad lockout and override

- **`pin_lockout_enabled`**, **`pin_lockout_after_attempts`**, **`pin_lockout_duration`** — consecutive wrong PIN threshold and keypad ignore period.
- **`pin_lockout_override_pin`** — clears lockout and wrong-PIN streak **without** opening the door.

---

## 13. Troubleshooting

| Symptom | Checks |
|---------|--------|
| No keypad response | Wrong `keypad_evdev_path`; use `listkeypads` / `kb`, then `evtest`. Dual mode: two distinct paths. |
| Door never pulses | GPIO unavailable; relay pin and `active_low`; `relay_output_mode` and I2C wiring for expander relays. |
| Pair exit never opens | MQTT connected; exit `pair_peer_role` = `exit`; same topic and token; broker ACLs. |
| Elevator wait never dispatches | Sense mode: `elevator_floor_input_pins` and active-low wiring; timeout; dispatch pin list lengths vs inputs/enables. |
| PIN OK but elevator rejects floor | SQLite: `access_elevator_pin_floors`, floor groups, **`access_elevator_floor_time_rules`** (`lock`/`open`); `access_control_elevator_id` must match `elevator_id` in rows. |
| Schedule seems ignored | `access_schedule_enforce`; door/elevator id set; enabled `access_levels` targeting that id; time profile timezone; windows weekday/minutes. |
| Config “stuck” after edit | `cfg reload` or restart; GPIO / I2C changes need **restart**. |
| MQTT command ignored | Topic spelling; if token set, JSON + correct `token`. |

---

## 14. Related files

| File | Purpose |
|------|---------|
| `virtualkeyz2.json` | Main configuration |
| `access_control.db` | SQLite PINs and access schedules (working directory) |
| `changelog.txt` | Human-readable change history |
| `tools/bump-version.sh` | Version + changelog bump |
| `tools/listkeypads` | Stable evdev paths for keypad JSON |

---

*Product line: VirtualKeyz 2.x. For technical support, refer to your integrator or project maintainer.*
