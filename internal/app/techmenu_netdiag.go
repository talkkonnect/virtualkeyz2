package app

import (
	"cmp"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// --- Technician menu: network diagnostics (Linux /proc) ---

func readResolvConfNameservers() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var ns []string
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 && strings.EqualFold(f[0], "nameserver") {
			ns = append(ns, f[1])
		}
	}
	return ns
}

func procLittleEndianHexIPv4(h8 string) string {
	if len(h8) != 8 {
		return ""
	}
	raw, err := hex.DecodeString(h8)
	if err != nil || len(raw) != 4 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d", raw[3], raw[2], raw[1], raw[0])
}

// parseDefaultIPv4GatewaysByIface reads /proc/net/route (Linux): iface name -> gateway for default IPv4 routes.
func parseDefaultIPv4GatewaysByIface() map[string]string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}
	out := make(map[string]string)
	lines := strings.Split(string(b), "\n")
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if f[1] != "00000000" {
			continue
		}
		gw := procLittleEndianHexIPv4(f[2])
		if gw == "" {
			continue
		}
		out[f[0]] = gw
	}
	return out
}

func ifaceLANWiFiBucket(name string) int {
	n := strings.ToLower(strings.TrimSpace(name))
	if strings.HasPrefix(n, "wlan") || strings.HasPrefix(n, "wl") {
		return 1
	}
	if strings.HasPrefix(n, "eth") || strings.HasPrefix(n, "en") || strings.HasPrefix(n, "end") || strings.HasPrefix(n, "usb") {
		return 0
	}
	return 2
}

func collectIPv4Nets(ifi *net.Interface) []*net.IPNet {
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	var out []*net.IPNet
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() {
			continue
		}
		out = append(out, ipn)
	}
	return out
}

func techMenuWriteOneInterface(w io.Writer, ifi net.Interface, gwByIface map[string]string) {
	up := ifi.Flags&net.FlagUp != 0
	admin := "down"
	if up {
		admin = "up"
	}
	fmt.Fprintf(w, "  %s: admin=%s  MTU=%d\n", ifi.Name, admin, ifi.MTU)
	nets := collectIPv4Nets(&ifi)
	if len(nets) == 0 {
		fmt.Fprintln(w, "    IPv4: (none assigned)")
	} else {
		for _, ipn := range nets {
			mask := net.IP(ipn.Mask).String()
			fmt.Fprintf(w, "    IPv4: %s  subnet mask: %s\n", ipn.IP.String(), mask)
		}
	}
	gw := ""
	if gwByIface != nil {
		gw = gwByIface[ifi.Name]
	}
	if gw == "" {
		fmt.Fprintln(w, "    Default gateway (IPv4, this iface): (none in /proc/net/route)")
	} else {
		fmt.Fprintf(w, "    Default gateway (IPv4, this iface): %s\n", gw)
	}
}

func techMenuWriteNetworkDiag(w io.Writer) {
	fmt.Fprintln(w, "\n--- Network (snapshot) ---")
	if runtime.GOOS != "linux" {
		fmt.Fprintf(w, "  Full interface list is from Go (below); gateway/DNS use Linux-specific paths.\n\n")
	}
	dns := readResolvConfNameservers()
	if len(dns) == 0 {
		fmt.Fprintln(w, "  DNS (/etc/resolv.conf): (none found)")
	} else {
		fmt.Fprintf(w, "  DNS (/etc/resolv.conf): %s\n", strings.Join(dns, ", "))
	}

	var gwByIface map[string]string
	if runtime.GOOS == "linux" {
		gwByIface = parseDefaultIPv4GatewaysByIface()
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		fmt.Fprintf(w, "  error listing interfaces: %v\n\n", err)
		return
	}

	var ethList, wifiList, otherList []net.Interface
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		switch ifaceLANWiFiBucket(ifi.Name) {
		case 0:
			ethList = append(ethList, ifi)
		case 1:
			wifiList = append(wifiList, ifi)
		default:
			otherList = append(otherList, ifi)
		}
	}

	printGroup := func(title string, list []net.Interface) {
		fmt.Fprintf(w, "\n%s\n", title)
		if len(list) == 0 {
			fmt.Fprintln(w, "  (no interface in this category)")
			return
		}
		for _, ifi := range list {
			techMenuWriteOneInterface(w, ifi, gwByIface)
		}
	}

	printGroup("--- Ethernet / LAN ---", ethList)
	printGroup("--- Wi-Fi ---", wifiList)
	if len(otherList) > 0 {
		printGroup("--- Other interfaces ---", otherList)
	}
	fmt.Fprintln(w, "")
}

func procLocalAddrToHostPort(addr string, ipv6 bool) (string, bool) {
	colon := strings.LastIndex(addr, ":")
	if colon < 0 {
		return "", false
	}
	ipHex := addr[:colon]
	portHex := addr[colon+1:]
	portU, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return "", false
	}
	port := int(portU)
	if !ipv6 {
		if len(ipHex) != 8 {
			return "", false
		}
		ip := procLittleEndianHexIPv4(ipHex)
		if ip == "" {
			return "", false
		}
		return net.JoinHostPort(ip, strconv.Itoa(port)), true
	}
	if len(ipHex) != 32 {
		return "", false
	}
	raw, err := hex.DecodeString(ipHex)
	if err != nil || len(raw) != 16 {
		return "", false
	}
	ip := net.IP(raw)
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), true
}

