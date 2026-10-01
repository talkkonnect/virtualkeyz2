package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/jmoiron/sqlx"
	_ "github.com/mattn/go-sqlite3"

	"virtualkeyz2/internal/config"
	"virtualkeyz2/internal/mcp23017"
	"virtualkeyz2/internal/outputnames"
	"virtualkeyz2/internal/xl9535"
)

// Software build metadata — updated by ./tools/bump-version.sh after each documented revision.
const (
	SoftwareVersion    = "0.30"
	SoftwareReleaseUTC = "2026-10-01T07:26:57Z"
)

// Config types and mode constants (see internal/config).
type (
	DeviceConfig         = config.DeviceConfig
	GPIOSettings         = config.GPIOSettings
	WebhookEventEndpoint = config.WebhookEventEndpoint
	LCDDisplaySettings   = config.LCDDisplaySettings
)

const (
	ModeAccessEntry                 = config.ModeAccessEntry
	ModeAccessExit                  = config.ModeAccessExit
	ModeAccessEntryWithExitButton   = config.ModeAccessEntryWithExitButton
	ModeAccessExitWithEntryButton   = config.ModeAccessExitWithEntryButton
	ModeAccessDualUSBKeypad         = config.ModeAccessDualUSBKeypad
	ModeAccessPairedRemoteExit      = config.ModeAccessPairedRemoteExit
	ModeElevatorWaitFloorButtons    = config.ModeElevatorWaitFloorButtons
	ModeElevatorPredefinedFloor     = config.ModeElevatorPredefinedFloor
	RelayOutputGPIO                 = config.RelayOutputGPIO
	RelayOutputMCP23017             = config.RelayOutputMCP23017
	RelayOutputXL9535               = config.RelayOutputXL9535
	PairPeerRoleNone                = config.PairPeerRoleNone
	PairPeerRoleEntry               = config.PairPeerRoleEntry
	PairPeerRoleExit                = config.PairPeerRoleExit
	ElevatorWaitFloorCabSenseSense  = config.ElevatorWaitFloorCabSenseSense
	ElevatorWaitFloorCabSenseIgnore = config.ElevatorWaitFloorCabSenseIgnore
)

func NormalizeKeypadOperationMode(s string) string { return config.NormalizeKeypadOperationMode(s) }

func normalizePairPeerRole(s string) string { return config.NormalizePairPeerRole(s) }

func isDualUSBKeypadMode(mode string) bool { return config.IsDualUSBKeypadMode(mode) }

func modeUsesExitGPIOButton(mode string) bool { return config.ModeUsesExitGPIOButton(mode) }

func modeUsesEntryGPIOButton(mode string) bool { return config.ModeUsesEntryGPIOButton(mode) }

func isElevatorWaitFloorMode(mode string) bool { return config.IsElevatorWaitFloorMode(mode) }

func isElevatorKeypadMode(mode string) bool { return config.IsElevatorKeypadMode(mode) }

func normalizeElevatorWaitFloorCabSense(s string) string {
	return config.NormalizeElevatorWaitFloorCabSense(s)
}

func elevatorWaitFloorSenseCabInputs(cfg DeviceConfig) bool {
	return config.ElevatorWaitFloorSenseCabInputs(cfg)
}

func pairedEntryPublishesToPeer(mode, pairRole string) bool {
	return config.PairedEntryPublishesToPeer(mode, pairRole)
}

func pairedExitSubscribesToPeer(mode, pairRole string) bool {
	return config.PairedExitSubscribesToPeer(mode, pairRole)
}

// elevatorWaitFloorEnableChannelCount is the number of independent enable outputs (per-floor list or one legacy relay).
func elevatorWaitFloorEnableChannelCount(app *AppContext) int {
	if n := len(app.elevatorWaitFloorEnablePins); n > 0 {
		return n
	}
	if app.GPIOSettings.ElevatorEnableRelayPin != 0 {
		return 1
	}
	return 0
}

