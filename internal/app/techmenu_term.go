package app

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/term"
)

// Technician terminal UI: reserve bottom row for "{TechMenuPrompt}> " while logs scroll above (DECSTBM + prompt redraw).
var (
	techUILock            sync.Mutex
	techBottomLineEnabled bool
	techTerminalRows      int

	// techMenuInputDraft is the current in-progress line from readTechMenuLine; repainted after log lines (same lock as UI).
	techMenuInputDraft []byte

	startupLogMu        sync.Mutex
	startupLogBuffer    [][]byte
	startupLogsReleased bool // after menu banner or -notechmenu; further log lines are not buffered
	// startupLogsReleasedFast mirrors startupLogsReleased so released-state log lines skip startupLogMu.
	startupLogsReleasedFast atomic.Bool

	techMenuPromptMu   sync.RWMutex
	techMenuPromptText string // copy of AppContext.TechMenuPrompt for the log writer (set via registerTechMenuPrompt)
)

// linux / glibc TIOCGWINSZ
const tiocgwinsz = 0x5413

type termWinSize struct {
	row uint16
	col uint16
	x   uint16
	y   uint16
}

func queryTerminalRows() int {
	var ws termWinSize
	fd := os.Stdout.Fd()
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(tiocgwinsz), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.row < 2 {
		if s := os.Getenv("LINES"); s != "" {
			var n int
			_, _ = fmt.Sscanf(s, "%d", &n)
			if n >= 2 {
				return n
			}
		}
		return 24
	}
	return int(ws.row)
}

// registerTechMenuPrompt copies the label from AppContext for use on the technician status line (call after appCtx is built).
func registerTechMenuPrompt(label string) {
	techMenuPromptMu.Lock()
	defer techMenuPromptMu.Unlock()
	if strings.TrimSpace(label) == "" {
		techMenuPromptText = "tech"
		return
	}
	techMenuPromptText = label
}

func activeTechMenuPrompt() string {
	techMenuPromptMu.RLock()
	defer techMenuPromptMu.RUnlock()
	if techMenuPromptText == "" {
		return "tech"
	}
	return techMenuPromptText
}

// moveToScrollRegionBottomUnlocked moves the cursor to the first column of the bottom line
// inside the scrolling region (row rows-1). The following text + LF scrolls only that region,
// so logs never print on the reserved status row (row rows).
func moveToScrollRegionBottomUnlocked(w io.Writer) {
	if !techBottomLineEnabled || techTerminalRows < 2 {
		return
	}
	_, _ = fmt.Fprintf(w, "\033[%d;1H", techTerminalRows-1)
}

// paintTechPromptRowUnlocked redraws the bottom status row and leaves the cursor after "{prompt}> "
// for /dev/tty echo. Does not use save/restore (that restored the cursor onto the status line and broke logging).
func paintTechPromptRowUnlocked(w io.Writer) {
	if !techBottomLineEnabled || techTerminalRows < 2 {
		return
	}
	_, _ = fmt.Fprintf(w, "\033[%d;1H\033[K", techTerminalRows)
	_, _ = fmt.Fprint(w, activeTechMenuPrompt())
	_, _ = fmt.Fprint(w, "> ")
}

// paintTechPromptAndInputDraftUnlocked redraws the status prompt and any in-progress technician input.
// Caller must hold techUILock.
func paintTechPromptAndInputDraftUnlocked(w io.Writer) {
	paintTechPromptRowUnlocked(w)
	if len(techMenuInputDraft) > 0 {
		_, _ = w.Write(techMenuInputDraft)
	}
}

func enableTechBottomTerminalLayout() {
	rows := queryTerminalRows()
	if rows < 2 {
		return
	}
	techUILock.Lock()
	techTerminalRows = rows
	techBottomLineEnabled = true
	// Scroll only lines 1..rows-1; bottom row stays fixed. Home cursor in scroll region for new logs.
	_, _ = fmt.Fprintf(os.Stdout, "\033[1;%dr\033[1;1H", rows-1)
	paintTechPromptAndInputDraftUnlocked(os.Stdout)
	techUILock.Unlock()
}

func disableTechBottomTerminalLayout() {
	techUILock.Lock()
	techBottomLineEnabled = false
	_, _ = fmt.Fprint(os.Stdout, "\033[r\n")
	techUILock.Unlock()
}

