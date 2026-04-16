# VirtualKeyz2 — Operator Guide

This document is for installers and operators who configure and run the service on a Raspberry Pi (or similar Linux host). It describes behaviour, **`device`** / **`gpio`** JSON keys, SQLite access control, **`acl`** technician commands, **exception calendars** (holidays), **door-open alarms** and **webhooks**, **audit logging**, and wiring notes. Authoritative field names match `virtualkeyz2.json`.

---

## 1. Process flags, configuration file, and applying changes

| Flag | Meaning |
|------|---------|
| `-config <path>` | JSON configuration file (default `virtualkeyz2.json` in the working directory). |
| `-daemon` | Declares daemon-style startup (logging only; no extra integration in-tree). |
| `-notechmenu` | Disable the interactive technician menu on `/dev/tty` when a TTY is present. |

**Applying changes:** edit JSON, then either **restart the process** or use the technician menu: **`cfg reload`** loads from disk; **`cfg apply`** / **`cfg live`** refreshes in-memory settings (MQTT, log level, durations, paths, and keys handled by **`cfg set`**).

**GPIO / relay mapping:** changing **`gpio`** pin numbers, **`relay_output_mode`**, or I2C settings **requires a process restart**; **`cfg apply`** does not re-initialise hardware.

**Database:** the service opens SQLite **`access_control.db`** in the **current working directory** (PINs, schedules, exception calendars, elevator floor rules, and the **`logs`** audit table). Ensure the service user can read/write this file.

**Building:** build the **package directory**, not a single source file:

```bash
cd /path/to/virtualkeyz2
go build -o virtualkeyz2 .
```

Using only `go build virtualkeyz2.go` omits other `.go` files in the package and will fail or miss code paths.

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
| `acl` / `acl help` | **Access-control help** (SQLite doors, PINs, schedules, exception calendars); Tab completes subcommands |
| `v` | Software build version and release |
| `ch` | Changelog text |
| `i` | Network snapshot (Ethernet / Wi‑Fi, DNS, gateways) |
| `p` | System-wide listening TCP/UDP ports |
| `occ` | Dual USB mode: in-memory area occupancy (masked PIN tails + `access_pins` labels) |
| `kb` / `kb all` | List **by-id/by-path** USE_PATH values (`kb` = USB only; `kb all` = include non-USB) |
| `cfg set <key> <value>` | Change one setting in memory (see §5 and **`cfg keys`**) |
| `cfg save` | Write current config to the `-config` JSON path |
| `cfg reload` | Load JSON from disk and apply |
| `cfg apply` / `cfg live` | Apply in-memory config (e.g. after `cfg set`) |
| `cfg keys` | All **`cfg set`** keys (snake_case, same as JSON `device` / `gpio` flat keys) |
| `cfg history clear` | Clear command history |
| `...` | Shutdown request (same path as SIGTERM) |

### 3.1 Tab completion

Press **Tab** to complete top-level commands, **`cfg`** subcommands, **`cfg set`** keys, **`acl`** categories and verbs (e.g. **`acl pin hold_extra`**). If several names share a prefix, Tab may extend only to the common prefix; type another character and press Tab again.

### 3.2 `cfg` versus `acl` versus JSON

| Mechanism | What it changes |
|-----------|-----------------|
| **`cfg set` / `cfg save`** | **`virtualkeyz2.json`** — scalar **`device`** and **`gpio`** fields. Nested webhook maps / endpoint lists (**`webhook_event_types`**, **`webhook_event_endpoints`**) are edited in JSON (or merge partial JSON on reload), not via **`cfg set`**. |
| **`acl …` commands** | Rows in **`access_control.db`** (doors, elevators, PINs, groups, profiles, windows, levels, targets, exception calendars). |
| **`acl bind door|elevator`** | Convenience: sets **`access_control_door_id`** / **`access_control_elevator_id`** in memory like **`cfg set`**; run **`cfg save`** to persist in JSON. |

---

## 4. Access control commands (`acl`) — reference