// procNetSocketInode returns the socket inode from a /proc/net/{tcp,udp}{,6} data line (field index 9 on recent kernels).
func procNetSocketInode(f []string) (string, bool) {
	if len(f) <= 9 {
		return "", false
	}
	inode := f[9]
	if _, err := strconv.ParseUint(inode, 10, 64); err != nil {
		return "", false
	}
	return inode, true
}

func isProcNetSocketDataLine(f []string) bool {
	return len(f) >= 10 && strings.Contains(f[1], ":")
}

// buildGlobalSocketInodeToProcs maps socket inode -> "pid/comm" list (all processes holding that inode).
func buildGlobalSocketInodeToProcs() map[string]string {
	sets := make(map[string]map[string]struct{})
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	const pref = "socket:["
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		comm, _ := os.ReadFile(filepath.Join("/proc", pid, "comm"))
		name := strings.TrimSpace(string(comm))
		if name == "" {
			name = "?"
		}
		fdDir := filepath.Join("/proc", pid, "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if !strings.HasPrefix(link, pref) || !strings.HasSuffix(link, "]") {
				continue
			}
			ino := strings.TrimSuffix(strings.TrimPrefix(link, pref), "]")
			if sets[ino] == nil {
				sets[ino] = make(map[string]struct{})
			}
			sets[ino][fmt.Sprintf("%s/%s", pid, name)] = struct{}{}
		}
	}
	out := make(map[string]string, len(sets))
	for ino, s := range sets {
		out[ino] = strings.Join(slices.Sorted(maps.Keys(s)), ", ")
	}
	return out
}

type procListenRow struct {
	proto string
	addr  string
	inode string
}

func scanProcNetAllTCPListeners(path, proto string, out *[]procListenRow) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ipv6 := strings.Contains(path, "tcp6")
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "local_address") {
			continue
		}
		f := strings.Fields(line)
		if !isProcNetSocketDataLine(f) {
			continue
		}
		if f[3] != "0A" { // TCP_LISTEN
			continue
		}
		inode, ok := procNetSocketInode(f)
		if !ok {
			continue
		}
		hostport, ok := procLocalAddrToHostPort(f[1], ipv6)
		if !ok {
			continue
		}
		*out = append(*out, procListenRow{proto: proto, addr: hostport, inode: inode})
	}
	return nil
}

// UDP "listening" / bound unconnected sockets (st 07) in /proc/net/udp{,6}.
func scanProcNetAllUDPBinds(path, proto string, out *[]procListenRow) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ipv6 := strings.Contains(path, "udp6")
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "local_address") {
			continue
		}
		f := strings.Fields(line)
		if !isProcNetSocketDataLine(f) {
			continue
		}
		if f[3] != "07" { // UDP unconnected (typically bound / accepting datagrams)
			continue
		}
		inode, ok := procNetSocketInode(f)
		if !ok {
			continue
		}
		hostport, ok := procLocalAddrToHostPort(f[1], ipv6)
		if !ok {
			continue
		}
		*out = append(*out, procListenRow{proto: proto, addr: hostport, inode: inode})
	}
	return nil
}

func collectSystemListenRows() ([]procListenRow, error) {
	var rows []procListenRow
	if err := scanProcNetAllTCPListeners("/proc/net/tcp", "tcp", &rows); err != nil {
		return nil, err
	}
	_ = scanProcNetAllTCPListeners("/proc/net/tcp6", "tcp6", &rows)
	_ = scanProcNetAllUDPBinds("/proc/net/udp", "udp", &rows)
	_ = scanProcNetAllUDPBinds("/proc/net/udp6", "udp6", &rows)
	slices.SortFunc(rows, func(a, b procListenRow) int {
		return cmp.Or(cmp.Compare(a.proto, b.proto), cmp.Compare(a.addr, b.addr))
	})
	return rows, nil
}

func techMenuWriteProcListenPorts(w io.Writer) {
	fmt.Fprintln(w, "\n--- System listening / bound ports (all processes) ---")
	fmt.Fprintln(w, "  TCP: sockets in LISTEN. UDP: unconnected bound sockets (typical servers).")
	if runtime.GOOS != "linux" {
		fmt.Fprintln(w, "  (only implemented on Linux via /proc)")
		fmt.Fprintln(w, "")
		return
	}
	ino2p := buildGlobalSocketInodeToProcs()
	rows, err := collectSystemListenRows()
	if err != nil {
		fmt.Fprintf(w, "  error: %v\n\n", err)
		return
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "  (none parsed)")
	} else {
		for _, r := range rows {
			who := ino2p[r.inode]
			if who == "" {
				who = fmt.Sprintf("inode %s (owner not resolved)", r.inode)
			}
			fmt.Fprintf(w, "  %-4s  %-40s  %s\n", r.proto, r.addr, who)
		}
	}
	fmt.Fprintln(w, "\n  Source: /proc/net/tcp, tcp6, udp, udp6; owners from /proc/*/fd (needs permission to scan all PIDs).")
	fmt.Fprintln(w, "")
}