// terminalHardReset sends RIS and related sequences (like the `reset` command) so margins, modes, and colors return to defaults.
func terminalHardReset() {
	const seq = "\033[0m\033[?25h\033[r\033c"
	_, _ = fmt.Fprint(os.Stdout, seq)
	if t, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		_, _ = fmt.Fprint(t, seq)
		_ = t.Close()
	}
}

// techMenuClearScreenAndRelayout clears the visible screen and restores the scrolling region and bottom prompt.
func techMenuClearScreenAndRelayout() {
	rows := queryTerminalRows()
	if rows < 2 {
		techUILock.Lock()
		rows = techTerminalRows
		techUILock.Unlock()
	}
	if rows < 2 {
		rows = 24
	}
	techUILock.Lock()
	defer techUILock.Unlock()
	techTerminalRows = rows
	techBottomLineEnabled = true
	_, _ = fmt.Fprint(os.Stdout, "\033[2J\033[1;1H")
	_, _ = fmt.Fprintf(os.Stdout, "\033[1;%dr\033[1;1H", rows-1)
	paintTechPromptAndInputDraftUnlocked(os.Stdout)
}

// bufferStartupLogLine returns true if the line was buffered (caller should not emit yet).
func bufferStartupLogLine(line []byte) bool {
	if startupLogsReleasedFast.Load() {
		return false
	}
	startupLogMu.Lock()
	defer startupLogMu.Unlock()
	if startupLogsReleased {
		return false
	}
	cp := append([]byte(nil), line...)
	startupLogBuffer = append(startupLogBuffer, cp)
	return true
}

// releaseStartupLogBuffer flushes buffered log lines after the menu is visible (or when there is no menu).
func releaseStartupLogBuffer(w io.Writer) {
	startupLogMu.Lock()
	if startupLogsReleased {
		startupLogMu.Unlock()
		return
	}
	lines := startupLogBuffer
	startupLogBuffer = nil
	startupLogsReleased = true
	startupLogsReleasedFast.Store(true)
	startupLogMu.Unlock()

	for _, ln := range lines {
		techUILock.Lock()
		moveToScrollRegionBottomUnlocked(w)
		_, _ = w.Write(ln)
		paintTechPromptAndInputDraftUnlocked(w)
		techUILock.Unlock()
	}
}

// techMenuSyncPrint runs f on stdout under the UI lock and redraws the bottom prompt. Do not call log from inside f.
func techMenuSyncPrint(f func(w io.Writer)) {
	techUILock.Lock()
	defer techUILock.Unlock()
	moveToScrollRegionBottomUnlocked(os.Stdout)
	f(os.Stdout)
	paintTechPromptAndInputDraftUnlocked(os.Stdout)
}

func (ctx *AppContext) techHistoryMax() int {
	ctx.configMu.RLock()
	m := ctx.Config.TechMenuHistoryMax
	ctx.configMu.RUnlock()
	if m <= 0 {
		return 100
	}
	if m > 10000 {
		return 10000
	}
	return m
}

func (ctx *AppContext) techHistoryTrimToMax() {
	max := ctx.techHistoryMax()
	ctx.techHistMu.Lock()
	for len(ctx.techHist) > max {
		ctx.techHist = ctx.techHist[1:]
	}
	ctx.techHistMu.Unlock()
}

func (ctx *AppContext) techHistoryAppend(entry string) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return
	}
	max := ctx.techHistoryMax()
	ctx.techHistMu.Lock()
	ctx.techHist = append(ctx.techHist, entry)
	for len(ctx.techHist) > max {
		ctx.techHist = ctx.techHist[1:]
	}
	ctx.techHistMu.Unlock()
}

func (ctx *AppContext) techHistoryClear() {
	ctx.techHistMu.Lock()
	ctx.techHist = nil
	ctx.techHistMu.Unlock()
}

func techMenuRootCommands() []string {
	return []string{
		"...", "…",
		"1", "2", "3", "4", "5", "6", "7", "8", "9",
		"acl",
		"c", "cfg", "ch", "clear", "cls",
		"exit",
		"firemans", "fireman", "fs",
		"h", "help", "i", "kb", "kbd", "keypads",
		"m", "menu", "occ", "p", "q", "quit",
		"v", "z",
	}
}

