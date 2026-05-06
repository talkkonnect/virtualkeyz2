package config

import "testing"

func TestNormalizeKeypadOperationMode(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ModeAccessEntry},
		{"  ACCESS_ENTRY ", ModeAccessEntry},
		{"Elevator_Wait_Floor_Buttons", ModeElevatorWaitFloorButtons},
		{"unknown-mode-xyz", ModeAccessEntry},
	}
	for _, tt := range tests {
		if got := NormalizeKeypadOperationMode(tt.in); got != tt.want {
			t.Errorf("NormalizeKeypadOperationMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNormalizeElevatorWaitFloorCabSense(t *testing.T) {
	if g := NormalizeElevatorWaitFloorCabSense("ignore"); g != ElevatorWaitFloorCabSenseIgnore {
		t.Fatalf("got %q", g)
	}
	if g := NormalizeElevatorWaitFloorCabSense(""); g != ElevatorWaitFloorCabSenseSense {
		t.Fatalf("got %q", g)
	}
}
