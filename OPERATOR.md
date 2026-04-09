# VirtualKeyz2 — Operator Guide

This document is for installers and operators who configure and run the service on a Raspberry Pi (or similar Linux host). It describes behaviour, configuration keys, and practical wiring notes. Authoritative JSON field names match `virtualkeyz2.json`.

---

## 1. Configuration file and service

- **Config path:** passed with `-config` (default `virtualkeyz2.json` in the working directory).
- **Apply changes:** edit JSON, then either **restart the service** or use the technician menu (`cfg reload` loads from disk; `cfg apply` refreshes in-memory items such as MQTT and log level).
- **GPIO pin map:** changing BCM pin numbers in JSON **requires a process restart** to take effect; `cfg apply` does not re-initialise hardware mapping.

---

## 2. Software build version

- On startup the log line includes the **build version** and **release timestamp** (UTC).
- From the technician menu: **`v`** shows version and release; **`ch`** prints `changelog.txt` if found (next to the binary, current working directory, or project root).
- Developers bump version and changelog with `./tools/bump-version.sh "description"`.

---

## 3. Technician menu (console / `/dev/tty`)

When the process has a TTY, a bottom-line prompt appears. Useful keys:

| Key | Action |
|-----|--------|
| `h` | Main menu help |
| `1` / `cfg list` | Full configuration (sensitive tokens shown as `(set)`) |
| `v` | Software build version and release |
| `ch` | Changelog text |
| `i` | Network snapshot (Ethernet / Wi‑Fi, DNS, gateways) |
| `p` | System-wide listening TCP/UDP ports |
| `occ` | Dual USB mode: show in-memory area occupancy (masked PINs + `access_pins` labels) |
| `kb` / `kb all` | List **by-id/by-path** USE_PATH values (`kb` = USB only; `kb all` = include non-USB) |
| `cfg set <key> <value>` | Change one setting (then `cfg apply` / `cfg save` as needed) |
| `cfg keys` | All settable keys (same names as JSON) |
| `...` | Shutdown request (same path as SIGTERM) |

---

## 4. Operation modes (`device.keypad_operation_mode`)

Set **exactly one** of the following string values.

### 4.1 `access_entry` (default)

- Single USB keypad.
- Valid PIN → pulse **door** relay (`gpio.door_relay_*`).

### 4.2 `access_exit`

- Same logic as **entry**; use this value when the keypad and relay are wired for an **exit** door or strike. Software behaviour is identical to `access_entry`; documentation and webhooks distinguish the mode.

### 4.3 `access_entry_with_exit_button`

- Keypad for **entry** (valid PIN → door pulse).
- **Free egress:** momentary input on **`gpio.exit_button_pin`** also pulses the door relay.
- **`gpio.exit_button_active_low`:** `true` if the contact pulls the pin to ground when pressed (typical with internal pull-up).

### 4.4 `access_exit_with_entry_button`

- Keypad used at **exit** side (valid PIN → door pulse).
- **`gpio.entry_button_pin`:** additional input to pulse the door (e.g. inside “request entry”).
- **`gpio.entry_button_active_low`:** same convention as exit button.

### 4.5 `access_dual_usb_keypad`

- Two USB keypads on different evdev nodes:
  - **`device.keypad_evdev_path`** — **entry** keypad.
  - **`device.keypad_exit_evdev_path`** — **exit** keypad (must differ from entry path).