// AppContext holds our global connections and configurations
type AppContext struct {
	// RootCtx is cancelled when the process begins shutdown (HTTP drain, etc.).
	RootCtx      context.Context
	DB           *sqlx.DB
	MQTTClient   mqtt.Client
	mqttMu       sync.RWMutex // serializes reconnect vs publish/handler client reads
	Config       DeviceConfig
	configMu     sync.RWMutex // protects Config, GPIOSettings, TechMenuPrompt for reload/set/save
	GPIO         *GPIOManager // nil if GPIO character device failed to open (e.g. not on a Pi)
	GPIOSettings GPIOSettings
	// TechMenuPrompt is the technician /dev/tty status-line label, shown as "{TechMenuPrompt}> " before input.
	TechMenuPrompt string
	// ConfigPath is the JSON path from -config; used for cfg save/reload from the technician menu.
	ConfigPath string
	// PinDisplayDigits receives how many PIN digits are entered; displayController prints that many asterisks.
	// Buffered (see pinDisplayDigitsBuffer); notifyPinDisplay uses a non-blocking send so a stuck display goroutine cannot block keypad input.
	PinDisplayDigits chan int
	// lcdUI carries full-screen LCD updates; non-blocking enqueue from lcdEnqueueFull (same buffer policy as PinDisplayDigits).
	lcdUI chan lcdCmd

	inputHold inputHolds // per-source post-result input hold (input_hold.go)

	pinFailMu  sync.Mutex
	pinFailSeq int // consecutive rejected PIN submissions (reset on success or after buzzer fires)

	techHistMu sync.Mutex
	techHist   []string // technician /dev/tty commands, oldest first (capped by TechMenuHistoryMax)

	keypadLockoutMu         sync.Mutex
	keypadLockoutUntil      time.Time   // zero = no active lockout
	keypadLockoutEndTimer   *time.Timer // wall-clock end of lockout period
	keypadLockoutEndLogOnce *sync.Once  // ensures single WARNING when lockout period ends

	qrRejectMu       sync.Mutex
	qrRejectPayload  string    // last rejected QR payload; repeats are suppressed (see qrRejectRepeatQuietPeriod)
	qrRejectLastSeen time.Time // last time qrRejectPayload was read (rejected or suppressed)

	elevatorMu                   sync.Mutex
	elevatorGrantUntil           time.Time // non-zero: waiting for cab floor button (elevator_wait_floor_buttons)
	elevatorGrantStartedAt       time.Time // when the current grant began; used for cab-sense arming/debounce
	elevatorCabFloorDebounceHeld []int     // pressed indices seen while accumulating elevatorCabFloorDebounceTick
	elevatorCabFloorDebounceTick int       // consecutive 50ms polls with same held snapshot
	elevatorGrantPIN             string    // credential used for current wait-floor grant (for DB floor ACL)
	elevatorGrantViaFallback     bool
	// elevatorStaticTestFloorACLBypass: set during static-test QR elevator grant so floor list ACL is skipped (cleared in clearElevatorGrantState).
	elevatorStaticTestFloorACLBypass atomic.Bool

	// Dual USB keypad: per-PIN / per–secure-zone "inside" counts (entry +1, exit −1). When DB is nil, each PIN uses an atomic counter in occupancyCounters. When DB is open, counts live in SQLite dual_keypad_zone_occupancy.
	occupancyCounters sync.Map // string (PIN) -> *atomic.Int32; used only when ctx.DB == nil

	// elevatorFloorDispatchPins: parsed from GPIOSettings.ElevatorFloorDispatchPins; len matches cab floor inputs when non-empty.
	elevatorFloorDispatchPins []uint8
	// elevatorPredefinedEnablePins: parsed from GPIOSettings.ElevatorPredefinedEnablePins; at most one pin in predefined mode.
	elevatorPredefinedEnablePins []uint8
	// elevatorWaitFloorEnablePins: parsed from GPIOSettings.ElevatorWaitFloorEnablePins; len matches cab floor inputs when set.
	elevatorWaitFloorEnablePins []uint8
	// elevatorParameterModesDoc: optional JSON subtree from elevator_parameter_modes; documentation only, preserved on cfg save.
	elevatorParameterModesDoc json.RawMessage

	// doorAlarmMu protects doorHoldExtraGrace (per-credential extra time before first door-open alarm).
	doorAlarmMu        sync.Mutex
	doorHoldExtraGrace time.Duration

	// Lighting control (gpio lighting_* pins + device.lighting_timeout): manual lighting button and valid PIN (default access modes); timer reload does not de-energize the relay.
	lightingMu       sync.Mutex
	lightingTimerGen uint64
	lightingOffTimer *time.Timer

	// Fireman's service / emergency bypass (fail-safe): when active, all relay outputs are held off, lighting relay held on (if configured),
	// access schedules and elevator floor rules are bypassed for valid credentials, and elevator software does not energize hoist relays.
	firemansMu     sync.RWMutex
	firemansActive bool

	// Fire alarm interface (fail-safe unlock): when active, main door relay is held energized until the alarm input clears.
	fireAlarmMu     sync.RWMutex
	fireAlarmActive bool

	// Outbound event/heartbeat webhook HTTP: concurrency cap + optional circuit breaker (failure-driven open state).
	webhookOutbound webhookOutboundGuard
}