All **`acl`** commands run from the technician menu. Full inline help: **`acl help`**. Errors often include a **hint** (for example: create a door with **`acl door add`** before **`acl target door`**).

**Conventions**

- **Display names** in the CLI should be a **single token**; use underscores instead of spaces (e.g. `Main_Entrance`).
- **Time windows:** `weekday` is **0 = Sunday … 6 = Saturday**, or **7 = any day**. **`start_minute`** and **`end_minute`** are **0–1439** (minutes from midnight). If start > end, the window crosses midnight.
- **PINs** live in **`access_pins`**. List commands show what you query; PIN policy is your organisation’s responsibility.

### 4.1 Discover and summary

| Command | Purpose |
|---------|---------|
| `acl summary` | **`access_control_door_id`**, **`access_control_elevator_id`**, **`access_schedule_enforce`**, and row counts for main ACL tables plus **`logs`**. |
| `acl door list` | **`access_doors`**. |
| `acl door_group list` | **`access_door_groups`**. |
| `acl elevator list` | **`access_elevators`**. |
| `acl elevator_group list` | **`access_elevator_groups`**. |
| `acl pin list` | **`access_pins`** columns including **`door_hold_extra_seconds`** (§4.5). |
| `acl group list` | User groups and member PIN counts. |
| `acl profile list` | **`access_time_profiles`** (id, display name, IANA timezone, **`respects_exception_calendar`**). |
| `acl level list` | **`access_levels`**. |
| `acl target list` | **`access_level_targets`**. |
| `acl exception calendar list` | Exception calendars (§4.8). |
| `acl exception date list [calendar_id]` | Exception dates. |

### 4.2 Bind this controller to a logical door or elevator

```text
acl bind door east
acl bind elevator cab_a
cfg save
```

The id must match a row you created and, for schedule enforcement, a **`acl target …`** row (§4.7).

### 4.3 Doors, elevators, and groups

```text
acl door add east Main_Entrance
acl elevator add cab_a Lobby_car_A
acl door_group add all_exits North_exits
acl elevator_group add bank1 Bank_1_cars
```

**Note:** **`acl`** does not manage **`access_door_group_members`** or **`access_elevator_group_members`** (which physical doors/cars belong to a group). Use **`sqlite3`** or your provisioning tool for those joins.

### 4.4 PINs and user groups

```text
acl pin add 123456 Alice
acl pin add temporary 999999 2026-12-31T23:59:59Z Visitor1 --max-uses 10
acl pin hold_extra 123456 120
acl pin disable 123456
acl pin enable 123456

acl group add staff Staff
acl group join staff 123456
acl group leave staff 123456
```

- **`acl pin hold_extra <pin> <extra_seconds>`** — sets **`access_pins.door_hold_extra_seconds`**. **0** clears. Extends the **first** door-open alarm threshold for the **next** time this credential unlocks the door (accessibility / slower entry); see §10. Maximum **24h** worth of seconds is enforced in software.

### 4.5 `access_pins` columns (operator-relevant)

| Column | Meaning |
|--------|---------|
| `pin`, `label`, `enabled` | Credential identity and on/off. |
| `temporary`, `expires_at`, `max_uses`, `use_count` | Visitor/contractor lifecycle: temporary PINs need **`expires_at`** (RFC3339); optional **`max_uses`** caps successful grants. |
| **`door_hold_extra_seconds`** | Optional extra time (seconds) added to **`device.door_open_warning_after`** for the open period following an unlock by this PIN (§10). |

### 4.6 Time profiles and windows

```text
acl profile add biz Business_Hours
acl profile add nights After_hours Asia/Bangkok
acl profile respects_exceptions biz on

acl window add biz 1 525 1020
```

**`acl profile respects_exceptions <profile_id> on|off`** — when **on** (default), profiles that represent “standard business” hours follow **exception calendars** (holidays / early close). When **off**, that profile ignores exception dates (e.g. 24/7-style profiles).

### 4.7 Access levels and targets

