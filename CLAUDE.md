# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

VirtualKeyz2: PIN-based door/elevator access control for Raspberry Pi. USB keypads (evdev) and QR scanners feed PINs. Relays are driven via SoC GPIO (go-gpiocdev) or I2C expanders (MCP23017 / XL9535). SQLite holds ACLs and the audit log. It also does MQTT, webhooks, an HTTP listener, an HD44780 LCD and audio feedback. `OPERATOR.md` is the full operator/config reference (flags, every `device`/`gpio` JSON key, `cfg`/`acl` menu commands, operation modes, DB schema). Read the relevant section before changing behaviour.

## Commands

```bash
make build        # go build -o virtualkeyz2 ./cmd/virtualkeyz2
make test         # go test -count=1 ./...
make test-race
make vet
make lint         # golangci-lint (config: .golangci.yml)
make ci           # fmt vet test lint vuln

go test -count=1 -run TestName ./internal/access/   # single test
go test -tags integration ./internal/app/integration/  # build-sanity integration test (build tag)
go run ./tools/listkeypads                             # list evdev keypad devices
```

Module path is `virtualkeyz2` (imports look like `virtualkeyz2/internal/...`). Go 1.26. Cgo is required (`mattn/go-sqlite3`). `SKILL.md` is a "modern Go" guideline: use language and stdlib features up to Go 1.26 (`wg.Go`, `errors.AsType`, `new(expr)`, `strings.SplitSeq`, `omitzero`, `t.Context()`, etc.).

Run with `./virtualkeyz2 -config virtualkeyz2.json [-notechmenu] [-daemon]`. The process opens `access_control.db` in the **current working directory** (`store.DefaultDSN`). Hardware access (GPIO/I2C/evdev/audio) needs root or the matching groups (see `systemd/virtualkeyz2.service`). Off-Pi, GPIO init fails and `AppContext.GPIO` is nil, so code must tolerate that.

## Versioning

After each code change, run `./tools/bump-version.sh "description"`. It bumps `SoftwareVersion` / `SoftwareReleaseUTC` in `internal/app/virtualkeyz2.go` and prepends an entry to `changelog.txt`. Don't edit those constants by hand.

## Architecture

- `cmd/virtualkeyz2/main.go` only calls `app.Main()`.
- `internal/app` is the application, one package split by subsystem. `virtualkeyz2.go` holds the version constants, type aliases, `AppContext`, `newDefaultAppContext` (built-in defaults) and `Main`. The rest:
  - Config: `config_json.go` (JSON types, normalize/validate, `applyVirtualKeyz2JSON`), `config_persist.go` (`cfg save`), `config_live.go` (reload/apply/restart), `cfg_keys.go` (`cfg set` key table).
  - Input → decision: `keypad.go`, `qr.go`, `pin.go` (`processPIN`, grant/reject, lockout), `input_hold.go` (post-result input window, feedback sounds), `credentials.go`, `schedule.go` (+ `initAccessScheduleSchema`), `exception_calendar.go`, `occupancy.go`.
  - Outputs/hardware: `gpio.go` (`GPIOManager`, I2C recovery), `door.go`, `elevator.go`, `lighting.go`, `firemans_firealarm.go`, `sound.go`, `lcd_ui.go`.
  - Integration: `webhook.go` + `webhook_queue.go` (ordered per-endpoint workers), `audit_log.go` (batched async `logs` writer), `mqtt.go`, `http.go`, `logging.go`.
  - Technician menu: `techmenu.go`, `techmenu_cfg.go`, `techmenu_term.go`, `techmenu_acl.go`, `techmenu_netdiag.go`.
- `AppContext` is the central state struct passed everywhere. `configMu` guards `Config`/`GPIOSettings`/`TechMenuPrompt` (these change at runtime via `cfg set`/`cfg reload`). `mqttMu` guards the MQTT client. Take the RLock when reading config from goroutines.
- `internal/config`: `DeviceConfig`, `GPIOSettings`, and mode/relay-backend constants. These are re-exported as type aliases and constants at the top of `virtualkeyz2.go`.
- `internal/store`: opens SQLite (WAL) and runs embedded `migrations/*.sql` in filename order. The migrations are re-executed on every start, so they must be idempotent (`IF NOT EXISTS`). There are also ad-hoc column migrations (`migrate_pins.go`). The schedule/ACL tables are created in `internal/app`, not here.
- `internal/access`: exception-calendar / time helpers (pure and unit-tested).
- `internal/mcp23017`, `internal/xl9535`: I2C relay expander drivers behind the `i2cRelayExpander` interface. I2C ops are serialized by `GPIOManager.i2cOpsMu`, with bus recovery and reopen.
- `internal/keypadlist`, `internal/remotemqtt`, `internal/outputnames`: small helpers (evdev discovery, MQTT command payloads, elevator output names).

### Adding or changing a `device` config key

One key touches several places in `internal/app`, and all of them must be kept in sync:
1. Field in `config.DeviceConfig` (`internal/config/types.go`), plus a default in `newDefaultAppContext` (`virtualkeyz2.go`).
2. Pointer field in `virtualkeyz2DeviceJSON` and its overlay in `applyVirtualKeyz2JSON` (`config_json.go`, load/reload).
3. Field in `virtualkeyz2PersistDevice` and `buildPersistFile` (`config_persist.go`, `cfg save`).
4. An entry in `techMenuCfgKeys` (`internal/app/cfg_keys.go`; drives both `cfg set` and Tab completion), a line in `techMenuCfgKeysHelp` and a line in `techMenuShowConfig`.
5. Document it in `OPERATOR.md` §5.

Golden tests (`internal/app/config_golden_test.go`, `testdata/*.golden*`) lock down load/save, `cfg list`, `cfg keys` and `cfg set` behaviour. After an intended change, regenerate with `go test ./internal/app/ -run Golden -update` and review the diff.

GPIO pin / `relay_output_mode` / I2C changes only take effect after a full restart (`cfg restart` re-execs the binary). `cfg apply`/`cfg reload` do not re-run hardware setup.

### Concurrency conventions

The keypad input path must never block. After a result, `pin_entry_feedback_delay` is a per-source input-ignore window (`holdInput`/`inputHeld` in `input_hold.go`), not a sleep; feedback sounds go through `ctx.playFeedbackSound` / `playOKSound` / `playRejectSound`. Use `debugf` for DEBUG lines on hot paths. Display/LCD/webhook notifications use buffered channels with non-blocking `select` sends, or run async (e.g. `notifyPinDisplay`, `lcdEnqueueFull`, `postEventWebhook`). The remote framebuffer display (fb-virtualkeyz2) is driven by `pin_progress` / `keypad_session_end` webhook events.
