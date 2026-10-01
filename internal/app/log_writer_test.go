package app

import (
	"bytes"
	"testing"
)

func TestColorLogWriterSplitsAndFilters(t *testing.T) {
	prevRel := startupLogsReleasedFast.Load()
	startupLogsReleasedFast.Store(true) // write straight through, no startup buffering
	t.Cleanup(func() { startupLogsReleasedFast.Store(prevRel) })
	prevMin := logEmitMinSeverity.Load()
	t.Cleanup(func() { logEmitMinSeverity.Store(prevMin) })

	var out bytes.Buffer
	w := &colorLogWriter{w: &out, noColor: true}
	logEmitMinSeverity.Store(1) // INFO+
	_, _ = w.Write([]byte("x.go:1: INFO: one\n"))
	_, _ = w.Write([]byte("x.go:2: DEBUG: hidden\nx.go:3: WARN"))
	_, _ = w.Write([]byte("ING: two\n"))
	if got, want := out.String(), "x.go:1: INFO: one\nx.go:3: WARNING: two\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	out.Reset()
	w = &colorLogWriter{w: &out}
	_, _ = w.Write([]byte("x.go:4: ERROR: bad\n"))
	if got, want := out.String(), colorError+"x.go:4: ERROR: bad\n"+colorReset; got != want {
		t.Fatalf("colored got %q, want %q", got, want)
	}
}

func TestDebugfFiltered(t *testing.T) {
	prevMin := logEmitMinSeverity.Load()
	t.Cleanup(func() { logEmitMinSeverity.Store(prevMin) })
	logEmitMinSeverity.Store(1)
	debugf("%v", failStringer{t})
}

type failStringer struct{ t *testing.T }

func (f failStringer) String() string {
	f.t.Fatal("args must not be formatted when DEBUG is filtered")
	return ""
}