```text
acl level add L1 biz staff Staff_weekdays
acl level disable L1
acl level enable L1

acl target door L1 east
acl target elevator L1 cab_a
acl target door_group L1 all_exits
acl target elevator_group L1 bank1
acl target list
```

### 4.8 Exception calendars (holidays / early closure)

Civil dates are interpreted in **`device.access_exception_site_timezone`** (IANA, e.g. `America/New_York`); **empty** uses the system local timezone.

```text
cfg set access_exception_site_timezone America/New_York
cfg save

acl exception calendar add national National 100
acl exception date add national 2026-12-25 full Christmas
acl exception date add national 2026-12-24 early 780 Christmas_Eve_1pm_close
acl exception import national /path/to/holidays.csv
```

- **`acl exception calendar add <id> [display_name [priority [source_note]]]`** — priority resolves overlapping calendars (higher wins).
- **`acl exception date add`** — **`full`** = holiday (access windows that respect exceptions are treated as closed that day); **`early <minute>`** = early closure that day (minute 0–1439 from midnight in the profile’s timezone evaluation path).
- **`acl exception import <calendar_id> <csv_path>`** — CSV: `YYYY-MM-DD,full|early[,minute][,label]`.

### 4.9 End-to-end example (door + schedule + PIN)

```text
acl door add east Main_Entrance
acl pin add 123456 Alice
acl group add staff Staff
acl group join staff 123456
acl profile add biz Business_Hours
acl window add biz 1 525 1020
acl level add L1 biz staff Staff_on_schedule
acl target door L1 east
acl bind door east
cfg set access_schedule_enforce true
cfg save
```

### 4.10 What stays in SQL / outside `acl`

The **`acl`** menu covers the core schedule and exception model. Advanced elevator floor tables (**`access_elevator_pin_floors`**, floor groups, **`access_elevator_floor_time_rules`**, labels) may still be maintained with **`sqlite3`** or your provisioning tool — see §8.2.

---

## 5. Device and GPIO settings (`cfg set` / JSON)

All keys below are settable with **`cfg set <key> <value>`** (same names as JSON). Durations use Go syntax (e.g. `10s`, `400ms`, `1m0s`). Booleans: `true` / `false`. **`cfg keys`** prints a compact list with short descriptions.

### 5.1 Logging, heartbeat, sounds

| Key | Purpose |
|-----|---------|
| `log_level` | `debug`, `info`, `warning`, `error`, `critical`, `all` (empty = all). |
| `heartbeat_interval` | Heartbeat webhook tick interval and internal debug cadence. |
| `sound_card_name` | ALSA device for WAV playback (e.g. `plughw:0,0`). |
| `sound_startup` / `sound_shutdown` / `sound_pin_ok` / `sound_pin_reject` / `sound_keypress` | WAV paths (empty skips). |

### 5.2 Door sensor, door-open alarms, and accessibility

Requires **`gpio.door_sensor_pin`** non-zero and a configured polarity.

| Key | Purpose |
|-----|---------|
| `door_sensor_closed_is_low` | **true** = closed when GPIO reads low (typical pull-up to 3.3 V, contact to GND when closed). |
| `door_open_warning_after` | **Base** duration the door may stay open before the **first** **`door_open_timeout`** webhook. |
| `door_open_alarm_interval` | Interval between **repeat** **`door_open_timeout`** webhooks after the first (default **30s** if unset/zero after normalisation). |
| `door_open_alarm_max_count` | Maximum **`door_open_timeout`** events per **continuous** open period; **0 = unlimited**. |
| `door_forced_after_warnings` | After this many **`door_open_timeout`** events in **one** open period, emit **`door_forced`** once and **stop further door alarm webhooks** until the door has **closed and opened again**; **0 = never**. |

Per-PIN extra time before the first alarm: **`access_pins.door_hold_extra_seconds`** via **`acl pin hold_extra`** (§4.4–4.5). The effective first threshold is **`door_open_warning_after` + extra** for the open period following that PIN’s unlock.

### 5.3 PIN entry, lockout, fallback