- **Credentials:** valid PINs are loaded from SQLite table **`access_pins`** (`pin`, optional `label`, `enabled`). If no row matches, the built-in test PIN `123456` still works for lab use (`legacy_or_unlabeled` in logs). Add rows with `sqlite3 access_control.db` or your own tool.
- **Logging:** every keypad line identifies the side (`entry` / `exit` / `single`) — keypress DEBUG lines, PIN submit, timeouts, accept/reject, and lockout messages.
- **Area occupancy (in memory, until restart):** a successful PIN at the **entry** keypad increments that credential’s “inside” count and the **total people** in the zone. The **same** PIN at the **exit** keypad decrements it. If someone exits without a matching entry, behaviour depends on **`device.dual_keypad_reject_exit_without_entry`:** when **false** (default), a **WARNING** is logged, the door still opens, and webhooks include `occupancy_mismatch` on `pin_accepted`. When **true**, the exit attempt is treated as a **failed access** (reject sound, wrong-PIN streak / lockout rules, **no** door pulse); webhook `pin_rejected` with reason `exit_without_recorded_entry`.
- **Technician `occ`:** prints current totals, masked PIN tails, and labels from `access_pins` (useful for reconciling counts).
- **Webhooks** (`pin_accepted`): include `keypad_role`, `credential_label`, and for dual entry/exit also `access_area_occupancy_total`, `credential_inside_count`, and `occupancy_mismatch` when applicable.
- Discover evdev nodes on the Pi with `cat /proc/bus/input/devices` or `evtest`.

### 4.6 `access_paired_remote_exit`

Two controllers share MQTT:

| Unit | `device.pair_peer_role` | Behaviour |
|------|-------------------------|-----------|
| **Entry** | `entry` | Valid PIN → pulse **local** door + publish JSON to **`device.mqtt_pair_peer_topic`**. |
| **Exit** | `exit` | Subscribes to **`device.mqtt_pair_peer_topic`**; on valid message, pulses **local** door. |

Requirements:

- **`device.mqtt_enabled`** true and broker reachable.
- **Payload** (JSON): `{"cmd":"pulse_paired_exit"}` or `{"cmd":"unlock_peer_exit"}`. Optional `"token"` must match **`device.pair_peer_token`** when that token is non-empty.

Local PIN on the exit unit still pulses that unit’s door (same as other access modes).

### 4.7 `elevator_wait_floor_buttons`

After a **valid PIN**:

1. If **`gpio.elevator_enable_relay_pin`** is non-zero, the **elevator_enable** output is turned **ON** for the wait window.
2. For up to **`device.elevator_floor_wait_timeout`**, the software watches **`device.elevator_floor_input_pins`** (comma-separated BCM numbers, e.g. `17,27,22`).
3. Inputs are **active low** (pulled high; floor contact pulls to ground when “pressed”).
4. On the **first** active floor input, the wait ends, enable output turns **OFF**, and a **dispatch** pulse is issued:
   - **`gpio.elevator_dispatch_relay_pin`** if non-zero, else the **door** relay.
5. Pulse length: **`device.elevator_dispatch_pulse_duration`** (falls back to door pulse duration if unset).

If the timeout expires, the grant clears and a warning is logged (and event webhook if configured).

### 4.8 `elevator_predefined_floor`

- After valid PIN → **dispatch** pulse only (same relay selection as above).
- **`device.elevator_predefined_floor`** is a **logical floor number** for logs and webhooks; it does not select different GPIO patterns. Use external hardware or future extensions for per-floor relay matrices.

---

## 5. Keypad device paths

| Key | Meaning |
|-----|---------|
| `device.keypad_evdev_path` | Primary keypad. Prefer a **stable** path from `/dev/input/by-id/` or `/dev/input/by-path/` (see §5.1); bare `/dev/input/eventN` can change after reboot. |
| `device.keypad_exit_evdev_path` | Second keypad for `access_dual_usb_keypad` only (same stable-path rule). |

### 5.1 Installer tool: `listkeypads`

Shipped source includes a small Linux utility that lists **`/dev/input/by-id/...`** and **`/dev/input/by-path/...`** symlinks resolved to the real **`/dev/input/eventN`** node. **Use the `USE_PATH` column in JSON** — those names stay tied to the same USB device or port across reboots. Dynamic `event` numbers are shown only as **BACKEND_EVENT** for debugging.

From the project directory on the Pi (or any Linux host):

```bash
go run ./tools/listkeypads
go run ./tools/listkeypads -usb
go build -o listkeypads ./tools/listkeypads && sudo install -m755 listkeypads /usr/local/bin/
```

