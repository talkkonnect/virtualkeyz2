# VirtualKeyz2 — Operator Guide

This document is for installers and operators who configure and run the service on a Raspberry Pi (or similar Linux host). It covers process flags, **`virtualkeyz2.json`** (**`device`**, **`gpio`**, top-level keys), the **`/dev/tty`** technician menu, SQLite access control (**`acl`**), exception calendars, door-open alarms, **lighting**, **fireman's service** (emergency bypass), **fire-alarm fail-unlock**, optional **tamper** and **motion** inputs, **MQTT** and **HTTP** endpoints, **webhooks** (including circuit breaker and multi-endpoint delivery), and **audit logging**. Field names match the JSON keys used by **`cfg set`** and the sample **`virtualkeyz2.json`**.

---

## 1. Process flags, configuration file, and applying changes

| Flag | Meaning |
|------|---------|
| `-config <path>` | JSON configuration file (default **`virtualkeyz2.json`** in the working directory). If the file is missing, built-in defaults are used and a message is logged. |
| `-daemon` | Prints a “daemon mode” banner only; there is no extra in-tree integration (use **systemd** or your supervisor to run detached). |
| `-notechmenu` | Disable the interactive technician menu on **`/dev/tty`** when a TTY is present. |

**Applying changes**

- **`cfg set <key> <value>`** (technician menu) updates **in-memory** values immediately for that key.
- **`cfg apply`** / **`cfg live`** — reconnect **MQTT**, reapply **log severity filter**, **`tech_menu_prompt`**, and re-sample **fireman's service** / **fire-alarm interface** state from GPIO after config-driven changes. **Does not** re-open the GPIO chip or re-map relay pins.
- **`cfg reload`** — read JSON from disk (**`-config`** path), merge into memory, then same live effects as **`cfg apply`**, plus the GPIO sync steps above.
- **`cfg save`** — write the current in-memory document to the **`-config`** JSON path (includes **`device`**, **`gpio`**, **`tech_menu_prompt`**, and preserved **`elevator_parameter_modes`** documentation blob).
- **`cfg restart`** / **`cfg reboot`** — **`exec`** the same binary with the same arguments and environment so **GPIO**, **I2C expanders**, and all hardware paths are re-initialized. Use after changing **`gpio.*`**, **`relay_output_mode`**, or whenever **`cfg reload`** is not enough.

**GPIO / relay mapping:** changing **`gpio`** pin numbers, **`relay_output_mode`**, or **I2C** settings requires a **full process restart** (**`cfg restart`** or stop/start the service). **`cfg reload`** / **`cfg apply`** alone do not re-run hardware setup.

**Database:** the service opens SQLite **`access_control.db`** in the **current working directory** (PINs, schedules, exception calendars, elevator floor rules, dual-keypad zone occupancy when used, and the **`logs`** audit table). Ensure the service user can read/write this file.

**Building:** build the **`cmd/virtualkeyz2`** main package:

```bash
cd /path/to/virtualkeyz2
go build -o virtualkeyz2 ./cmd/virtualkeyz2
```

The application code lives under **`internal/app`** (not the repo root package).

---

## 2. Software build version

- On startup the log line includes the **build version** and **release timestamp** (UTC).
- From the technician menu: **`v`** shows version and release; **`ch`** prints **`changelog.txt`** if found (next to the binary, current working directory, or project root).
- Developers bump version and changelog with **`./tools/bump-version.sh "description"`**.

---

## 3. Technician menu (console / `/dev/tty`)

When the process has a TTY and **`-notechmenu`** is not used, a bottom-line prompt appears (**`tech_menu_prompt`**). The banner (**`h`** / **`help`**) lists numeric shortcuts and **`acl`** / **`cfg`** usage.

