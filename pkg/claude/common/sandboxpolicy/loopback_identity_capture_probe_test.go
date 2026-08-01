package sandboxpolicy

// TEMPORARY DIAGNOSTIC — TCL-916. THIS FILE MUST BE DELETED BEFORE MERGE.
//
// This test answers the question the TCL-910 route probe could not: whether a
// TCP SYN addressed to 0.0.0.1 is observable at the host's packet-capture
// boundary. It deliberately fails so plain `go test` publishes the report in
// CI logs and so the diagnostic cannot be merged accidentally.
//
// The 8.8.8.8 control uses the same capture interface, filter shape, protocol,
// port, and dial path as the subject. If that SYN is not observed, absence of
// the 0.0.0.1 SYN is uninterpretable rather than evidence of local refusal.

import (
	"bytes"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type tcl916ReadyBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *tcl916ReadyBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	ready := strings.Contains(w.buf.String(), "listening on")
	w.mu.Unlock()
	if ready {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func (w *tcl916ReadyBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

type tcl916CaptureResult struct {
	target   string
	ready    bool
	observed bool
	dialErr  error
	stdout   string
	stderr   string
	waitErr  error
}

func tcl916CaptureSYN(target string, port int) tcl916CaptureResult {
	result := tcl916CaptureResult{target: target}
	filter := fmt.Sprintf(
		"dst host %s and tcp dst port %d and tcp[tcpflags] & tcp-syn != 0",
		target, port,
	)
	// alarm survives exec, so tcpdump itself receives SIGALRM after four
	// seconds when the filter sees nothing. Bounding the child directly is
	// important: signaling sudo does not reliably terminate its child on
	// macOS, and a null capture must still reach the caller-owned report.
	cmd := exec.Command(
		"sudo", "-n", "/usr/bin/perl", "-e", "alarm shift; exec @ARGV", "4",
		"tcpdump", "-n", "-l", "-i", "any", "-c", "1", filter,
	)
	var stdout bytes.Buffer
	stderr := &tcl916ReadyBuffer{ready: make(chan struct{})}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		result.stderr = err.Error()
		return result
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case <-stderr.ready:
		result.ready = true
	case result.waitErr = <-waited:
		result.stdout = stdout.String()
		result.stderr = stderr.String()
		return result
	case <-time.After(5 * time.Second):
		result.waitErr = fmt.Errorf("tcpdump did not become ready or exit within 5s")
		result.stdout = stdout.String()
		result.stderr = stderr.String()
		return result
	}

	conn, err := net.DialTimeout(
		"tcp4", net.JoinHostPort(target, fmt.Sprint(port)), 2*time.Second,
	)
	result.dialErr = err
	if conn != nil {
		_ = conn.Close()
	}

	result.waitErr = <-waited
	result.observed = result.waitErr == nil && strings.TrimSpace(stdout.String()) != ""
	result.stdout = stdout.String()
	result.stderr = stderr.String()
	return result
}

func (r tcl916CaptureResult) appendTo(report *strings.Builder, label string) {
	fmt.Fprintf(report, "=== %s: %s:53 ===\n", label, r.target)
	fmt.Fprintf(report, "capture ready: %t\n", r.ready)
	fmt.Fprintf(report, "SYN observed: %t\n", r.observed)
	fmt.Fprintf(report, "dial result: %v\n", r.dialErr)
	fmt.Fprintf(report, "tcpdump wait result: %v\n", r.waitErr)
	fmt.Fprintf(report, "tcpdump stdout:\n%s", r.stdout)
	fmt.Fprintf(report, "tcpdump stderr:\n%s\n", r.stderr)
}

func TestTCL916CaptureZeronetTransmissionTEMPORARY(t *testing.T) {
	report := &strings.Builder{}
	fmt.Fprintf(report, "=== TCL-916 capture: GOOS=%s GOARCH=%s ===\n", runtime.GOOS, runtime.GOARCH)

	control := tcl916CaptureSYN("8.8.8.8", 53)
	control.appendTo(report, "CONTROL: routed destination")
	if !control.ready || !control.observed {
		report.WriteString("CONTROL FAILED — subject absence is uninterpretable.\n\n")
	}

	subject := tcl916CaptureSYN("0.0.0.1", 53)
	subject.appendTo(report, "SUBJECT: non-unspecified 0/8")

	t.Errorf("TEMPORARY TCL-916 DIAGNOSTIC — this failure is deliberate and this "+
		"file is deleted before merge.\n%s", report.String())
}
