// Package keypadlist prints a table of Linux evdev devices for keypad wiring (used by tools/listkeypads and the technician menu).
// Paths under /dev/input/by-id and /dev/input/by-path stay stable across reboots; bare /dev/input/eventN numbers are not.
package keypadlist

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// Fprint writes the listkeypads table. If usbOnly is true, only USB-attached devices are shown
// (Phys starts with usb-, or symlink path suggests a USB topology).
func Fprint(w io.Writer, usbOnly bool) error {
	if err := requireLinux(); err != nil {
		return err
	}

	kbdFromProc := kbdHandlersFromProc()
	groups := make(map[string][]string) // canonical /dev/input/eventN -> symlink paths
	scanStableLinkDir("/dev/input/by-id", groups)
	scanStableLinkDir("/dev/input/by-path", groups)

	seenEvent := make(map[string]struct{})
	var out []stableRow
	for eventAbs, links := range groups {
		if len(links) == 0 {
			continue
		}
		sort.Strings(links)
		base := filepath.Base(eventAbs)
		if !deviceNodeExists(eventAbs) {
			continue
		}
		seenEvent[eventAbs] = struct{}{}

		row := buildRow(eventAbs, base, links, kbdFromProc)
		if usbOnly && !isUSBDevice(row.phys, links) {
			continue
		}
		out = append(out, row)
	}

	// Fallback: event nodes with no by-id / by-path symlink (unusual for USB; still list with warning).
	globbed, _ := filepath.Glob("/dev/input/event*")
	for _, eventAbs := range globbed {
		if _, ok := seenEvent[eventAbs]; ok {
			continue
		}
		base := filepath.Base(eventAbs)
		if !deviceNodeExists(eventAbs) {
			continue
		}
		row := buildRow(eventAbs, base, []string{eventAbs}, kbdFromProc)
		row.fallbackDynamic = true
		if usbOnly && !isUSBDevice(row.phys, nil) {
			continue
		}
		out = append(out, row)
	}

	sort.Slice(out, func(i, j int) bool {
		pi, pj := keypadRowPriority(out[i]), keypadRowPriority(out[j])
		if pi != pj {
			return pi > pj
		}
		return eventNum(filepath.Base(out[i].eventPath)) < eventNum(filepath.Base(out[j].eventPath))
	})

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VirtualKeyz2 listkeypads — stable paths (by-id / by-path)")
	fmt.Fprintln(tw, "Put USE_PATH in device.keypad_evdev_path and device.keypad_exit_evdev_path (not event numbers — those change on reboot).")
	if usbOnly {
		fmt.Fprintln(tw, "Filter: USB-attached devices only.\n")
	} else {
		fmt.Fprintln(tw, "Tip: technician `kb` or CLI -usb limits to USB. BACKEND_EVENT is the resolved node for debugging only.\n")
	}
	_, _ = fmt.Fprintln(tw, "USE_PATH\tBACKEND_EVENT\tNAME\tPHYS\tNOTES")
	_, _ = fmt.Fprintln(tw, "—\t—\t—\t—\t—")
	for _, r := range out {
		notes := rowNotes(r)
		phys := r.phys
		if phys == "" {
			phys = "—"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.usePath, r.eventPath, r.name, phys, notes)
	}
	_ = tw.Flush()

	_, _ = fmt.Fprintln(w, "\nTip: sudo evtest <USE_PATH> — symlinks work the same as /dev/input/eventN.")
	return nil
}

type stableRow struct {
	usePath         string // preferred by-id or by-path for JSON
	eventPath       string // canonical /dev/input/eventN
	name            string
	phys            string
	hasKbd          bool
	allLinks        []string
	fallbackDynamic bool // true if only bare eventN (no stable symlink)
}

func buildRow(eventAbs, eventBase string, links []string, kbdFromProc map[string]bool) stableRow {
	name := sysfsInputName(eventBase)
	if name == "" {
		name = "(name not read from sysfs)"
	}
	phys := sysfsPhys(eventBase)
	hasKbd := kbdFromProc[eventBase] || sysfsHasEvKey(eventBase)
	use := pickPreferredStablePath(links)
	return stableRow{
		usePath:   use,
		eventPath: eventAbs,
		name:      name,
		phys:      phys,
		hasKbd:    hasKbd,
		allLinks:  append([]string(nil), links...),
	}
}

func rowNotes(r stableRow) string {
	var parts []string
	if r.fallbackDynamic {
		parts = append(parts, "no by-id/by-path link — event# may change on reboot")
	}
	extra := aliasNote(r.usePath, r.allLinks)
	if extra != "" {
		parts = append(parts, extra)
	}
	switch {
	case r.hasKbd:
		parts = append(parts, "keyboard/keypad candidate")
	case strings.Contains(strings.ToLower(r.name), "key"):
		parts = append(parts, "name suggests keyboard — verify with evtest")
	default:
		parts = append(parts, "verify: sudo evtest "+r.usePath)
	}
	return strings.Join(parts, "; ")
}

