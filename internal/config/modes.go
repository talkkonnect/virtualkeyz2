package config

import (
	"strings"
	"time"
)

// Keypad / access operation modes (device.keypad_operation_mode in JSON).
const (
	ModeAccessEntry               = "access_entry"
	ModeAccessExit                = "access_exit"
	ModeAccessEntryWithExitButton = "access_entry_with_exit_button"
	ModeAccessExitWithEntryButton = "access_exit_with_entry_button"
	ModeAccessDualUSBKeypad       = "access_dual_usb_keypad"
	ModeAccessPairedRemoteExit    = "access_paired_remote_exit"
	ModeElevatorWaitFloorButtons  = "elevator_wait_floor_buttons"
	ModeElevatorPredefinedFloor   = "elevator_predefined_floor"
)

// Relay output backend (gpio.relay_output_mode).
const (
	RelayOutputGPIO     = "gpio"
	RelayOutputMCP23017 = "mcp23017"
	RelayOutputXL9535   = "xl9535"
)

// PairPeerRoleEntry / PairPeerExit used with ModeAccessPairedRemoteExit.
const (
	PairPeerRoleNone  = "none"
	PairPeerRoleEntry = "entry"
	PairPeerRoleExit  = "exit"
)

// Elevator wait-floor cab sense sub-modes (device.elevator_wait_floor_cab_sense).
const (
	ElevatorWaitFloorCabSenseSense  = "sense"
	ElevatorWaitFloorCabSenseIgnore = "ignore"
)

// NormalizeKeypadOperationMode returns a canonical mode string or access_entry if unknown.
func NormalizeKeypadOperationMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", ModeAccessEntry:
		return ModeAccessEntry
	case ModeAccessExit:
		return ModeAccessExit
	case ModeAccessEntryWithExitButton:
		return ModeAccessEntryWithExitButton
	case ModeAccessExitWithEntryButton:
		return ModeAccessExitWithEntryButton
	case ModeAccessDualUSBKeypad:
		return ModeAccessDualUSBKeypad
	case ModeAccessPairedRemoteExit:
		return ModeAccessPairedRemoteExit
	case ModeElevatorWaitFloorButtons:
		return ModeElevatorWaitFloorButtons
	case ModeElevatorPredefinedFloor:
		return ModeElevatorPredefinedFloor
	default:
		return ModeAccessEntry
	}
}

// NormalizePairPeerRole returns entry, exit, or none.
func NormalizePairPeerRole(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case PairPeerRoleEntry:
		return PairPeerRoleEntry
	case PairPeerRoleExit:
		return PairPeerRoleExit
	default:
		return PairPeerRoleNone
	}
}

// IsDualUSBKeypadMode reports whether mode is access_dual_usb_keypad.
func IsDualUSBKeypadMode(mode string) bool {
	return mode == ModeAccessDualUSBKeypad
}

// ModeUsesExitGPIOButton reports whether the mode uses the exit GPIO button.
func ModeUsesExitGPIOButton(mode string) bool {
	return mode == ModeAccessEntryWithExitButton
}

// ModeUsesEntryGPIOButton reports whether the mode uses the entry GPIO button.
func ModeUsesEntryGPIOButton(mode string) bool {
	return mode == ModeAccessExitWithEntryButton
}

// IsElevatorWaitFloorMode reports elevator_wait_floor_buttons.
func IsElevatorWaitFloorMode(mode string) bool {
	return mode == ModeElevatorWaitFloorButtons
}

// IsElevatorPredefinedMode reports elevator_predefined_floor.
func IsElevatorPredefinedMode(mode string) bool {
	return mode == ModeElevatorPredefinedFloor
}

// IsElevatorKeypadMode reports either elevator keypad mode.
func IsElevatorKeypadMode(mode string) bool {
	return IsElevatorWaitFloorMode(mode) || IsElevatorPredefinedMode(mode)
}

// NormalizeElevatorWaitFloorCabSense returns sense or ignore.
func NormalizeElevatorWaitFloorCabSense(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ElevatorWaitFloorCabSenseIgnore, "off", "false", "no":
		return ElevatorWaitFloorCabSenseIgnore
	default:
		return ElevatorWaitFloorCabSenseSense
	}
}

// ElevatorWaitFloorSenseCabInputs is true when cab GPIO sensing is enabled in cfg.
func ElevatorWaitFloorSenseCabInputs(cfg DeviceConfig) bool {
	return NormalizeElevatorWaitFloorCabSense(cfg.ElevatorWaitFloorCabSense) == ElevatorWaitFloorCabSenseSense
}

// PairedEntryPublishesToPeer is true when this unit should publish pair-peer MQTT as the entry side.
func PairedEntryPublishesToPeer(mode, pairRole string) bool {
	return mode == ModeAccessPairedRemoteExit && strings.EqualFold(pairRole, PairPeerRoleEntry)
}

// PairedExitSubscribesToPeer is true when this unit should subscribe for pair-peer MQTT as the exit side.
func PairedExitSubscribesToPeer(mode, pairRole string) bool {
	return mode == ModeAccessPairedRemoteExit && strings.EqualFold(pairRole, PairPeerRoleExit)
}

// ElevatorCabSenseArmDelay and related timing for wait-floor cab sense debounce.
const (
	ElevatorCabSenseArmDelay    = 300 * time.Millisecond
	ElevatorCabSenseStableTicks = 3
)
