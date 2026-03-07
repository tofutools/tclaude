package web

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// isWSL returns true if running inside Windows Subsystem for Linux
func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	lower := strings.ToLower(string(data))
	return strings.Contains(lower, "microsoft") || strings.Contains(lower, "wsl")
}

// windowsHostIP returns the Windows host IP as seen from WSL.
// Uses the default gateway from /etc/resolv.conf nameserver or ip route.
func windowsHostIP() string {
	// Try resolv.conf first (most reliable on WSL2)
	data, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "nameserver") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					return parts[1]
				}
			}
		}
	}

	// Fallback: default route
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err == nil {
		fields := strings.Fields(string(out))
		for i, f := range fields {
			if f == "via" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
	}

	return ""
}

// windowsHostname tries to get the Windows hostname via powershell.exe
func windowsHostname() string {
	out, err := exec.Command("hostname.exe").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// windowsLANIPs tries to get the Windows host's LAN IPs via powershell.
func windowsLANIPs() []string {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command",
		`Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.InterfaceAlias -notmatch 'Loopback' -and $_.InterfaceAlias -notmatch 'vEthernet' } | Select-Object -ExpandProperty IPAddress`,
	).Output()
	if err != nil {
		return nil
	}
	var ips []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		ip := strings.TrimSpace(line)
		if ip != "" && net.ParseIP(ip) != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

// printNetworkInfo prints reachable addresses for the server
func printNetworkInfo(scheme string, port int) {
	hostname, _ := os.Hostname()
	fmt.Printf("\n  Hostname: %s\n", hostname)

	// Linux/local IPs
	fmt.Println("  Local IPs:")
	for _, ip := range localIPStrings() {
		fmt.Printf("    %s://%s:%d\n", scheme, ip, port)
	}

	// WSL-specific info
	if isWSL() {
		fmt.Println("\n  WSL detected - Windows host info:")

		if winHost := windowsHostname(); winHost != "" {
			fmt.Printf("    Windows hostname: %s\n", winHost)
		}

		if hostIP := windowsHostIP(); hostIP != "" {
			fmt.Printf("    WSL gateway:      %s\n", hostIP)
		}

		if winIPs := windowsLANIPs(); len(winIPs) > 0 {
			fmt.Println("    Windows LAN IPs (use from phone after port forwarding):")
			for _, ip := range winIPs {
				fmt.Printf("      %s://%s:%d\n", scheme, ip, port)
			}
		}

		fmt.Printf("\n    Port forward (run in PowerShell as admin):\n")
		wslIP := wslEthIP()
		fmt.Printf("      netsh interface portproxy add v4tov4 listenport=%d listenaddress=0.0.0.0 connectport=%d connectaddress=%s\n", port, port, wslIP)
	}
}

// localIPStrings returns all non-loopback IPv4 addresses as strings
func localIPStrings() []string {
	var result []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return result
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			result = append(result, ipnet.IP.String())
		}
	}
	return result
}

// wslEthIP returns the WSL eth0 IP, which is the one Windows needs to forward to.
// Falls back to first non-loopback IP.
func wslEthIP() string {
	// Try eth0 first - that's the standard WSL2 interface
	iface, err := net.InterfaceByName("eth0")
	if err == nil {
		addrs, err := iface.Addrs()
		if err == nil {
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
					return ipnet.IP.String()
				}
			}
		}
	}

	// Fallback: first non-loopback
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "<WSL_IP>"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "<WSL_IP>"
}