| Key | Purpose |
|-----|---------|
| `pin_length` | Digits before auto-submit; **0** = require **Enter** / KP Enter to submit. |
| `keypad_inter_digit_timeout` | Max gap between digits (clamped in software). |
| `keypad_session_timeout` | Max time from first digit to submit/clear. |
| `pin_entry_feedback_delay` | Pause after PIN OK/reject before accepting new keys. |
| `pin_lockout_enabled` | Master switch for wrong-PIN lockout. |
| `pin_lockout_after_attempts` | Consecutive wrong PINs before lockout (0 = off; else clamped 3–5). |
| `pin_lockout_duration` | Keypad ignore duration after lockout. |
| `pin_lockout_override_pin` | Submitted PIN clears lockout **without** opening the door. |
| `pin_reject_buzzer_after_attempts` | Wrong-PIN streak threshold for buzzer pulse (**0** = off). |
| `buzzer_relay_pulse_duration` | Buzzer relay pulse length. |
| `fallback_access_pin` | Accepted when no **`access_pins`** row matches (empty disables). |
| `access_schedule_apply_to_fallback_pin` | When **true**, fallback PIN is subject to schedules too. |

### 5.4 MQTT

| Key | Purpose |
|-----|---------|
| `mqtt_enabled` | Client on/off. |
| `mqtt_broker` | e.g. `tcp://host:1883`. |
| `mqtt_client_id` | Client id; copied to webhooks and **`logs.device_client_id`**. |
| `mqtt_username` / `mqtt_password` | Optional broker credentials. |
| `mqtt_command_topic` | Subscribe topic for remote commands (§9). |
| `mqtt_status_topic` | JSON command acknowledgements. |
| `mqtt_command_token` | If set, command payload must be JSON with matching **`token`**. |
| `mqtt_pair_peer_topic` | Pair-peer topic (§6.6). |
| `pair_peer_role` | `none` \| `entry` \| `exit`. |
| `pair_peer_token` | Optional secret in pair-peer JSON. |

### 5.5 Webhooks (scalars via `cfg set`; lists/maps in JSON)

| Key | Purpose |
|-----|---------|
| `webhook_event_enabled` | POST discrete events when **true** and URL(s) configured. |
| `webhook_event_url` | Legacy single event URL (used only if **`webhook_event_endpoints`** is empty). |
| `webhook_event_token_enabled` / `webhook_event_token` | Optional **Authorization: Bearer** for legacy URL. |
| `webhook_heartbeat_enabled` | POST heartbeats when **true** and URL set. |
| `webhook_heartbeat_url` | Heartbeat URL. |
| `webhook_heartbeat_token_enabled` / `webhook_heartbeat_token` | Bearer for heartbeat. |

**In JSON only (not individual `cfg set` keys):**

- **`webhook_event_types`** — Global allowlist: if non-empty, only event names with value **true** are sent.
- **`webhook_event_endpoints`** — Array of `{ enabled, url, token_enabled, token, event_types? }`. If **non-empty**, it **replaces** **`webhook_event_url`** for event delivery until the list is cleared. Each endpoint may have its own **`event_types`** allowlist.

### 5.6 Operation mode and access binding

| Key | Purpose |
|-----|---------|
| `keypad_operation_mode` | §6. |
| `keypad_evdev_path` / `keypad_exit_evdev_path` | evdev devices (dual mode: must differ). |
| `access_control_door_id` | SQLite **`access_doors.id`** for this device. |
| `access_control_elevator_id` | SQLite **`access_elevators.id`** for elevator modes. |
| `access_schedule_enforce` | When **true** and id set, enforce levels + windows for that target. |
| `access_exception_site_timezone` | IANA zone for exception-calendar **civil** dates (§4.8). |
| `tech_menu_history_max` | Technician command history cap. |
| `tech_menu_prompt` | Top-level JSON: prompt label (not under **`device`**). |

### 5.7 Elevator-specific `device` keys

