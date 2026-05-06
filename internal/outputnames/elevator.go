package outputnames

import "fmt"

// ElevatorFloorDispatch is the GPIO output name for per-floor elevator dispatch index i.
func ElevatorFloorDispatch(i int) string {
	return fmt.Sprintf("elevator_floor_dispatch_%d", i)
}

// ElevatorPredefinedEnable is the GPIO output name for predefined-floor enable index i.
func ElevatorPredefinedEnable(i int) string {
	return fmt.Sprintf("elevator_predefined_enable_%d", i)
}

// ElevatorWaitFloorEnable is the GPIO output name for wait-floor enable index i.
func ElevatorWaitFloorEnable(i int) string {
	return fmt.Sprintf("elevator_wait_floor_enable_%d", i)
}