| Input | Action |
|-------|--------|
| **`h`** / **`help`** / **`menu`** / **`m`** | Main menu / help text |
| **`1`** / **`cfg list`** | Full configuration (sensitive tokens shown as **`(set)`**) |
| **`c`** / **`cls`** / **`clear`** | Clear screen and restore bottom prompt layout |
| **`z`** | Clear command history |
| **`2`** | Door sensor: read once |
| **`3`** | Watch door sensor (~2 s) |
| **`4`** / **`5`** | Test pulse: door relay / buzzer relay |
| **`6`** / **`7`** | Show / reset wrong-PIN streak |
| **`8`** / **`9`** | Test sounds (key / PIN OK) |
| **`acl`** / **`acl help`** | SQLite access control; Tab completes subcommands |
| **`v`** | Build version and release (UTC) |
| **`ch`** | Changelog text |
| **`i`** | Network snapshot (Ethernet / Wi‑Fi, DNS, gateways) |
| **`p`** | System-wide listening TCP/UDP ports |
| **`occ`** | Dual USB mode: in-memory area occupancy (masked PIN tails + **`access_pins`** labels) |
| **`kb`** / **`kb all`** | List **by-id/by-path** paths (**`kb`** = USB only; **`kb all`** = include non-USB) |
| **`cfg set <key> <value>`** | Change one setting in memory (see §5; **`cfg keys`** for the full list) |
| **`cfg save`** | Write current config to the **`-config`** JSON path |
| **`cfg reload`** | Load JSON from disk and apply live |
| **`cfg apply`** / **`cfg live`** | Reconnect MQTT, refresh log filter / prompt, sync fireman's + fire-alarm GPIO state |
| **`cfg restart`** / **`cfg reboot`** | Replace process via **`exec`** (same argv/env; full GPIO re-init) |
| **`cfg keys`** | All **`cfg set`** keys (snake_case; **`device`** and **`gpio`** fields in one flat namespace) |
| **`cfg history clear`** | Clear command history |
| **`firemans`** / **`fs`** **`on` \| `off` \| `status`** | Fireman's service (requires **`firemans_service_enabled`**) |
| **`exit`** / **`q`** / **`quit`** | Leave menu loop (does not stop the process) |
| **`...`** / **`…`** | Shutdown request (same path as **SIGTERM**) |

### 3.1 Tab completion

Press **Tab** to complete top-level commands, **`cfg`** subcommands, **`cfg set`** keys, and **`acl`** categories and verbs (e.g. **`acl pin hold_extra`**). If several names share a prefix, Tab may extend only to the common prefix; type another character and press Tab again.

### 3.2 `cfg` versus `acl` versus JSON

| Mechanism | What it changes |
|-----------|-----------------|
| **`cfg set` / `cfg save`** | Scalar fields in **`virtualkeyz2.json`** — all keys listed under **`cfg keys`** (both **`device`**-backed and **`gpio`**-backed names are flat). Nested webhook maps / endpoint lists (**`webhook_event_types`**, **`webhook_event_endpoints`**) are edited in JSON (or merged on reload), not via **`cfg set`**. |
| **`acl …` commands** | Rows in **`access_control.db`** (doors, elevators, PINs, groups, profiles, windows, levels, targets, exception calendars). |
| **`acl bind door|elevator`** | Convenience: sets **`access_control_door_id`** / **`access_control_elevator_id`** in memory like **`cfg set`**; run **`cfg save`** to persist in JSON. |

---

## 4. Access control commands (`acl`) — reference

All **`acl`** commands run from the technician menu. Full inline help: **`acl help`**. Errors often include a **hint** (for example: create a door with **`acl door add`** before **`acl target door`**).

**Conventions**

- **Display names** in the CLI should be a **single token**; use underscores instead of spaces (e.g. **`Main_Entrance`**).
- **Time windows:** **`weekday`** is **0 = Sunday … 6 = Saturday**, or **7 = any day**. **`start_minute`** and **`end_minute`** are **0–1439** (minutes from midnight). If start > end, the window crosses midnight.
- **PINs** live in **`access_pins`**. List commands show what you query; PIN policy is your organisation’s responsibility.

### 4.1 Discover and summary

