package diag

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// NetSnapshot describes the machine's network at this moment: which
// interfaces are up, the Wi-Fi signal and the default route. It is read from
// /sys and /proc, so it is empty on systems without them.
func NetSnapshot() string {
	return netSnapshot("/sys/class/net", "/proc/net/wireless", "/proc/net/route")
}

var ignoredPrefixes = []string{"lo", "docker", "br-", "veth", "virbr", "tun", "tap", "wg", "tailscale", "zt"}

func netSnapshot(sysNet, wirelessFile, routeFile string) string {
	signal := wirelessSignals(wirelessFile)
	entries, _ := os.ReadDir(sysNet)
	var parts []string
	for _, e := range entries {
		name := e.Name()
		skip := false
		for _, p := range ignoredPrefixes {
			if name == p || (p != "lo" && strings.HasPrefix(name, p)) {
				skip = true
			}
		}
		if skip {
			continue
		}
		state, _ := os.ReadFile(filepath.Join(sysNet, name, "operstate"))
		part := name + " " + strings.TrimSpace(string(state))
		if level, ok := signal[name]; ok {
			part += fmt.Sprintf(" (sinal %s dBm)", level)
		}
		parts = append(parts, part)
	}
	out := "interfaces: " + strings.Join(parts, ", ")
	if len(parts) == 0 {
		out = "interfaces: nenhuma"
	}
	if gw := defaultRouteInterface(routeFile); gw != "" {
		out += "; rota padrão por " + gw
	} else {
		out += "; sem rota padrão"
	}
	return out
}

// wirelessSignals reads the signal level per interface from /proc/net/wireless.
func wirelessSignals(file string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(file)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest) // status, link, level, noise, ...
		if len(fields) >= 3 {
			out[strings.TrimSpace(name)] = strings.TrimSuffix(fields[2], ".")
		}
	}
	return out
}

// defaultRouteInterface returns the interface of the route to 0.0.0.0.
func defaultRouteInterface(file string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	for i, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if i > 0 && len(f) > 2 && f[1] == "00000000" {
			return f[0]
		}
	}
	return ""
}

// Uptime is how long the machine has been on (0 when unknown).
func Uptime() time.Duration {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}
