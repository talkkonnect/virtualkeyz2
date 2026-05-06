package config

import "time"

// LCDDisplaySettings configures the optional I2C HD44780 20×4 module (device.lcd_display in JSON).
type LCDDisplaySettings struct {
	Enabled          bool
	I2CBus           int
	I2CAddr          uint8
	BacklightTimeout time.Duration
	I2CDebugEnabled  bool
}

// GPIOSettings holds BCM pin numbers and relay polarity (wiring on the Pi).
type GPIOSettings struct {
	RelayOutputMode string
	MCP23017I2CBus  int
	MCP23017I2CAddr uint8
	XL9535I2CBus    int
	XL9535I2CAddr   uint8
	I2CBusRecoverySCLBCM uint8

	DoorRelayPin         uint8
	DoorRelayActiveLow   bool
	BuzzerRelayPin       uint8
	BuzzerRelayActiveLow bool
	DoorSensorPin        uint8
	HeartbeatLEDPin      uint8
	ExitButtonPin        uint8
	ExitButtonActiveLow  bool
	EntryButtonPin       uint8
	EntryButtonActiveLow bool
	ElevatorDispatchRelayPin  uint8
	ElevatorDispatchActiveLow bool
	ElevatorEnableRelayPin    uint8
	ElevatorEnableActiveLow   bool
	ElevatorFloorDispatchPins string
	ElevatorWaitFloorEnablePins string
	ElevatorPredefinedEnablePins string
	LightingButtonPin        uint8
	LightingButtonActiveLow  bool
	LightingRelayPin         uint8
	LightingRelayActiveLow   bool
	FiremansServiceInputPin  uint8
	FiremansServiceActiveLow bool
	AutomaticDoorOperatorRelayPin       uint8
	AutomaticDoorOperatorRelayActiveLow bool
	IntercomCameraTriggerRelayPin       uint8
	IntercomCameraTriggerRelayActiveLow bool
	FireAlarmInterfacePin       uint8
	FireAlarmInterfaceActiveLow   bool
	TamperSwitchPin             uint8
	TamperSwitchActiveLow       bool
	MotionSensorPin             uint8
	MotionSensorActiveLow       bool
}

// WebhookEventEndpoint is one HTTP destination for JSON event webhooks.
type WebhookEventEndpoint struct {
	Enabled      bool            `json:"enabled"`
	URL          string          `json:"url"`
	TokenEnabled bool            `json:"token_enabled"`
	Token        string          `json:"token"`
	EventTypes   map[string]bool `json:"event_types,omitempty"`
}

// DeviceConfig represents configurable parameters (loaded from SQLite/Central Server).
type DeviceConfig struct {
	HeartbeatInterval    time.Duration
	DoorOpenWarningAfter time.Duration
	DoorOpenAlarmInterval time.Duration
	DoorOpenAlarmMaxCount int
	DoorForcedAfterWarnings int
	DoorSensorClosedIsLow bool
	SoundCardName         string
	SoundStartup          string
	SoundShutdown         string
	SoundPinOK            string
	SoundAccessGranted    string
	SoundPinReject        string
	SoundKeypress         string
	SoundLightingTimerSet string
	SoundLightingTimerExpired string
	SoundDoorOpen         string

	SoundStartupEnabled              bool
	SoundShutdownEnabled             bool
	SoundPinOKEnabled                bool
	SoundAccessGrantedEnabled        bool
	SoundPinRejectEnabled            bool
	SoundKeypressEnabled             bool
	SoundLightingTimerSetEnabled     bool
	SoundLightingTimerExpiredEnabled bool
	SoundDoorOpenEnabled             bool

	LogLevel              string
	PinLength             int
	RelayPulseDuration    time.Duration
	AutomaticDoorOperatorPulseDuration time.Duration
	IntercomCameraTriggerPulseDuration time.Duration
	PinRejectBuzzerAfterAttempts int
	BuzzerRelayPulseDuration     time.Duration

	MQTTEnabled      bool
	MQTTBroker       string
	MQTTClientID     string
	MQTTUsername     string
	MQTTPassword     string
	MQTTCommandTopic string
	MQTTStatusTopic  string
	MQTTCommandToken string
	TechMenuHistoryMax int

	KeypadInterDigitTimeout time.Duration
	KeypadSessionTimeout    time.Duration
	PinEntryFeedbackDelay   time.Duration
	PinLockoutEnabled       bool
	PinLockoutAfterAttempts int
	PinLockoutDuration      time.Duration
	PinLockoutOverridePin   string
	FallbackAccessPin       string

	WebhookEventEnabled      bool
	WebhookEventURL          string
	WebhookEventTokenEnabled bool
	WebhookEventToken        string
	WebhookEventTypes        map[string]bool
	WebhookEventEndpoints    []WebhookEventEndpoint
	WebhookHeartbeatEnabled      bool
	WebhookHeartbeatURL          string
	WebhookHeartbeatTokenEnabled bool
	WebhookHeartbeatToken        string
	WebhookHTTPTimeout           time.Duration
	WebhookMaxConcurrent         int
	WebhookCircuitBreakerEnabled bool
	WebhookCircuitFailureThreshold int
	WebhookCircuitOpenDuration   time.Duration

	KeypadOperationMode string
	KeypadEvdevPath     string
	KeypadExitEvdevPath string
	ScannerDevicePath   string
	MaxDevicesPerUser   int
	QRTimeWindowSeconds int
	StaticTestQRCode    string
	StaticTestQRCodeEnabled bool
	PairPeerRole        string
	MQTTPairPeerTopic   string
	PairPeerToken       string
	ElevatorFloorWaitTimeout time.Duration
	ElevatorWaitFloorCabSense string
	ElevatorFloorInputPins    string
	ElevatorPredefinedFloor   int
	ElevatorPredefinedFloors  []int
	ElevatorDispatchPulseDuration time.Duration
	ElevatorFloorDispatchPulseDurations []time.Duration
	ElevatorEnablePulseDuration time.Duration
	DualKeypadRejectExitWithoutEntry bool

	AccessControlDoorID              string
	AccessControlElevatorID          string
	AccessScheduleEnforce            bool
	AccessScheduleApplyToFallbackPin bool
	AccessExceptionSiteTimezone      string
	LightingTimeout                  time.Duration

	LCDDisplay LCDDisplaySettings

	FiremansServiceEnabled bool
	SoundFiremansActivated          string
	SoundFiremansDeactivated        string
	SoundFiremansActivatedEnabled   bool
	SoundFiremansDeactivatedEnabled bool
}