| Key | Purpose |
|-----|---------|
| `elevator_floor_wait_timeout` | Wait-floor grant window. |
| `elevator_wait_floor_cab_sense` | `sense` (default) or `ignore`. |
| `elevator_floor_input_pins` | Comma BCM list (sense mode). |
| `elevator_predefined_floor` / `elevator_predefined_floors` | Predefined-floor mode indices / labels. |
| `elevator_dispatch_pulse_duration` | Default dispatch pulse. |
| `elevator_floor_dispatch_pulse_durations` | Comma-separated durations per dispatch index. |
| `elevator_enable_pulse_duration` | Predefined-floor enable pulse (wait-floor uses full timeout for enables). |
| `dual_keypad_reject_exit_without_entry` | Dual USB: reject exit without entry when **true**. |

### 5.8 GPIO (`gpio` section)

| Key | Purpose |
|-----|---------|
| `relay_output_mode` | `gpio` \| `mcp23017` \| `xl9535`. |
| `mcp23017_i2c_bus` / `mcp23017_i2c_addr` | MCP23017 I2C bus and 7-bit address. |
| `xl9535_i2c_bus` / `xl9535_i2c_addr` | XL9535 bus and address. |
| `door_relay_pin` / `door_relay_active_low` | Strike / door relay (BCM or expander index per mode). |
| `buzzer_relay_pin` / `buzzer_relay_active_low` | Wrong-PIN buzzer. |
| `door_sensor_pin` | Door position input (**BCM**; not on expander). |
| `heartbeat_led_pin` | Activity LED (**BCM**). |
| `exit_button_pin` / `exit_button_active_low` | REX (**BCM**). |
| `entry_button_pin` / `entry_button_active_low` | Entry request (**BCM**). |
| `elevator_dispatch_relay_pin` / `elevator_dispatch_active_low` | Shared dispatch when no per-floor list. |
| `elevator_enable_relay_pin` / `elevator_enable_active_low` | Legacy single wait-floor enable. |
| `elevator_floor_dispatch_pins` | Comma BCM or expander indices for per-floor dispatch. |
| `elevator_wait_floor_enable_pins` | Wait-floor “return ground” enables. |
| `elevator_predefined_enable_pins` | Predefined-floor call simulation. |

**Note:** Door sensor, heartbeat LED, exit/entry buttons, and **cab floor sense** inputs remain **SoC BCM GPIO**, not on the I2C expander.

### 5.9 Top-level JSON (besides `device` / `gpio`)

| Key | Purpose |
|-----|---------|
| `tech_menu_prompt` | Short label on the technician prompt line. |
| `elevator_parameter_modes` | **Documentation only** — not read by control logic; preserved on **`cfg save`**. |

---

## 6. Operation modes (`device.keypad_operation_mode`)

Set **exactly one** of the following string values.

### 6.1 `access_entry` (default)

Single USB keypad on **`keypad_evdev_path`**. Valid PIN → pulse **door** relay.

### 6.2 `access_exit`

Same behaviour as entry; logs/webhooks record the mode for wiring clarity.

### 6.3 `access_entry_with_exit_button`

Valid PIN → door pulse. **`exit_button_pin`**: free egress.

### 6.4 `access_exit_with_entry_button`

Keypad at exit; **`entry_button_pin`**: request entry from inside.

### 6.5 `access_dual_usb_keypad`

**`keypad_evdev_path`** = entry; **`keypad_exit_evdev_path`** = exit (must differ). Occupancy and **`occ`** command; **`dual_keypad_reject_exit_without_entry`**.

### 6.6 `access_paired_remote_exit`

| Unit | `pair_peer_role` | Behaviour |
|------|------------------|-----------|
| **Entry** | `entry` | Valid PIN → local door + publish JSON to **`mqtt_pair_peer_topic`**. |
| **Exit** | `exit` | Subscribes; on valid message, pulses local door. |

### 6.7 `elevator_wait_floor_buttons`

After valid PIN (and schedule checks if configured): per-floor or legacy enable relays, **`elevator_floor_wait_timeout`**, cab sense **`elevator_wait_floor_cab_sense`**, floor inputs and dispatch as in the previous OPERATOR sections (see `elevator_parameter_modes` in JSON for a field legend).