| Command | Purpose |
|---------|---------|
| **`acl summary`** | **`access_control_door_id`**, **`access_control_elevator_id`**, **`access_schedule_enforce`**, and row counts for main ACL tables plus **`logs`**. |
| **`acl door list`** | **`access_doors`**. |
| **`acl door_group list`** | **`access_door_groups`**. |
| **`acl elevator list`** | **`access_elevators`**. |
| **`acl elevator_group list`** | **`access_elevator_groups`**. |
| **`acl pin list`** | **`access_pins`** columns including **`door_hold_extra_seconds`** (§4.5). |
| **`acl group list`** | User groups and member PIN counts. |
| **`acl profile list`** | **`access_time_profiles`** (id, display name, IANA timezone, **`respects_exception_calendar`**). |
| **`acl level list`** | **`access_levels`**. |
| **`acl target list`** | **`access_level_targets`**. |
| **`acl exception calendar list`** | Exception calendars (§4.8). |
| **`acl exception date list [calendar_id]`** | Exception dates. |

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
| **`pin`**, **`label`**, **`enabled`** | Credential identity and on/off. |
| **`temporary`**, **`expires_at`**, **`max_uses`**, **`use_count`** | Visitor/contractor lifecycle: temporary PINs need **`expires_at`** (RFC3339); optional **`max_uses`** caps successful grants. |
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

Civil dates are interpreted in **`device.access_exception_site_timezone`** (IANA, e.g. **`America/New_York`**); **empty** uses the system local timezone.

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
- **`acl exception import <calendar_id> <csv_path>`** — CSV: **`YYYY-MM-DD,full|early[,minute][,label]`**.

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

All keys accepted by **`cfg set`** are listed by **`cfg keys`** and documented in **`cfg keys`** output (same text as the in-program help block). Durations use Go syntax (e.g. **`10s`**, **`400ms`**, **`1m0s`**). Booleans: **`true`** / **`false`**. JSON splits values into **`device`** and **`gpio`** objects, but **`cfg set`** uses one **flat snake_case** namespace.

### 5.1 Logging, heartbeat, sounds

| Key | Purpose |
|-----|---------|
| **`log_level`** | **`debug`**, **`info`**, **`warning`**, **`error`**, **`critical`**, **`all`** (empty = all). |
| **`heartbeat_interval`** | Heartbeat webhook tick interval and internal debug cadence. |
| **`sound_card_name`** | ALSA device for WAV playback (e.g. **`plughw:0,0`**). |
| **`sound_startup`** / **`sound_shutdown`** / **`sound_pin_ok`** / **`sound_access_granted`** / **`sound_pin_reject`** / **`sound_keypress`** | WAV paths (empty skips when enabled). **`sound_access_granted`** is used for **REX / entry-button** style grants; **`sound_pin_ok`** for keypad PIN OK. |
| **`sound_lighting_timer_set`** / **`sound_lighting_timer_expired`** | Optional WAV when the lighting auto-off timer is (re)armed or when it expires. |
| **`sound_door_open`** | WAV on **first** **`door_open_timeout`** and each **repeat** while the door stays open (see §10). |
| **`sound_firemans_activated`** / **`sound_firemans_deactivated`** | Optional WAV when fireman's service is turned **on** / **off**. |
| **`sound_*_enabled`** | Per-sound master switches (**`true`** = WAV path may play; **`false`** = never play that cue). |

### 5.2 Door relay timing and auxiliary pulses

| Key | Purpose |
|-----|---------|
| **`relay_pulse_duration`** | Door strike pulse length after a normal access grant (and default for some auxiliary outputs). |
| **`automatic_door_operator_pulse_duration`** | Pulse for **`gpio.automatic_door_operator_relay_pin`**; **empty** or zero uses **`relay_pulse_duration`**. |
| **`intercom_camera_trigger_pulse_duration`** | Pulse for **`gpio.intercom_camera_trigger_relay_pin`**; **empty** or zero normalizes to **800ms** in software. |

