package probehelper

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

const (
	// internalPrefix marks every private argv mode. The "stacked_probe" in it
	// is historical — the retired stacked sandbox introduced these modes — but
	// the value is an exec token of live OpenCode servers and launchers, so it
	// must not change across an upgrade.
	internalPrefix = "__tclaude_stacked_probe_"

	OpenCodeUnixRelayMode = opencodeapi.InheritedUnixRelayMode

	invalidInvocationExit = 64
	helperFailureExit     = 74
)

var (
	closeFD                 = unix.Close
	serveInheritedUnixRelay = opencodeapi.ServeInheritedUnixRelay
)

// Dispatch handles the private OpenCode helper modes before normal command
// construction can touch config, logging, or database state. The exact tokens
// deliberately are not Cobra commands and therefore never enter help or shell
// completion.
func Dispatch(args []string) (bool, int) {
	if len(args) < 2 || !strings.HasPrefix(args[1], internalPrefix) {
		return false, 0
	}
	switch args[1] {
	case OpenCodeUnixRelayMode:
		if len(args) < 6 || args[4] != "--" {
			return true, invalidInvocationExit
		}
		fd, err := opencodeapi.ParseInheritedRelayFD(args[2])
		if err != nil {
			return true, invalidInvocationExit
		}
		// The launcher always preserves the relay executable immediately after
		// the listener. Filtered-network supervisors prepend their own inherited
		// descriptors, so the pair is not necessarily fd 3/fd 4. Closing fd 4
		// here could instead close the Go runtime's lazily-created netpoll eventfd.
		_ = closeFD(fd + 1)
		if err := serveInheritedUnixRelay(
			context.Background(), fd, args[3], args[5:]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "tclaude OpenCode Unix relay: %v\n", err)
			return true, helperFailureExit
		}
		return true, 0
	case opencodeapi.UnixLaunchMode:
		if len(args) < 5 || args[3] != "--" {
			return true, invalidInvocationExit
		}
		if err := opencodeapi.ExecUnixRelayLaunch(args[2], args[4:]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "tclaude OpenCode Unix launcher: %v\n", err)
			return true, helperFailureExit
		}
		return true, 0
	case opencodeapi.UnixAttachShimMode:
		if len(args) < 9 || args[7] != "--" {
			return true, invalidInvocationExit
		}
		pid, pidErr := strconv.Atoi(args[2])
		device, deviceErr := strconv.ParseInt(args[4], 10, 64)
		inode, inodeErr := strconv.ParseInt(args[5], 10, 64)
		if pidErr != nil || deviceErr != nil || inodeErr != nil ||
			pid <= 1 || device <= 0 || inode <= 0 {
			return true, invalidInvocationExit
		}
		runtime := db.OpenCodeRuntime{
			PID: pid, ServerURL: args[6],
			Transport:           db.OpenCodeTransportUnixRelay,
			ControlSocketPath:   args[3],
			ControlSocketDevice: device,
			ControlSocketInode:  inode,
		}
		if err := opencodeapi.RunUnixAttachShim(
			context.Background(), runtime, args[8:]); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "tclaude OpenCode attach shim: %v\n", err)
			return true, opencodeapi.ProcessExitCode(err, helperFailureExit)
		}
		return true, 0
	default:
		return true, invalidInvocationExit
	}
}