### 6.8 `elevator_predefined_floor`

Valid PIN → dispatch / predefined enable pulses; no in-cab GPIO. Floor ACL uses **`access_control_elevator_id`** and SQLite floor tables (§8.2).

---

## 7. Exception calendars vs time windows

**`access_time_windows`** define recurring weekly hours. **`access_exception_dates`** (via **`acl exception …`**) define **specific civil dates** where:

- **`full`** — holiday: profiles with **`respects_exception_calendar`** treat the day as closed.
- **`early`** — early closure: schedules end at the given minute that day.

Evaluation uses **`access_exception_site_timezone`** for date boundaries when set. **`acl profile respects_exceptions`** controls whether a given profile reacts to those dates.

---

## 8. SQLite (`access_control.db`)

Created on startup in the process **working directory**. DSN uses **`_fk=1`** and a busy timeout.

### 8.1 Schedule model (summary)

**`access_doors`**, **`access_elevators`**, groups, **`access_user_groups`**, **`access_time_profiles`**, **`access_time_windows`**, **`access_levels`**, **`access_level_targets`**. Bind the device with **`access_control_door_id`** / **`access_control_elevator_id`**. If **no** enabled level targets that door/elevator, schedule enforcement is **not** applied for that target (backward compatible).

### 8.2 Elevator per-floor permissions and time rules

**`floor_index`** order matches **`elevator_floor_input_pins`**, **`elevator_wait_floor_enable_pins`**, and **`elevator_floor_dispatch_pins`** for the configured mode.

| Table | Role |
|-------|------|
| **`access_elevator_pin_floors`** | Explicit allowed floors; no rows ⇒ all floors (PIN-only). |
| **`access_elevator_floor_groups`** / **`access_elevator_floor_group_members`** / **`access_elevator_pin_floor_groups`** | Floor group inheritance. |
| **`access_elevator_floor_labels`** | Human-readable labels for logs/webhooks. |
| **`access_elevator_floor_time_rules`** | Per-floor **`open`** / **`lock`** windows using time profiles. |

### 8.3 Event audit log (`logs`)

Every **event** that would drive an **event webhook** is appended to **`logs`** with the same **`event_name`** and **`detail_json`** (no PIN digits), **even if** **`webhook_event_enabled`** is false. **Heartbeats** are **not** stored in **`logs`**.

| Column | Meaning |
|--------|---------|
| `event_name` | e.g. `pin_accepted`, `door_open_timeout`, `door_forced`. |
| `device_client_id` | **`mqtt_client_id`** at insert time. |
| `detail_json` | JSON detail map (matches webhooks). |

---

## 9. MQTT

### 9.1 Remote commands (`mqtt_command_topic`)

Payload: plain text command or JSON **`{"cmd":"..."}`**. If **`mqtt_command_token`** is set, use JSON with **`"token"`**.

| Command aliases | Action |
|-----------------|--------|
| `open_door`, `door_open`, `unlock` | Pulse door relay; **`mqtt_remote_door_open`**. |
| `buzzer`, `buzz`, `alarm` | Buzzer relay; **`mqtt_remote_buzzer`**. |
| `door_status`, `status_door` | ACK includes **`door_open`** if sensor configured. |
| `ping`, `hello` | ACK **`detail`**: `pong`. |

Acknowledgements on **`mqtt_status_topic`**: **`ok`**, **`cmd`**, optional **`error`**, **`detail`**, optional **`door_open`**.

### 9.2 Pair-peer (`mqtt_pair_peer_topic`)

JSON **`{"cmd":"pulse_paired_exit"}`** or **`unlock_peer_exit`**; optional **`token`**. Fires **`mqtt_pair_peer_exit_pulse`**.

---

## 10. Door sensor behaviour (summary)

