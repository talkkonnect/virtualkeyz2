package app

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Golden tests lock down config load/save/show/set behaviour so refactors of the
// per-key plumbing cannot silently change it. Regenerate with:
//
//	go test ./internal/app/ -run Golden -update
var updateGolden = flag.Bool("update", false, "rewrite golden files in testdata/")

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update)", path, err)
	}
	if !bytes.Equal(got, want) {
		gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
		for i := range max(len(gl), len(wl)) {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("%s differs at line %d:\n got: %q\nwant: %q", name, i+1, g, w)
			}
		}
		t.Fatalf("%s differs", name)
	}
}

func quietLogs(t *testing.T) {
	t.Helper()
	prev := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(prev) })
}

func goldenDefaultCtx(t *testing.T) *AppContext {
	t.Helper()
	ctx := newDefaultAppContext(context.Background(), nil)
	ctx.ConfigPath = "testdata/virtualkeyz2.json"
	return ctx
}

func goldenSampleCtx(t *testing.T) *AppContext {
	t.Helper()
	ctx := goldenDefaultCtx(t)
	if err := loadVirtualKeyz2Config(filepath.Join("testdata", "sample_config.json"), ctx); err != nil {
		t.Fatal(err)
	}
	normalizeKeypadAndPinUX(&ctx.Config)
	return ctx
}

func persistJSON(t *testing.T, ctx *AppContext) []byte {
	t.Helper()
	b, err := json.MarshalIndent(buildPersistFile(ctx), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// flatPersist returns the persisted document as path -> JSON value.
func flatPersist(t *testing.T, ctx *AppContext) map[string]string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(persistJSON(t, ctx), &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if m, ok := v.(map[string]any); ok {
			for k, sub := range m {
				walk(prefix+"."+k, sub)
			}
			return
		}
		b, _ := json.Marshal(v)
		out[prefix] = string(b)
	}
	for k, v := range doc {
		walk(k, v)
	}
	return out
}

func TestGoldenPersistDefaults(t *testing.T) {
	quietLogs(t)
	checkGolden(t, "persist_defaults.golden.json", persistJSON(t, goldenDefaultCtx(t)))
}

func TestGoldenPersistSample(t *testing.T) {
	quietLogs(t)
	checkGolden(t, "persist_sample.golden.json", persistJSON(t, goldenSampleCtx(t)))
}

func TestGoldenShowConfig(t *testing.T) {
	quietLogs(t)
	for _, tc := range []struct {
		name string
		ctx  func(*testing.T) *AppContext
	}{{"defaults", goldenDefaultCtx}, {"sample", goldenSampleCtx}} {
		var buf bytes.Buffer
		techMenuShowConfig(&buf, tc.ctx(t))
		checkGolden(t, "showconfig_"+tc.name+".golden.txt", buf.Bytes())
	}
}

func TestGoldenCfgKeysHelpAndCompletion(t *testing.T) {
	var buf bytes.Buffer
	techMenuCfgKeysHelp(&buf)
	checkGolden(t, "cfgkeys_help.golden.txt", buf.Bytes())
	checkGolden(t, "cfgkeys_completion.golden.txt", []byte(strings.Join(techMenuCfgKeysForCompletion, "\n")+"\n"))
}

// TestGoldenCfgSet applies a fixed set of candidate values to every cfg key on a
// fresh default context and records the error (if any) plus which persisted
// lines changed.
func TestGoldenCfgSet(t *testing.T) {
	quietLogs(t)
	baseCtx := goldenDefaultCtx(t)
	normalizeKeypadAndPinUX(&baseCtx.Config)
	syncElevatorFloorDispatchPulseDurations(baseCtx)
	base := flatPersist(t, baseCtx)
	values := []string{"", "0", "1", "7", "true", "false", "2s", "1m30s", "abc", "1,2", "5,6,7", "400ms,500ms", "gpio", "mcp23017", "elevator_predefined_floor", "access_entry", "sense", "ignore", "debug", "-3"}
	var out strings.Builder
	for _, key := range techMenuCfgKeysForCompletion {
		for _, v := range values {
			ctx := goldenDefaultCtx(t)
			err := techMenuCfgSetValue(ctx, key, v)
			got := flatPersist(t, ctx)
			var diff []string
			for k, gv := range got {
				if bv, ok := base[k]; !ok || bv != gv {
					diff = append(diff, k+"="+gv)
				}
			}
			for k := range base {
				if _, ok := got[k]; !ok {
					diff = append(diff, k+"=<absent>")
				}
			}
			slices.Sort(diff)
			errS := "ok"
			if err != nil {
				errS = "err: " + err.Error()
			}
			fmt.Fprintf(&out, "%s=%q -> %s %v\n", key, v, errS, diff)
		}
	}
	syncLogFilterFromConfigLevel("debug")
	checkGolden(t, "cfgset.golden.txt", []byte(out.String()))
}