// techMenuCfgSubcommands lists cfg second tokens for Tab completion (no single-letter aliases — they share prefixes and block LCP).
func techMenuCfgSubcommands() []string {
	return []string{
		"apply", "help", "history", "keys", "list", "live",
		"reboot", "reread", "reload", "restart", "save", "set", "show", "write",
	}
}

func techMenuSplitForComplete(line string) (prefix []string, partial string, trailingSpace bool) {
	if len(line) == 0 {
		return nil, "", false
	}
	trailingSpace = line[len(line)-1] == ' ' || line[len(line)-1] == '\t'
	trimmed := strings.TrimRight(line, " \t")
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return nil, "", trailingSpace
	}
	if trailingSpace {
		return fields, "", true
	}
	if len(fields) == 1 {
		return nil, fields[0], false
	}
	return fields[:len(fields)-1], fields[len(fields)-1], false
}

func techMenuLowerPrefixSlice(s []string) []string {
	out := make([]string, len(s))
	for i, w := range s {
		out[i] = strings.ToLower(w)
	}
	return out
}

func techMenuFilterPrefixLower(cands []string, lowPrefix string) []string {
	var out []string
	for _, c := range cands {
		if strings.HasPrefix(strings.ToLower(c), lowPrefix) {
			out = append(out, c)
		}
	}
	return out
}

func techMenuLongestCommonPrefix(strs []string) string {
	if len(strs) == 0 {
		return ""
	}
	ref := strs[0]
	for i := range ref {
		rc := ref[i]
		for _, s := range strs[1:] {
			if i >= len(s) || s[i] != rc {
				return ref[:i]
			}
		}
	}
	return ref
}

func techMenuCompleteAddTrailingSpace(prefixLower []string, completed string) bool {
	if len(prefixLower) >= 1 && prefixLower[0] == "acl" {
		return techMenuACLCompleteAddSpace(prefixLower, completed)
	}
	c := strings.ToLower(completed)
	if c == "..." || c == "…" {
		return false
	}
	if len(prefixLower) == 0 {
		return true
	}
	if len(prefixLower) == 1 && prefixLower[0] == "cfg" {
		return true
	}
	if len(prefixLower) == 2 && prefixLower[0] == "cfg" && prefixLower[1] == "set" {
		return true
	}
	if len(prefixLower) == 1 && prefixLower[0] == "kb" {
		return true
	}
	return false
}

// techMenuTabCompleteLine returns an updated input line and whether to ring the terminal bell (no extension).
func techMenuTabCompleteLine(line string) (newLine string, bell bool) {
	prefix, partial, trail := techMenuSplitForComplete(line)
	pl := techMenuLowerPrefixSlice(prefix)

	var matches []string
	switch {
	case len(pl) >= 1 && pl[0] == "acl":
		var ok bool
		matches, ok = techMenuACLTabMatches(prefix, partial, trail)
		if !ok {
			matches = nil
		}
	case len(pl) == 0 && !trail:
		matches = techMenuFilterPrefixLower(techMenuRootCommands(), strings.ToLower(partial))
	case len(pl) == 1 && pl[0] == "cfg" && trail:
		matches = append([]string(nil), techMenuCfgSubcommands()...)
	case len(pl) == 1 && pl[0] == "cfg" && !trail:
		matches = techMenuFilterPrefixLower(techMenuCfgSubcommands(), strings.ToLower(partial))
	case len(pl) == 2 && pl[0] == "cfg" && pl[1] == "set" && trail:
		matches = append([]string(nil), techMenuCfgKeysForCompletion...)
	case len(pl) == 2 && pl[0] == "cfg" && pl[1] == "set" && !trail:
		matches = techMenuFilterPrefixLower(techMenuCfgKeysForCompletion, strings.ToLower(partial))
	case len(pl) == 1 && pl[0] == "kb" && trail:
		matches = []string{"all"}
	case len(pl) == 1 && pl[0] == "kb" && !trail:
		matches = techMenuFilterPrefixLower([]string{"all"}, strings.ToLower(partial))
	default:
		return line, true
	}

	if len(pl) >= 1 && pl[0] == "acl" && len(matches) == 0 {
		return line, true
	}

	if len(matches) == 0 {
		return line, true
	}

	lowPart := strings.ToLower(partial)
	if trail {
		lowPart = ""
	}

	var pick string
	addSpace := false

	if len(matches) == 1 {
		pick = matches[0]
		addSpace = techMenuCompleteAddTrailingSpace(pl, pick)
	} else {
		lcp := techMenuLongestCommonPrefix(matches)
		if !strings.HasPrefix(lcp, lowPart) || len(lcp) == len(lowPart) {
			return line, true
		}
		pick = lcp
		addSpace = false
	}

	newWords := append(append([]string{}, prefix...), pick)
	out := strings.Join(newWords, " ")
	if addSpace {
		out += " "
	}
	return out, false
}

