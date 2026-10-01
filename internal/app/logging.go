package app

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

// logEmitMinSeverity: emit log lines whose severity is >= this (0=DEBUG all, 1=INFO+, 2=WARNING+, 3=ERROR+, 4=CRITICAL only).
var logEmitMinSeverity atomic.Int32

func syncLogFilterFromConfigLevel(level string) {
	logEmitMinSeverity.Store(parseLogLevelMin(level))
}

func parseLogLevelMin(level string) int32 {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "all", "debug":
		return 0
	case "info":
		return 1
	case "warning", "warn":
		return 2
	case "error":
		return 3
	case "critical":
		return 4
	default:
		return 0
	}
}

// lineLogSeverity returns 0 DEBUG .. 4 CRITICAL; unknown lines treated as INFO (1).
func lineLogSeverity(line []byte) int32 {
	switch {
	case bytes.Contains(line, logTagCritical):
		return 4
	case bytes.Contains(line, logTagError):
		return 3
	case bytes.Contains(line, logTagWarning):
		return 2
	case bytes.Contains(line, logTagDebug):
		return 0
	default: // INFO: or untagged
		return 1
	}
}

var (
	logTagCritical = []byte("CRITICAL:")
	logTagError    = []byte("ERROR:")
	logTagWarning  = []byte("WARNING:")
	logTagDebug    = []byte("DEBUG:")
)

// debugf logs a "DEBUG: " line, skipping all formatting (and the log package's caller lookup)
// when log_level filters DEBUG out. Hot paths log through this instead of log.Printf.
func debugf(format string, args ...any) {
	if logEmitMinSeverity.Load() > 0 {
		return
	}
	_ = log.Output(2, fmt.Sprintf("DEBUG: "+format, args...))
}

func shouldEmitLogLine(line []byte) bool {
	min := logEmitMinSeverity.Load()
	return lineLogSeverity(line) >= min
}

// --- Subsystem Implementations (Stubs) ---

func initLogger() {
	// Set up logger with configurable levels (Info, Debug, Warning, Critical) [cite: 9]
	// Allows console debugging [cite: 8]
	log.SetOutput(newColorLogWriter(os.Stdout))
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("INFO: Access Control System Booting...")
}

// ANSI level colors (foreground). Set NO_COLOR in the environment to disable.
const (
	colorReset   = "\033[0m"
	colorDebug   = "\033[36m"   // cyan
	colorInfo    = "\033[32m"   // green
	colorWarning = "\033[33m"   // yellow
	colorError   = "\033[31m"   // red
	colorCrit    = "\033[1;31m" // bold red
)

// levelTag associates a log prefix with a color code.
var logLevelTags = []struct {
	prefix string
	color  string
}{
	{"CRITICAL:", colorCrit},
	{"ERROR:", colorError},
	{"WARNING:", colorWarning},
	{"DEBUG:", colorDebug},
	{"INFO:", colorInfo},
}

type colorLogWriter struct {
	w       io.Writer
	noColor bool         // NO_COLOR set in the environment (read once)
	buf     []byte       // partial line carried between Write calls
	line    bytes.Buffer // colored line
	frame   bytes.Buffer // cursor move + line + prompt repaint, written in one call
}

func newColorLogWriter(w io.Writer) *colorLogWriter {
	return &colorLogWriter{w: w, noColor: os.Getenv("NO_COLOR") != ""}
}

// Write splits p into lines. The log package calls it once per complete line (serialized by the
// Logger), so the common case writes p directly without buffering.
func (c *colorLogWriter) Write(p []byte) (n int, err error) {
	n = len(p)
	if len(c.buf) == 0 && bytes.IndexByte(p, '\n') == len(p)-1 {
		c.writeLine(p)
		return n, nil
	}
	c.buf = append(c.buf, p...)
	for {
		idx := bytes.IndexByte(c.buf, '\n')
		if idx < 0 {
			return n, nil
		}
		c.writeLine(c.buf[:idx+1])
		c.buf = c.buf[:copy(c.buf, c.buf[idx+1:])]
	}
}

func (c *colorLogWriter) writeLine(line []byte) {
	if !shouldEmitLogLine(line) {
		return
	}
	c.line.Reset()
	color := ""
	if !c.noColor {
		for _, lt := range logLevelTags {
			if bytes.Contains(line, []byte(lt.prefix)) {
				color = lt.color
				break
			}
		}
	}
	c.line.WriteString(color)
	c.line.Write(line)
	if color != "" {
		c.line.WriteString(colorReset)
	}
	if bufferStartupLogLine(c.line.Bytes()) {
		return
	}
	techUILock.Lock()
	c.frame.Reset()
	moveToScrollRegionBottomUnlocked(&c.frame)
	c.frame.Write(c.line.Bytes())
	paintTechPromptAndInputDraftUnlocked(&c.frame)
	_, _ = c.w.Write(c.frame.Bytes())
	techUILock.Unlock()
}