Authorized keypad grants and **MQTT `open_door`** pulse the door for **`relay_pulse_duration`**, then pulse **automatic door operator** and **intercom/camera trigger** relays when those outputs are configured (suppressed during **fireman's service** where applicable).

### 5.3 Door sensor, door-open alarms, and accessibility

Requires **`gpio.door_sensor_pin`** non-zero and a configured polarity.

| Key | Purpose |
|-----|---------|
| **`door_sensor_closed_is_low`** | **true** = closed when GPIO reads low (typical pull-up to 3.3 V, contact to GND when closed). |
| **`door_open_warning_after`** | **Base** duration the door may stay open before the **first** **`door_open_timeout`** webhook. |
| **`door_open_alarm_interval`** | Interval between **repeat** **`door_open_timeout`** webhooks after the first (default **30s** if unset/zero after normalisation). |
| **`door_open_alarm_max_count`** | Maximum **`door_open_timeout`** events per **continuous** open period; **0 = unlimited**. |
| **`door_forced_after_warnings`** | After this many **`door_open_timeout`** events in **one** open period, emit **`door_forced`** once and **stop further door alarm webhooks** until the door has **closed and opened again**; **0 = never**. |

Per-PIN extra time before the first alarm: **`access_pins.door_hold_extra_seconds`** via **`acl pin hold_extra`** (§4.4–4.5). The effective first threshold is **`door_open_warning_after` + extra** for the open period following that PIN’s unlock.

### 5.4 PIN entry, lockout, fallback

| Key | Purpose |
|-----|---------|
| **`pin_length`** | Digits before auto-submit; **0** = require **Enter** / KP Enter to submit. |
| **`keypad_inter_digit_timeout`** | Max gap between digits (clamped in software, typically **3s–10s**). |
| **`keypad_session_timeout`** | Max time from first digit to submit/clear (clamped **10s–60s**). |
| **`pin_entry_feedback_delay`** | Pause after PIN OK/reject before accepting new keys (clamped **2s–10s**). |
| **`pin_lockout_enabled`** | Master switch for wrong-PIN lockout. |
| **`pin_lockout_after_attempts`** | Consecutive wrong PINs before lockout (**0** = off; else clamped **3–5**). |
| **`pin_lockout_duration`** | Keypad ignore duration after lockout (clamped **30s–300s**). |
| **`pin_lockout_override_pin`** | Submitted PIN clears lockout **without** opening the door. |
| **`pin_reject_buzzer_after_attempts`** | Wrong-PIN streak threshold for buzzer pulse (**0** = off). |
| **`buzzer_relay_pulse_duration`** | Buzzer relay pulse length. |
| **`fallback_access_pin`** | Accepted when no **`access_pins`** row matches (empty disables). |
| **`access_schedule_apply_to_fallback_pin`** | When **true**, fallback PIN is subject to schedules too. |

### 5.5 Lighting

| Key | Purpose |
|-----|---------|
| **`lighting_timeout`** | Hold time for **`gpio.lighting_relay_pin`** after a **successful keypad PIN** (non-empty PIN) or **`lighting_button`** press. Each new qualifying event **restarts the full timer** without pulsing the relay off mid-window. Default **30m** when unset/zero; clamped **5s–24h**. |
| **`lighting_button_pin`** / **`lighting_button_active_low`** | BCM manual lighting button (**0** = disabled). Requires both button and relay pins for button-driven lighting. |
| **`lighting_relay_pin`** / **`lighting_relay_active_low`** | Lighting load relay (**BCM** or expander index **0–15** per **`relay_output_mode`**). **0** = disabled. |

While **fireman's service** is active, the lighting relay is **held on** for emergency illumination and the normal auto-off timer is **not** armed for PIN events.

### 5.6 Fireman's service (emergency bypass)

| Key | Purpose |
|-----|---------|
| **`firemans_service_enabled`** | Master enable: GPIO edge, **MQTT**, and technician **`firemans`** commands are ignored when **false**. |
| **`firemans_service_input_pin`** / **`firemans_service_active_low`** | Optional **BCM** maintained input (**0** = GPIO control disabled; use MQTT/menu only). |

When active: **all access relays** are de-energized (except the **door** relay may be re-held if the **fire-alarm interface** demands fail-unlock), **elevator hoist outputs** stay off in software, **schedules and elevator floor ACL** are bypassed for valid credentials, **lighting** is forced **on** if configured, and **wrong-PIN buzzer** / **MQTT door open** are suppressed. See **`firemans_service_activated`** / **`firemans_service_deactivated`** webhooks (§12).

### 5.7 Fire-alarm interface (fail-unlock)

| Key | Purpose |
|-----|---------|
| **`fire_alarm_interface_pin`** / **`fire_alarm_interface_active_low`** | **BCM** opto input (**0** = disabled). When the interface reads **active**, the **door** relay is **held energized** until the input clears (independent of fireman's service, though fireman's bulk-off logic skips turning the door off while fail-unlock is latched). |

State transitions are logged (**INFO** / **DEBUG**). There is **no** dedicated **`fireEventWebhook`** name for this input today — integrate via logs or extend upstream if you need HTTP events.

### 5.8 Optional BCM inputs (tamper, motion)

| Key | Purpose |
|-----|---------|
| **`tamper_switch_pin`** / **`tamper_switch_active_low`** | Enclosure tamper; **both edges** logged at **DEBUG** with a derived **`enclosure_secure`** flag. |
| **`motion_sensor_pin`** / **`motion_sensor_active_low`** | PIR / presence style input; **assert** edge logged at **DEBUG**. |

These are **diagnostic inputs** (logging only); they do not currently emit webhook **`event`** names.

### 5.9 MQTT

| Key | Purpose |
|-----|---------|
| **`mqtt_enabled`** | Client on/off. |
| **`mqtt_broker`** | e.g. **`tcp://host:1883`**. |
| **`mqtt_client_id`** | Client id; copied to webhooks and **`logs.device_client_id`**. |
| **`mqtt_username`** / **`mqtt_password`** | Optional broker credentials. |
| **`mqtt_command_topic`** | Subscribe topic for remote commands (§9). |
| **`mqtt_status_topic`** | JSON command acknowledgements. |
| **`mqtt_command_token`** | If set, command payload must be JSON with matching **`token`**. |
| **`mqtt_pair_peer_topic`** | Pair-peer topic (§6.6). |
| **`pair_peer_role`** | **`none`** \| **`entry`** \| **`exit`**. |
| **`pair_peer_token`** | Optional secret in pair-peer JSON. |

### 5.10 Webhooks (scalars via `cfg set`; lists/maps in JSON)

| Key | Purpose |
|-----|---------|
| **`webhook_event_enabled`** | POST discrete events when **true** and URL(s) configured. |
| **`webhook_event_url`** | Legacy single event URL (used only if **`webhook_event_endpoints`** is empty). |
| **`webhook_event_token_enabled`** / **`webhook_event_token`** | Optional **Authorization: Bearer** for legacy URL. |
| **`webhook_heartbeat_enabled`** | POST heartbeats when **true** and URL set. |
| **`webhook_heartbeat_url`** | Heartbeat URL. |
| **`webhook_heartbeat_token_enabled`** / **`webhook_heartbeat_token`** | Bearer for heartbeat. |
| **`webhook_http_timeout`** | Per-request POST timeout (clamped **5s–120s**; default **25s**). |
| **`webhook_max_concurrent`** | Global cap on in-flight webhook HTTP requests (clamped **1–256**; default **16**). |
| **`webhook_circuit_breaker_enabled`** | When **true**, repeated failures open the breaker (default **true**). |
| **`webhook_circuit_failure_threshold`** | Consecutive failures (network/timeout or HTTP **5xx**) before open (clamped **1–100**; default **5**). |
| **`webhook_circuit_open_duration`** | How long outbound POSTs are rejected after open (clamped **1s–1h**; default **60s**). |

**In JSON only (not individual `cfg set` keys):**

- **`webhook_event_types`** — Global allowlist: if non-empty, only event names with value **true** are sent.
- **`webhook_event_endpoints`** — Array of **`{ enabled, url, token_enabled, token, event_types? }`**. If **non-empty**, it **replaces** **`webhook_event_url`** for event delivery until the list is cleared. Each endpoint may have its own **`event_types`** allowlist.

### 5.11 Operation mode and access binding

| Key | Purpose |
|-----|---------|
| **`keypad_operation_mode`** | §6. |
| **`keypad_evdev_path`** / **`keypad_exit_evdev_path`** | evdev devices (dual mode: must differ). |
| **`access_control_door_id`** | SQLite **`access_doors.id`** for this device. |
| **`access_control_elevator_id`** | SQLite **`access_elevators.id`** for elevator modes. |
| **`access_schedule_enforce`** | When **true** and id set, enforce levels + windows for that target. |
| **`access_exception_site_timezone`** | IANA zone for exception-calendar **civil** dates (§4.8). |
| **`tech_menu_history_max`** | Technician command history cap (**0** defaults to **100**; max **10000**). |
| **`tech_menu_prompt`** | Top-level JSON key (not under **`device`**); also settable via **`cfg set`**. |

### 5.12 Elevator-specific `device` keys

| Key | Purpose |
|-----|---------|
| **`elevator_floor_wait_timeout`** | Wait-floor grant window (**5s–600s**). |
| **`elevator_wait_floor_cab_sense`** | **`sense`** (default) or **`ignore`**. |
| **`elevator_floor_input_pins`** | Comma BCM list (**sense** mode). |
| **`elevator_predefined_floor`** / **`elevator_predefined_floors`** | Predefined-floor mode: **`elevator_predefined_floors`** is a **comma-separated list of integers** in JSON (at most one floor in current validation); **`elevator_predefined_floor`** is index or legacy label behaviour per code comments. |
| **`elevator_dispatch_pulse_duration`** | Default dispatch pulse. |
| **`elevator_floor_dispatch_pulse_durations`** | Comma-separated durations per dispatch index. |
| **`elevator_enable_pulse_duration`** | Predefined-floor enable pulse (wait-floor uses full timeout for enables). |
| **`dual_keypad_reject_exit_without_entry`** | Dual USB: reject exit without entry when **true**. |

### 5.13 GPIO (`gpio` section and matching `cfg set` names)

| Key | Purpose |
|-----|---------|
| **`relay_output_mode`** | **`gpio`** \| **`mcp23017`** \| **`xl9535`**. |
| **`mcp23017_i2c_bus`** / **`mcp23017_i2c_addr`** | MCP23017 I2C bus and 7-bit address. |
| **`xl9535_i2c_bus`** / **`xl9535_i2c_addr`** | XL9535 bus and address. |
| **`door_relay_pin`** / **`door_relay_active_low`** | Strike / door relay (BCM or expander index per mode). |
| **`buzzer_relay_pin`** / **`buzzer_relay_active_low`** | Wrong-PIN buzzer. |
| **`door_sensor_pin`** | Door position input (**BCM**; not on expander). |
| **`heartbeat_led_pin`** | Activity LED (**BCM**). |
| **`exit_button_pin`** / **`exit_button_active_low`** | REX (**BCM**). |
| **`entry_button_pin`** / **`entry_button_active_low`** | Entry request (**BCM**). |
| **`elevator_dispatch_relay_pin`** / **`elevator_dispatch_active_low`** | Shared dispatch when **`elevator_floor_dispatch_pins`** is empty (**0** = use **door** relay in elevator modes). |
| **`elevator_enable_relay_pin`** / **`elevator_enable_active_low`** | Legacy single wait-floor enable. |
| **`elevator_floor_dispatch_pins`** | Comma BCM or expander indices for per-floor dispatch. |
| **`elevator_wait_floor_enable_pins`** | Wait-floor “return ground” enables. |
| **`elevator_predefined_enable_pins`** | Predefined-floor call simulation. |
| **`automatic_door_operator_relay_pin`** / **`automatic_door_operator_relay_active_low`** | Optional second door-system relay (**BCM** or expander). |
| **`intercom_camera_trigger_relay_pin`** / **`intercom_camera_trigger_relay_active_low`** | Short pulse on authorized access. |

**Note:** Door sensor, heartbeat LED, exit/entry buttons, **lighting button**, **fireman's**, **fire-alarm**, **tamper**, **motion**, and **cab floor sense** inputs remain **SoC BCM GPIO**, not on the I2C expander. **Lighting relay**, **automatic door operator**, and **intercom trigger** may be on the expander when **`relay_output_mode`** is **`mcp23017`** or **`xl9535`**.

### 5.14 Top-level JSON (besides `device` / `gpio`)

| Key | Purpose |
|-----|---------|
| **`tech_menu_prompt`** | Short label on the technician prompt line. |
| **`elevator_parameter_modes`** | **Documentation only** — not read by control logic; preserved on **`cfg save`**. |

---

## 6. Operation modes (`device.keypad_operation_mode`)

Set **exactly one** of the following string values.

### 6.1 `access_entry` (default)

Single USB keypad on **`keypad_evdev_path`**. Valid PIN → pulse **door** relay (and optional aux relays §5.2).

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
| **Entry** | **`entry`** | Valid PIN → local door + publish JSON to **`mqtt_pair_peer_topic`**. |
| **Exit** | **`exit`** | Subscribes; on valid message, pulses local door. |

### 6.7 `elevator_wait_floor_buttons`

After valid PIN (and schedule checks if configured): per-floor or legacy enable relays, **`elevator_floor_wait_timeout`**, cab sense **`elevator_wait_floor_cab_sense`**, floor inputs and dispatch as documented in **`elevator_parameter_modes`** in JSON.

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
| **`event_name`** | e.g. **`pin_accepted`**, **`door_open_timeout`**, **`door_forced`**. |
| **`device_client_id`** | **`mqtt_client_id`** at insert time. |
| **`detail_json`** | JSON detail map (matches webhooks). |

---

## 9. MQTT

### 9.1 Remote commands (`mqtt_command_topic`)

Payload: plain text command or JSON **`{"cmd":"..."}`**. If **`mqtt_command_token`** is set, use JSON with **`"token"`**.

| Command aliases | Action |
|-----------------|--------|
| **`open_door`**, **`door_open`**, **`unlock`** | Pulse door relay; **`mqtt_remote_door_open`** (also pulses aux relays §5.2). **Denied** with ack error **`firemans_service_active`** → **`mqtt_remote_door_open_denied`** webhook. |
| **`firemans_service_on`**, **`firemans_on`**, **`emergency_bypass_on`** | Activate fireman's service (requires **`firemans_service_enabled`**). |
| **`firemans_service_off`**, **`firemans_off`**, **`emergency_bypass_off`** | Deactivate fireman's service. |
| **`firemans_service_status`**, **`firemans_status`** | Ack detail includes **`firemans_service_active=…`**. |
| **`buzzer`**, **`buzz`**, **`alarm`** | Buzzer relay; **`mqtt_remote_buzzer`** (held off during fireman's service). |
| **`door_status`**, **`status_door`** | Ack includes **`door_open`** if sensor configured. |
| **`ping`**, **`hello`** | Ack **`detail`**: **`pong`**. |

Acknowledgements on **`mqtt_status_topic`**: **`ok`**, **`cmd`**, optional **`error`**, **`detail`**, optional **`door_open`**.

### 9.2 Pair-peer (`mqtt_pair_peer_topic`)

JSON **`{"cmd":"pulse_paired_exit"}`** or **`unlock_peer_exit`**; optional **`token`**. Fires **`mqtt_pair_peer_exit_pulse`**.

---

## 10. Door sensor behaviour (summary)

1. **Transitions:** **`door_opened`** / **`door_closed`** webhooks (and audit) on edge; closing clears per-PIN **`door_hold_extra`** grace for the next cycle.
2. **First alarm:** when open duration exceeds **`door_open_warning_after` +** current **`door_hold_extra`** (from last credential grant that set it).
3. **Repeats:** **`door_open_timeout`** every **`door_open_alarm_interval`**, up to **`door_open_alarm_max_count`** (if non-zero).
4. **Forced:** at **`door_forced_after_warnings`** timeout count, **`door_forced`** fires; then **no** further **`door_open_timeout`** / **`door_forced`** until **close** then **open** again.
5. **`sound_door_open`** may play on first and repeat timeouts when enabled.
6. Payloads include fields such as **`warning_sequence`**, **`threshold_effective`**, **`door_hold_extra`**, **`forced_after_warnings`** where applicable.

---

## 11. HTTP listener

Server on **`:8080`** (**`net/http`** mux): **`GET /admin`** returns plain text (**“Local Configuration Interface”**); **`POST /api/remote-control`** returns JSON **`{"status":"door_opened"}`** as a **stub**. **`tokenAuthAPI`** is a placeholder wrapper (currently passes all requests through). Treat as **local/debug** unless firewalled.

---

## 12. HTTP webhooks and event names

- **Events:** **`type`**: **`event`**, **`event`**: `<name>`, **`timestamp`**, **`device_client_id`**, plus detail keys. No PIN digits.
- **Heartbeat:** **`type`**: **`heartbeat`**, **`heartbeat_interval`**, etc.

**Common event names** (for **`webhook_event_types`** / endpoint allowlists):

**`pin_accepted`**, **`pin_rejected`**, **`wrong_pin_buzzer`**, **`keypad_lockout_activated`**, **`keypad_lockout_override`**, **`door_opened`**, **`door_closed`**, **`door_open_timeout`**, **`door_forced`**, **`mqtt_remote_door_open`**, **`mqtt_remote_door_open_denied`**, **`mqtt_remote_buzzer`**, **`mqtt_pair_peer_exit_pulse`**, **`firemans_service_activated`**, **`firemans_service_deactivated`**, **`elevator_floor_denied`**, **`elevator_floor_selected`**, **`elevator_floor_timeout`**, and credential lifecycle / schedule reasons embedded in **`pin_rejected`** details.

---

## 13. Keypad device paths

Prefer **`/dev/input/by-id/`** or **`/dev/input/by-path/`**. Tool: **`go run ./tools/listkeypads`** (**`-usb`** for USB only; technician **`kb`** matches USB-only listing; **`kb all`** includes non-USB).

---

## 14. Troubleshooting

| Symptom | Checks |
|---------|--------|
| No keypad | **`keypad_evdev_path`**; **`listkeypads`** / **`kb`**; **`evtest`**. |
| Door never pulses | GPIO / expander mapping; **`relay_output_mode`**; **`relay_pulse_duration`**. |
| Door alarms wrong | **`door_sensor_closed_is_low`**; **`door_open_*`** timings; **`acl pin hold_extra`**. |
| No repeat webhooks | **`door_open_alarm_interval`**; **`door_open_alarm_max_count`** not already reached; webhook circuit breaker (§5.10). |
| **`door_forced` never fires** | **`door_forced_after_warnings`** > 0; allowlist includes **`door_forced`**. |
| Webhook missing | **`webhook_event_enabled`**; URL or **`webhook_event_endpoints`**; **`webhook_event_types`** not blocking the event name; breaker open (logs). |
| MQTT door open ignored | **`firemans_service_active`**; **`mqtt_remote_door_open_denied`** in logs/webhooks. |
| Pair exit dead | MQTT, **`pair_peer_role`**, topic, token, broker ACLs. |
| Elevator floor denied | SQLite floor ACL + **`access_elevator_floor_time_rules`**; **`access_control_elevator_id`**. |
| Schedule ignored | **`access_schedule_enforce`**; bind ids; **`acl target list`**; timezones. |
| Holiday wrong | **`access_exception_site_timezone`**; **`acl exception date list`**; **`respects_exceptions`** on profile. |
| **`acl` fails** | **`acl help`**; create entities in dependency order. |
| Lighting stuck / never off | **`lighting_timeout`**; fireman's service hold; relay pin mapping. |
| GPIO “stuck” after JSON edit | **`cfg restart`** or service restart after **`gpio`** / I2C changes. |
| Build errors | **`go build -o virtualkeyz2 ./cmd/virtualkeyz2`** from repo root. |

---

## 15. Related files

| File | Purpose |
|------|---------|
| **`virtualkeyz2.json`** | Main configuration |
| **`internal/app/`** | Main application (menu, **`acl`**, MQTT, GPIO, door monitor, lighting, fireman's service) |
| **`internal/access/`** | Exception calendar resolution and schedule helpers |
| **`access_control.db`** | SQLite (PINs, ACL, exceptions, **`logs`**) |
| **`changelog.txt`** | Release history |
| **`tools/bump-version.sh`** | Version bump |
| **`tools/listkeypads`** | evdev discovery |

---

*VirtualKeyz 2.x. For support, contact your integrator or project maintainer.*