func aliasNote(primary string, all []string) string {
	var rest []string
	for _, p := range all {
		if p != primary {
			rest = append(rest, p)
		}
	}
	if len(rest) == 0 {
		return ""
	}
	sort.Strings(rest)
	if len(rest) <= 2 {
		return "also: " + strings.Join(rest, ", ")
	}
	return fmt.Sprintf("also: %s (+ %d more)", strings.Join(rest[:2], ", "), len(rest)-2)
}

func pickPreferredStablePath(links []string) string {
	if len(links) == 0 {
		return ""
	}
	var byID, byPath, other []string
	for _, p := range links {
		switch {
		case strings.Contains(p, "/by-id/"):
			byID = append(byID, p)
		case strings.Contains(p, "/by-path/"):
			byPath = append(byPath, p)
		default:
			other = append(other, p)
		}
	}
	sort.Strings(byID)
	sort.Strings(byPath)
	sort.Strings(other)
	if len(byID) > 0 {
		return byID[0]
	}
	if len(byPath) > 0 {
		return byPath[0]
	}
	return other[0]
}

// scanStableLinkDir adds symlink paths grouped by resolved canonical event device path.
func scanStableLinkDir(dir string, groups map[string][]string) {
	de, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range de {
		if e.IsDir() {
			continue
		}
		baseName := strings.ToLower(e.Name())
		// Skip obvious mouse interfaces; installers want key / keypad nodes.
		if strings.Contains(baseName, "event-mouse") {
			continue
		}
		linkPath := filepath.Join(dir, e.Name())
		resolved, err := filepath.EvalSymlinks(linkPath)
		if err != nil {
			continue
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			continue
		}
		evBase := filepath.Base(resolved)
		if !strings.HasPrefix(evBase, "event") {
			continue
		}
		suf := strings.TrimPrefix(evBase, "event")
		if suf == "" {
			continue
		}
		if _, err := strconv.Atoi(suf); err != nil {
			continue
		}
		groups[resolved] = append(groups[resolved], linkPath)
	}
}

func isUSBDevice(phys string, links []string) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(phys)), "usb-") {
		return true
	}
	for _, p := range links {
		pl := strings.ToLower(p)
		if strings.Contains(pl, "-usb-") || strings.Contains(pl, "/usb-") {
			return true
		}
	}
	return false
}

func kbdHandlersFromProc() map[string]bool {
	out := make(map[string]bool)
	b, err := os.ReadFile("/proc/bus/input/devices")
	if err != nil {
		return out
	}
	for _, block := range splitProcInputBlocks(string(b)) {
		var handlers string
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.HasPrefix(line, "H: Handlers=") {
				handlers = strings.TrimSpace(strings.TrimPrefix(line, "H: Handlers="))
				break
			}
		}
		if handlers == "" {
			continue
		}
		hasKbd := strings.Contains(" "+handlers+" ", " kbd ") || strings.HasPrefix(handlers, "kbd")
		if !hasKbd {
			continue
		}
		for _, ev := range handlersLineEvents(handlers) {
			out[ev] = true
		}
	}
	return out
}

func requireLinux() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("keypad list only runs on Linux (GOOS=%s)", runtime.GOOS)
	}
	if _, err := os.Stat("/dev/input"); err != nil {
		return fmt.Errorf("missing /dev/input")
	}
	return nil
}

func splitProcInputBlocks(data string) []string {
	lines := strings.Split(strings.TrimSpace(data), "\n")
	var blocks []string
	var cur strings.Builder
	flush := func() {
		s := strings.TrimSpace(cur.String())
		if s != "" {
			blocks = append(blocks, s)
		}
		cur.Reset()
	}
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "I:") && cur.Len() > 0 {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(line)
	}
	flush()
	return blocks
}

func handlersLineEvents(h string) []string {
	var out []string
	for _, tok := range strings.Fields(h) {
		if !strings.HasPrefix(tok, "event") {
			continue
		}
		suf := tok[len("event"):]
		if suf == "" {
			continue
		}
		if _, err := strconv.Atoi(suf); err == nil {
			out = append(out, tok)
		}
	}
	return out
}

func eventNum(eventID string) int {
	s := strings.TrimPrefix(eventID, "event")
	n, _ := strconv.Atoi(s)
	return n
}

func keypadRowPriority(r stableRow) int {
	if r.hasKbd {
		return 3
	}
	nl := strings.ToLower(r.name)
	if strings.Contains(nl, "keypad") || strings.Contains(nl, "keyboard") || strings.Contains(nl, "hid") {
		return 2
	}
	if strings.Contains(nl, "key") {
		return 1
	}
	return 0
}

func deviceNodeExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func sysfsInputName(eventBase string) string {
	candidates := []string{
		filepath.Join("/sys/class/input", eventBase, "device", "name"),
		filepath.Join("/sys/class/input", eventBase, "name"),
	}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func sysfsPhys(eventBase string) string {
	p := filepath.Join("/sys/class/input", eventBase, "device", "phys")
	if b, err := os.ReadFile(p); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func sysfsHasEvKey(eventBase string) bool {
	p := filepath.Join("/sys/class/input", eventBase, "device", "capabilities", "ev")
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return false
	}
	v, err := strconv.ParseUint(fields[0], 16, 64)
	if err != nil {
		return false
	}
	return v&0x01 != 0
}