// WrongPINCount returns consecutive rejected PIN submissions (for technician diagnostics).
func (ctx *AppContext) WrongPINCount() int {
	ctx.pinFailMu.Lock()
	defer ctx.pinFailMu.Unlock()
	return ctx.pinFailSeq
}

// ResetWrongPINCount clears the consecutive wrong-PIN counter.
func (ctx *AppContext) ResetWrongPINCount() {
	ctx.pinFailMu.Lock()
	defer ctx.pinFailMu.Unlock()
	ctx.pinFailSeq = 0
}

// newDefaultAppContext returns an AppContext holding the built-in configuration defaults
// (before the -config JSON overlay).
func newDefaultAppContext(rootCtx context.Context, db *sqlx.DB) *AppContext {
	return &AppContext{
		RootCtx: rootCtx,
		DB:      db,
		Config: DeviceConfig{
			HeartbeatInterval:                60 * time.Second,
			DoorOpenWarningAfter:             10 * time.Second,
			DoorOpenAlarmInterval:            30 * time.Second,
			DoorOpenAlarmMaxCount:            0,
			DoorForcedAfterWarnings:          0,
			DoorSensorClosedIsLow:            true,
			PinLength:                        6,
			RelayPulseDuration:               400 * time.Millisecond,
			PinRejectBuzzerAfterAttempts:     3,
			BuzzerRelayPulseDuration:         800 * time.Millisecond,
			SoundCardName:                    "plughw:1,0",
			SoundStartup:                     "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/startup.wav",
			SoundShutdown:                    "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/shutdown.wav",
			SoundPinOK:                       "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/pin_ok.wav",
			SoundAccessGranted:               "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/access_granted.wav",
			SoundPinReject:                   "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/pin_reject.wav",
			SoundKeypress:                    "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/key.wav",
			SoundDoorOpen:                    "/home/talkkonnect/gocode/src/github.com/virtualkeyz2/sounds/door_open.wav",
			SoundStartupEnabled:              true,
			SoundShutdownEnabled:             true,
			SoundPinOKEnabled:                true,
			SoundAccessGrantedEnabled:        true,
			SoundPinRejectEnabled:            true,
			SoundKeypressEnabled:             true,
			SoundLightingTimerSetEnabled:     true,
			SoundLightingTimerExpiredEnabled: true,
			SoundDoorOpenEnabled:             true,
			// Blocking (sync) by default for user-facing PIN feedback so the tone
			// finishes before the flow continues; other sounds default to
			// non-blocking (async) via their zero value.
			SoundShutdownBlocking:          true,
			SoundPinOKBlocking:             true,
			SoundAccessGrantedBlocking:     true,
			SoundPinRejectBlocking:         true,
			MQTTEnabled:                    true,
			MQTTBroker:                     "tcp://central-mqtt-server:1883",
			MQTTClientID:                   "virtualkeyz2-pi-001",
			MQTTCommandTopic:               "virtualkeyz2/commands",
			MQTTStatusTopic:                "virtualkeyz2/status",
			TechMenuHistoryMax:             100,
			KeypadInterDigitTimeout:        5 * time.Second,
			KeypadSessionTimeout:           30 * time.Second,
			PinEntryFeedbackDelay:          3 * time.Second,
			PinLockoutEnabled:              true,
			PinLockoutAfterAttempts:        5,
			PinLockoutDuration:             60 * time.Second,
			PinLockoutOverridePin:          "",
			FallbackAccessPin:              "",
			WebhookEventEnabled:            false,
			WebhookEventTokenEnabled:       false,
			WebhookHeartbeatEnabled:        false,
			WebhookHeartbeatTokenEnabled:   false,
			WebhookHTTPTimeout:             25 * time.Second,
			WebhookMaxConcurrent:           16,
			WebhookCircuitBreakerEnabled:   true,
			WebhookCircuitFailureThreshold: 5,
			WebhookCircuitOpenDuration:     60 * time.Second,
			AccessScheduleEnforce:          true,
			KeypadOperationMode:            ModeAccessEntry,
			KeypadEvdevPath:                "/dev/input/event1",
			ScannerDevicePath:              "/dev/input/by-id/usb-YUREN_Yuren_HID_FS_Keyboard_SN_20190000-event-kbd",
			ScannerEnabled:                 true,
			MaxDevicesPerUser:              3,
			QRTimeWindowSeconds:            30,
			StaticTestQRCodeEnabled:        false,
			LCDDisplay: LCDDisplaySettings{
				Enabled:          false,
				I2CBus:           1,
				I2CAddr:          0x27,
				BacklightTimeout: 60 * time.Second,
				I2CDebugEnabled:  false,
			},
		},
		GPIOSettings: GPIOSettings{
			RelayOutputMode:      RelayOutputGPIO,
			MCP23017I2CBus:       1,
			MCP23017I2CAddr:      0x20,
			XL9535I2CBus:         1,
			XL9535I2CAddr:        0x20,
			DoorRelayPin:         5,
			DoorRelayActiveLow:   false,
			BuzzerRelayPin:       10,
			BuzzerRelayActiveLow: false,
			DoorSensorPin:        7,
			HeartbeatLEDPin:      26,
		},
		PinDisplayDigits: make(chan int, pinDisplayDigitsBuffer),
		lcdUI:            make(chan lcdCmd, lcdCmdBuffer),
		TechMenuPrompt:   "MeSpace-Siam-5th-Floor-Right-Door",
	}
}