1. **Transitions:** **`door_opened`** / **`door_closed`** webhooks (and audit) on edge; closing clears per-PIN **`door_hold_extra`** grace for the next cycle.
2. **First alarm:** when open duration exceeds **`door_open_warning_after` +** current **`door_hold_extra`** (from last credential grant that set it).
3. **Repeats:** **`door_open_timeout`** every **`door_open_alarm_interval`**, up to **`door_open_alarm_max_count`** (if non-zero).
4. **Forced:** at **`door_forced_after_warnings`** timeout count, **`door_forced`** fires; then **no** further **`door_open_timeout`** / **`door_forced`** until **close** then **open** again.
5. Payloads include fields such as **`warning_sequence`**, **`threshold_effective`**, **`door_hold_extra`**, **`forced_after_warnings`** where applicable.

---

## 11. HTTP listener

Server on **`:8080`** (Gin): **`GET /admin`** placeholder; **`POST /api/remote-control`** stub response. Treat as **local/debug** unless firewalled.

---

## 12. HTTP webhooks and event names

- **Events:** **`type`**: **`event`**, **`event`**: `<name>`, **`timestamp`**, **`device_client_id`**, plus detail keys. No PIN digits.
- **Heartbeat:** **`type`**: **`heartbeat`**, **`heartbeat_interval`**, etc.

**Common event names** (for **`webhook_event_types`** / endpoint allowlists):

`pin_accepted`, `pin_rejected`, `wrong_pin_buzzer`, `keypad_lockout_activated`, `keypad_lockout_override`, `door_opened`, `door_closed`, `door_open_timeout`, `door_forced`, `mqtt_remote_door_open`, `mqtt_remote_buzzer`, `mqtt_pair_peer_exit_pulse`, `elevator_floor_denied`, `elevator_floor_selected`, `elevator_floor_timeout`, and credential lifecycle / schedule reasons embedded in **`pin_rejected`** details.

---

## 13. Keypad device paths

Prefer **`/dev/input/by-id/`** or **`/dev/input/by-path/`**. Tool: **`go run ./tools/listkeypads`** (or **`kb`** in the technician menu).

---

## 14. Troubleshooting

| Symptom | Checks |
|---------|--------|
| No keypad | **`keypad_evdev_path`**; **`listkeypads`** / **`kb`**; **`evtest`**. |
| Door never pulses | GPIO / expander mapping; **`relay_output_mode`**. |
| Door alarms wrong | **`door_sensor_closed_is_low`**; **`door_open_*`** timings; **`acl pin hold_extra`**. |
| No repeat webhooks | **`door_open_alarm_interval`**; **`door_open_alarm_max_count`** not already reached. |
| **`door_forced` never fires** | **`door_forced_after_warnings`** > 0; allowlist includes **`door_forced`**. |
| Webhook missing | **`webhook_event_enabled`**; URL or **`webhook_event_endpoints`**; **`webhook_event_types`** not blocking the event name. |
| Pair exit dead | MQTT, **`pair_peer_role`**, topic, token, broker ACLs. |
| Elevator floor denied | SQLite floor ACL + **`access_elevator_floor_time_rules`**; **`access_control_elevator_id`**. |
| Schedule ignored | **`access_schedule_enforce`**; bind ids; **`acl target list`**; timezones. |
| Holiday wrong | **`access_exception_site_timezone`**; **`acl exception date list`**; **`respects_exceptions`** on profile. |
| **`acl` fails** | **`acl help`**; create entities in dependency order. |
| GPIO “stuck” | **Restart** after **`gpio`** / I2C changes. |
| Build errors | **`go build -o virtualkeyz2 .`** from package directory. |

---

## 15. Related files

| File | Purpose |
|------|---------|
| `virtualkeyz2.json` | Main configuration |
| `virtualkeyz2.go` | Main application (menu, **`acl`**, MQTT, GPIO, door monitor) |
| `exception_calendar.go` | Exception calendar resolution |
| `access_control.db` | SQLite (PINs, ACL, exceptions, **`logs`**) |
| `changelog.txt` | Release history |
| `tools/bump-version.sh` | Version bump |
| `tools/listkeypads` | evdev discovery |

---

*VirtualKeyz 2.x. For support, contact your integrator or project maintainer.*
