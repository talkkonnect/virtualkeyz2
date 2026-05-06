// Command virtualkeyz2 runs the access-control / keypad controller binary.
package main

import (
	"os"

	"virtualkeyz2/internal/app"
)

func main() {
	os.Exit(app.Main())
}