- **Default:** all suitable symlink targets (mouse-only `-event-mouse` links are skipped).
- **`-usb`:** only devices whose sysfs **Phys** starts with `usb-` or whose symlink path looks like a USB topology (`-usb-` / `usb-`).

If udev did not create a by-id/by-path link for a node, the tool still lists **`/dev/input/eventN`** with a note that the number **may change on reboot** — prefer another interface on the same gadget that has a stable symlink, or fix udev rules.

Use **`sudo evtest <USE_PATH>`** to confirm which physical keypad is which.

The same table is available from the running service technician menu as **`kb`** (USB-only) or **`kb all`** (full list).

---

## 6. GPIO summary (`gpio` section)

| Field | Typical use |
|-------|-------------|
| `door_relay_pin` / `door_relay_active_low` | Main strike or door relay. |
| `buzzer_relay_pin` | Wrong-PIN buzzer (per `pin_reject_buzzer_after_attempts`). |
| `door_sensor_pin` | Door position (with `door_sensor_closed_is_low`). |
| `heartbeat_led_pin` | Blinking activity LED. |
| `exit_button_pin` | REX / exit button (`access_entry_with_exit_button`). |
| `entry_button_pin` | Entry request (`access_exit_with_entry_button`). |
| `elevator_dispatch_relay_pin` | Elevator dispatch (0 = use door relay in elevator modes). |
| `elevator_enable_relay_pin` | Optional “enable cab buttons” hold (`elevator_wait_floor_buttons`). |

BCM numbering is **Broadcom**, not physical pin order.

---

## 7. MQTT (remote commands)

- **Command topic:** `device.mqtt_command_topic` — JSON `{"cmd":"open_door"}` style commands (see logs / code for full list).
- **Token:** If `device.mqtt_command_token` is set, commands must be JSON and include matching `"token"`.
- **Status topic:** `device.mqtt_status_topic` — acknowledgements published when configured.

Pair-peer topic is separate: **`device.mqtt_pair_peer_topic`** (Section 4.6).

---

## 8. HTTP webhooks

When enabled, the service POSTs JSON to configured URLs:

- **Events** (`device.webhook_event_*`): PIN, door, MQTT, elevator, and GPIO REX events (no PIN digits in payload).
- **Heartbeat** (`device.webhook_heartbeat_*`): once per `heartbeat_interval`.

Optional **Bearer** token when `*_token_enabled` is true and token string is non-empty.

---

## 9. Keypad lockout and override

- **`device.pin_lockout_enabled`** — master switch for lockout.
- **`device.pin_lockout_after_attempts`** / **`device.pin_lockout_duration`** — threshold and lockout length.
- **`device.pin_lockout_override_pin`** — if set, entering this PIN clears lockout and wrong-PIN streak **without** opening the door.

---

## 10. Troubleshooting

| Symptom | Checks |
|---------|--------|
| No keypad response | Wrong `keypad_evdev_path`; run `go run ./tools/listkeypads -usb`, copy a **USE_PATH** from by-id/by-path, then `sudo evtest <USE_PATH>`. Dual mode: two distinct stable paths. |
| Door never pulses | GPIO not available (not on Pi / `rpio` failed); relay pin and `active_low` polarity. |
| Pair exit never opens | MQTT connected; exit unit `pair_peer_role` = `exit`; same topic and token; broker ACLs. |
| Elevator wait never fires dispatch | `elevator_floor_input_pins` correct BCM list; wiring active-low; timeout not too short. |
| Config “stuck” after edit | Use `cfg reload` or restart; GPIO changes need **restart**. |

---

## 11. Related files

| File | Purpose |
|------|---------|
| `virtualkeyz2.json` | Main configuration |
| `changelog.txt` | Human-readable change history |
| `tools/bump-version.sh` | Version + changelog bump after code changes |
| `tools/listkeypads` | List **by-id / by-path** stable paths (+ resolved event node) for keypad wiring |

---

*Product line: VirtualKeyz 2.x. For technical support, refer to your integrator or project maintainer.*
