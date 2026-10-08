// Package nodeinfo probes slowly changing, non-identifying node capabilities.
package nodeinfo

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func Base() proto.NodeMetadata {
	return proto.NodeMetadata{Schema: 1, OS: runtime.GOOS, Arch: runtime.GOARCH, TclaudeVersion: buildversion.AppVersion(), Labels: []string{}, Harnesses: []proto.NodeHarness{}}
}

// Probe runs only fixed --version argv for registered harness binaries. Failures
// leave the installed harness's version unknown, not a fabricated version.
func Probe(ctx context.Context) proto.NodeMetadata {
	n := Base()
	n.OSVersion = osVersion(ctx)
	for _, name := range harness.Names() {
		h, _ := harness.Get(name)
		if h.Spawn == nil {
			continue
		}
		path, err := exec.LookPath(h.Spawn.Binary())
		if err != nil {
			continue
		}
		version := output(ctx, path, "--version")
		n.Harnesses = append(n.Harnesses, proto.NodeHarness{Name: name, Version: version})
	}
	return *proto.SanitizeNode(&n)
}

type limitedOutput struct{ data []byte }

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(w.data)+len(p) > 4096 {
		return 0, errors.New("version output too large")
	}
	w.data = append(w.data, p...)
	return len(p), nil
}
func output(parent context.Context, binary string, args ...string) string {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	var b limitedOutput
	cmd.Stdout = &b
	// Bound Wait even if a descendant retains stdout after the probe exits.
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(b.data)), "\n")
	if line == "" {
		return ""
	}
	return proto.SafeName(line, false)
}