// Main is the application entrypoint (called from cmd/virtualkeyz2).
func Main() int {
	// 1. Run Mode Configuration (Foreground vs Daemon) [cite: 8]
	daemonMode := flag.Bool("daemon", false, "Run system as a background daemon")
	noTechMenu := flag.Bool("notechmenu", false, "Disable interactive technician debug menu on /dev/tty")
	configPath := flag.String("config", "virtualkeyz2.json", "Path to JSON configuration file (optional; defaults used if missing)")
	flag.Parse()

	rootCtx, rootStop := context.WithCancel(context.Background())
	defer rootStop()

	if *daemonMode {
		fmt.Println("Starting in Daemon Mode...")
		// In a production environment, you would handle systemd integration here
	}

	// 2. Initialize Logging Levels (Info, Debug, Warning, Critical) [cite: 9]
	initLogger()
	slog.Info("VirtualKeyz2 starting", "version", SoftwareVersion, "release", SoftwareReleaseUTC)
	log.Printf("INFO: VirtualKeyz2 software build %s (release %s).", SoftwareVersion, SoftwareReleaseUTC)

	// 3. Initialize Local SQLite Database
	db := initDatabase()
	defer db.Close()

	appCtx := newDefaultAppContext(rootCtx, db)
	if err := loadVirtualKeyz2Config(*configPath, appCtx); err != nil {
		releaseStartupLogBuffer(os.Stdout)
		log.Fatalf("CRITICAL: configuration: %v", err)
	}
	normalizeKeypadAndPinUX(&appCtx.Config)
	appCtx.ConfigPath = *configPath
	syncLogFilterFromConfigLevel(appCtx.Config.LogLevel)
	registerTechMenuPrompt(appCtx.TechMenuPrompt)
	log.Printf("INFO: Keypad operation mode: %s", NormalizeKeypadOperationMode(appCtx.Config.KeypadOperationMode))
	log.Printf("INFO: Relay output mode: %s", normalizeRelayOutputMode(appCtx.GPIOSettings.RelayOutputMode))

	// 4. Initialize Hardware IO (GPIO, Relays, Heartbeat LED) [cite: 1, 3]
	err := openGPIOController()
	if err != nil {
		log.Printf("WARNING: Cannot open GPIO (Not running on Pi?): %v", err)
	} else {
		go manageHardwareHeartbeat(appCtx.GPIOSettings.HeartbeatLEDPin)
		gpio := NewGPIOManager()
		defer gpio.CloseI2CRelayForShutdown()
		relayI2CMode := isRelayOutputI2CExpander(appCtx.GPIOSettings.RelayOutputMode)
		useI2CExpander := false
		relayOutMode := normalizeRelayOutputMode(appCtx.GPIOSettings.RelayOutputMode)
		if relayI2CMode {
			switch relayOutMode {
			case RelayOutputMCP23017:
				bus := appCtx.GPIOSettings.MCP23017I2CBus
				addr := appCtx.GPIOSettings.MCP23017I2CAddr
				mcpDev, mcpErr := mcp23017.Open(bus, addr)
				if mcpErr != nil {
					log.Printf("WARNING: MCP23017 relay backend (%s / 0x%02x): %v", fmt.Sprintf("/dev/i2c-%d", bus), addr, mcpErr)
					log.Println("WARNING: Relay outputs disabled (mcp23017 mode but expander not available; pins 0-15 are not valid BCM numbers).")
				} else {
					gpio.SetI2CRelayExpander(mcpDev)
					gpio.SetI2CRelayBusRecovery(func() (i2cRelayExpander, error) {
						return mcp23017.Open(bus, addr)
					}, appCtx.GPIOSettings.I2CBusRecoverySCLBCM)
					useI2CExpander = true
					log.Printf("INFO: Relay outputs on MCP23017 bus %d address 0x%02x (pins 0-15 = GPA0..GPB7).", bus, addr)
				}
			case RelayOutputXL9535:
				bus := appCtx.GPIOSettings.XL9535I2CBus
				addr := appCtx.GPIOSettings.XL9535I2CAddr
				xlDev, xlErr := xl9535.Open(bus, addr)
				if xlErr != nil {
					log.Printf("WARNING: XL9535 relay backend (%s / 0x%02x): %v", fmt.Sprintf("/dev/i2c-%d", bus), addr, xlErr)
					log.Println("WARNING: Relay outputs disabled (xl9535 mode but expander not available; pins 0-15 are not valid BCM numbers).")
				} else {
					gpio.SetI2CRelayExpander(xlDev)
					gpio.SetI2CRelayBusRecovery(func() (i2cRelayExpander, error) {
						return xl9535.Open(bus, addr)
					}, appCtx.GPIOSettings.I2CBusRecoverySCLBCM)
					useI2CExpander = true
					log.Printf("INFO: Relay outputs on XL9535 bus %d address 0x%02x (pins 0-7 = port0, 8-15 = port1).", bus, addr)
				}
			}
		}
		if !relayI2CMode || useI2CExpander {
			gpio.AddOutput("door", appCtx.GPIOSettings.DoorRelayPin, appCtx.GPIOSettings.DoorRelayActiveLow, useI2CExpander)
			gpio.AddOutput("buzzer", appCtx.GPIOSettings.BuzzerRelayPin, appCtx.GPIOSettings.BuzzerRelayActiveLow, useI2CExpander)
			if appCtx.GPIOSettings.LightingRelayPin != 0 {
				gpio.AddOutput("lighting", appCtx.GPIOSettings.LightingRelayPin, appCtx.GPIOSettings.LightingRelayActiveLow, useI2CExpander)
			}
			if len(appCtx.elevatorFloorDispatchPins) > 0 {
				for i, pin := range appCtx.elevatorFloorDispatchPins {
					gpio.AddOutput(outputnames.ElevatorFloorDispatch(i), pin, appCtx.GPIOSettings.ElevatorDispatchActiveLow, useI2CExpander)
				}
			} else if appCtx.GPIOSettings.ElevatorDispatchRelayPin != 0 {
				gpio.AddOutput("elevator_dispatch", appCtx.GPIOSettings.ElevatorDispatchRelayPin, appCtx.GPIOSettings.ElevatorDispatchActiveLow, useI2CExpander)
			}
			if len(appCtx.elevatorPredefinedEnablePins) > 0 {
				for i, pin := range appCtx.elevatorPredefinedEnablePins {
					gpio.AddOutput(outputnames.ElevatorPredefinedEnable(i), pin, appCtx.GPIOSettings.ElevatorEnableActiveLow, useI2CExpander)
				}
			}
			if len(appCtx.elevatorWaitFloorEnablePins) > 0 {
				for i, pin := range appCtx.elevatorWaitFloorEnablePins {
					gpio.AddOutput(outputnames.ElevatorWaitFloorEnable(i), pin, appCtx.GPIOSettings.ElevatorEnableActiveLow, useI2CExpander)
				}
			} else if appCtx.GPIOSettings.ElevatorEnableRelayPin != 0 {
				gpio.AddOutput("elevator_enable", appCtx.GPIOSettings.ElevatorEnableRelayPin, appCtx.GPIOSettings.ElevatorEnableActiveLow, useI2CExpander)
			}
			if appCtx.GPIOSettings.AutomaticDoorOperatorRelayPin != 0 {
				gpio.AddOutput("automatic_door_operator", appCtx.GPIOSettings.AutomaticDoorOperatorRelayPin, appCtx.GPIOSettings.AutomaticDoorOperatorRelayActiveLow, useI2CExpander)
			}
			if appCtx.GPIOSettings.IntercomCameraTriggerRelayPin != 0 {
				gpio.AddOutput("intercom_camera_trigger", appCtx.GPIOSettings.IntercomCameraTriggerRelayPin, appCtx.GPIOSettings.IntercomCameraTriggerRelayActiveLow, useI2CExpander)
			}
		}
		gpio.SetDoorHoldOpenWhile(func() bool { return appCtx.FireAlarmInterfaceActive() })
		gpio.ConfigureDoorSensor(appCtx.GPIOSettings.DoorSensorPin)
		waitMode := NormalizeKeypadOperationMode(appCtx.Config.KeypadOperationMode) == ModeElevatorWaitFloorButtons
		if waitMode && elevatorWaitFloorSenseCabInputs(appCtx.Config) {
			if pins, err := parseBCMPinList(appCtx.Config.ElevatorFloorInputPins); err == nil && len(pins) > 0 {
				gpio.ConfigureElevatorFloorPins(pins)
			} else if err != nil && strings.TrimSpace(appCtx.Config.ElevatorFloorInputPins) != "" {
				log.Printf("WARNING: elevator_floor_input_pins: %v", err)
			}
		}
		setupOperationModeGPIOInputs(appCtx, gpio)
		setupFiremansServiceGPIOInput(appCtx, gpio)
		setupFireAlarmInterfaceGPIOInput(appCtx, gpio)
		setupTamperSwitchGPIOInput(appCtx, gpio)
		setupMotionSensorGPIOInput(appCtx, gpio)
		appCtx.GPIO = gpio
		go func() {
			time.Sleep(200 * time.Millisecond)
			appCtx.syncFiremansServiceFromHardwareReason("startup")
			appCtx.syncFireAlarmFromHardwareReason("startup")
		}()
	}

	// 5. Initialize MQTT for Centralized Remote Control [cite: 6, 7]
	appCtx.MQTTClient = initMQTT(appCtx)

	// 6. Start Concurrent Subsystems
	go startHeartbeatAPI(appCtx) // Regular heartbeat via API [cite: 9]
	go startKeypadListeners(appCtx)
	appCtx.configMu.RLock()
	scannerEnabled := appCtx.Config.ScannerEnabled
	appCtx.configMu.RUnlock()
	if scannerEnabled {
		go startQRScannerListener(appCtx)
	} else {
		log.Printf("INFO: QR scanner disabled via config (scanner_enabled=false); listener not started.")
	}
	go monitorElevatorFloorSelection(appCtx)
	go monitorDoorSensors(appCtx) // Door open timers & warnings
	go displayController(appCtx)  // I2C HD44780 LCD + PIN mask (non-blocking vs keypad)
	lcdShowIdle(appCtx)           // Initial welcome screen when lcd_display.enabled is true

	// 7. Start Web Server (Web UI & REST HTTP API with token support) [cite: 6, 7]
	srv := startWebServer(appCtx)
	appCtx.configMu.RLock()
	startupCfg := appCtx.Config
	appCtx.configMu.RUnlock()
	playSoundEnabled(startupCfg, startupCfg.SoundStartup, startupCfg.SoundStartupEnabled, startupCfg.SoundStartupBlocking)

	shutdownFromMenu := make(chan struct{}, 1)
	if !*noTechMenu {
		go runTechnicianMenu(appCtx, shutdownFromMenu)
	} else {
		releaseStartupLogBuffer(os.Stdout)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	select {
	case <-quit:
	case <-shutdownFromMenu:
	}
	rootStop()
	log.Println("INFO: Shutdown signal received.")
	appCtx.configMu.RLock()
	shutdownCfg := appCtx.Config
	appCtx.configMu.RUnlock()
	playSoundEnabled(shutdownCfg, shutdownCfg.SoundShutdown, shutdownCfg.SoundShutdownEnabled, shutdownCfg.SoundShutdownBlocking)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("WARNING: HTTP server shutdown: %v", err)
	}
	auditLogFlush(3 * time.Second)
	return 0
}
