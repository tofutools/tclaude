package sandboxpolicy

// TEMPORARY DIAGNOSTIC — TCL-910. THIS FILE MUST BE DELETED BEFORE MERGE.
//
// It exists to answer one question on hosts I cannot reach from a sandbox:
// does connect() to 0.0.0.0/8 beyond 0.0.0.0 itself reach the local host, on a
// machine with a real route table, on both platforms we ship? The GitHub
// runners are such machines and already run this package on ubuntu-latest and
// macos-latest, so riding the existing job is cheaper and more reproducible
// than any number typed out of somebody's laptop.
//
// It FAILS ON PURPOSE. `go test` is not run with -v in CI, and a passing test's
// output is buffered and discarded — so a green diagnostic would print nothing
// and read exactly like a diagnostic that never ran. The report only reaches
// the job log via a failure, and a failure is also what stops this from being
// merged by accident.
//
// Controls, so no line below can be produced by absence:
//
//   - CONTROL A: connect() to 0.0.0.0 must reach a listener bound to 127.0.0.1
//     ONLY. This is already established by execution; if it fails here, the
//     environment is interfering and no other line in the report means anything.
//   - CONTROL B: the route table, and a dial of a routable public address. On a
//     host with no default route EVERY off-host address yields ENETUNREACH, so
//     the 0.0.0.0/8 results would not distinguish "the kernel treats this space
//     specially" from "nothing is routed at all". That is exactly the trap the
//     sandbox this was written in falls into.

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func tcl910Describe(err error) string {
	if err == nil {
		return "<nil>"
	}
	if errno, ok := errors.AsType[syscall.Errno](err); ok {
		return fmt.Sprintf("%v [errno %d]", err, uintptr(errno))
	}
	return err.Error()
}

func tcl910Dial(network, addr string) (string, error) {
	conn, err := net.DialTimeout(network, addr, 3*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return fmt.Sprintf("local=%s remote=%s", conn.LocalAddr(), conn.RemoteAddr()), nil
}

func tcl910Run(report *strings.Builder, name string, args ...string) {
	out, err := exec.Command(name, args...).CombinedOutput()
	fmt.Fprintf(report, "$ %s %s\n%s", name, strings.Join(args, " "), out)
	if err != nil {
		fmt.Fprintf(report, "(exit: %v)\n", err)
	}
	report.WriteString("\n")
}

func tcl910RouteGet(report *strings.Builder, addr string) {
	switch runtime.GOOS {
	case "linux":
		tcl910Run(report, "ip", "route", "get", addr)
	case "darwin":
		family := "-inet"
		if strings.Contains(addr, ":") {
			family = "-inet6"
		}
		tcl910Run(report, "route", "-n", "get", family, addr)
	}
}

func tcl910Accept(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		conn.Close()
	}
}

func TestTCL910ProbeZeronetReachabilityTEMPORARY(t *testing.T) {
	report := &strings.Builder{}
	fmt.Fprintf(report, "=== TCL-910 probe: GOOS=%s GOARCH=%s ===\n", runtime.GOOS, runtime.GOARCH)
	tcl910Run(report, "uname", "-a")
	if runtime.GOOS == "darwin" {
		tcl910Run(report, "sw_vers")
	}

	listener4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot listen on 127.0.0.1, environment is unusable: %v", err)
	}
	defer listener4.Close()
	go tcl910Accept(listener4)
	port4 := listener4.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(report, "v4 listener bound to %s (127.0.0.1 ONLY, not 0.0.0.0)\n", listener4.Addr())

	var port6 int
	listener6, err6 := net.Listen("tcp6", "[::1]:0")
	if err6 == nil {
		defer listener6.Close()
		go tcl910Accept(listener6)
		port6 = listener6.Addr().(*net.TCPAddr).Port
		fmt.Fprintf(report, "v6 listener bound to %s (::1 ONLY, not ::)\n", listener6.Addr())
	} else {
		fmt.Fprintf(report, "v6 listener unavailable: %v\n", err6)
	}

	report.WriteString("\n=== CONTROL A: 0.0.0.0 must reach the 127.0.0.1-only listener ===\n")
	got, err := tcl910Dial("tcp4", fmt.Sprintf("0.0.0.0:%d", port4))
	fmt.Fprintf(report, "dial tcp4 0.0.0.0:%d -> %s err=%s\n", port4, got, tcl910Describe(err))
	if err != nil {
		report.WriteString("CONTROL A FAILED — every line below is uninterpretable.\n")
	}

	report.WriteString("\n=== CONTROL B: does this host route anything at all? ===\n")
	switch runtime.GOOS {
	case "linux":
		tcl910Run(report, "ip", "route", "show")
	case "darwin":
		tcl910Run(report, "netstat", "-rn", "-f", "inet")
	}
	tcl910RouteGet(report, "8.8.8.8")
	got, err = tcl910Dial("tcp4", "8.8.8.8:53")
	fmt.Fprintf(report, "dial tcp4 8.8.8.8:53 -> %s err=%s\n", got, tcl910Describe(err))
	report.WriteString("(Success, refusal or timeout here means routes exist. ENETUNREACH\n")
	report.WriteString(" here means no default route, and the /8 results below cannot\n")
	report.WriteString(" distinguish 'zeronet is special' from 'nothing is routed'.)\n")

	report.WriteString("\n=== SUBJECT: the rest of 0.0.0.0/8, same port, same listener ===\n")
	for _, host := range []string{"0.0.0.1", "0.0.1.1", "0.1.2.3", "0.128.0.1", "0.255.255.254"} {
		target := fmt.Sprintf("%s:%d", host, port4)
		got, err := tcl910Dial("tcp4", target)
		fmt.Fprintf(report, "dial tcp4 %-22s -> %s err=%s\n", target, got, tcl910Describe(err))
	}
	report.WriteString("\n")
	tcl910RouteGet(report, "0.0.0.0")
	tcl910RouteGet(report, "0.0.0.1")
	tcl910RouteGet(report, "0.1.2.3")

	report.WriteString("=== the kernel's local-address table (what 'the local host' means) ===\n")
	switch runtime.GOOS {
	case "linux":
		tcl910Run(report, "ip", "route", "show", "table", "local")
	case "darwin":
		tcl910Run(report, "ifconfig", "-a")
	}

	report.WriteString("=== SANITY: 127/8 past 127.0.0.1 is local (expect ECONNREFUSED, not ENETUNREACH) ===\n")
	got, err = tcl910Dial("tcp4", fmt.Sprintf("127.1.2.3:%d", port4))
	fmt.Fprintf(report, "dial tcp4 127.1.2.3:%d -> %s err=%s\n", port4, got, tcl910Describe(err))

	if err6 == nil {
		report.WriteString("\n=== v6 unspecified: :: must reach the ::1-only listener ===\n")
		got, err := tcl910Dial("tcp6", fmt.Sprintf("[::]:%d", port6))
		fmt.Fprintf(report, "dial tcp6 [::]:%d -> %s err=%s\n", port6, got, tcl910Describe(err))
		tcl910RouteGet(report, "::")
	}

	t.Errorf("TEMPORARY TCL-910 DIAGNOSTIC — this failure is deliberate and this "+
		"file is deleted before merge.\n%s", report.String())
}
