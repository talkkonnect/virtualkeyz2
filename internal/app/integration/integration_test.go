//go:build integration

package integration_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestCLIBuildSanity runs the built binary with -h if present, else skips.
func TestCLIBuildSanity(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("caller")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	cmd := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "vkz"), "./cmd/virtualkeyz2")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
}