func techMenuReadCSI(tty *os.File) ([]byte, error) {
	b := make([]byte, 1)
	if _, err := tty.Read(b); err != nil {
		return nil, err
	}
	if b[0] != '[' && b[0] != 'O' {
		return []byte{b[0]}, nil
	}
	out := []byte{b[0]}
	for {
		if _, err := tty.Read(b); err != nil {
			return out, err
		}
		out = append(out, b[0])
		if b[0] >= 0x40 && b[0] <= 0x7e {
			break
		}
	}
	return out, nil
}

func techMenuRedrawInputLine(line []byte) {
	techUILock.Lock()
	defer techUILock.Unlock()
	techMenuInputDraft = append([]byte(nil), line...)
	paintTechPromptRowUnlocked(os.Stdout)
	if len(line) > 0 {
		_, _ = os.Stdout.Write(line)
	}
}

// readTechMenuLine reads one line from /dev/tty with local echo, Up/Down history, and Backspace. Uses raw mode when possible.
func readTechMenuLine(ctx *AppContext, tty *os.File) (string, error) {
	fd := int(tty.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return readTechMenuLineFallback(tty)
	}
	defer func() {
		_ = term.Restore(fd, old)
		techUILock.Lock()
		techMenuInputDraft = nil
		techUILock.Unlock()
	}()

	var line []byte
	histIdx := -1
	redraw := func() { techMenuRedrawInputLine(line) }
	redraw()

	var upSeq = []byte("\x1b[A")
	var downSeq = []byte("\x1b[B")
	var upSS3 = []byte("\x1bOA")
	var downSS3 = []byte("\x1bOB")

	buf := make([]byte, 1)
	for {
		n, err := tty.Read(buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		b := buf[0]
		switch {
		case b == '\r' || b == '\n':
			techUILock.Lock()
			_, _ = fmt.Fprint(os.Stdout, "\n")
			techUILock.Unlock()
			return string(line), nil
		case b == 127 || b == 8:
			if len(line) > 0 {
				line = line[:len(line)-1]
				histIdx = -1
				redraw()
			}
		case b == '\t':
			histIdx = -1
			nl, bell := techMenuTabCompleteLine(string(line))
			line = []byte(nl)
			if bell {
				_, _ = tty.Write([]byte{'\a'})
			}
			redraw()
		case b == 27:
			csi, err := techMenuReadCSI(tty)
			if err != nil {
				return "", err
			}
			seq := append([]byte{27}, csi...)
			ctx.techHistMu.Lock()
			hist := append([]string(nil), ctx.techHist...)
			ctx.techHistMu.Unlock()
			nh := len(hist)
			switch {
			case bytes.Equal(seq, upSeq) || bytes.Equal(seq, upSS3):
				if nh == 0 {
					redraw()
					continue
				}
				if histIdx < 0 {
					histIdx = nh - 1
				} else if histIdx > 0 {
					histIdx--
				}
				line = append([]byte(nil), hist[histIdx]...)
				redraw()
			case bytes.Equal(seq, downSeq) || bytes.Equal(seq, downSS3):
				if histIdx < 0 {
					continue
				}
				if histIdx < nh-1 {
					histIdx++
					line = append([]byte(nil), hist[histIdx]...)
				} else {
					histIdx = -1
					line = nil
				}
				redraw()
			default:
				// ignore other escape sequences (arrows left/right, etc.)
			}
		case b >= 32 && b < 127:
			histIdx = -1
			line = append(line, b)
			redraw()
		case b == 3:
			line = nil
			histIdx = -1
			redraw()
		default:
			// ignore other control characters
		}
	}
}

func readTechMenuLineFallback(tty *os.File) (string, error) {
	r := bufio.NewReader(tty)
	s, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	s = strings.TrimSuffix(s, "\r")
	s = strings.TrimSuffix(s, "\n")
	return s, nil
}
