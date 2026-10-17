//go:build linux || darwin

package agentd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Keep a supervisor as the unreaped group leader until the daemon has killed
// the group. A plain Cmd.Wait reaps the leader before draining inherited pipes;
// a background child could otherwise survive it, or a late kill could target a
// reused PID. Command arguments are passed as argv, never interpolated here.
const breakGlassSupervisor = `"$@"
result=$?
printf '%s\n' "$result" >&3
IFS= read -r release <&4
`

func newBreakGlassProcess(ctx context.Context, args []string) (*exec.Cmd, func() (int, error), func(), error) {
	resultRead, resultWrite, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	holdRead, holdWrite, err := os.Pipe()
	if err != nil {
		_ = resultRead.Close()
		_ = resultWrite.Close()
		return nil, nil, nil, err
	}
	cleanup := func() { _ = resultRead.Close(); _ = resultWrite.Close(); _ = holdRead.Close(); _ = holdWrite.Close() }
	argv := append([]string{"-c", breakGlassSupervisor, "break-glass"}, args...)
	cmd := exec.CommandContext(ctx, "/bin/sh", argv...)
	configureProxyCommand(cmd)
	cmd.ExtraFiles = []*os.File{resultWrite, holdRead}
	wait := func() (int, error) {
		_ = resultWrite.Close()
		_ = holdRead.Close()
		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			line, e := bufio.NewReader(resultRead).ReadString('\n')
			if e != nil {
				done <- result{1, e}
				return
			}
			code, e := strconv.Atoi(strings.TrimSpace(line))
			if e == nil && (code < 0 || code > 255) {
				e = fmt.Errorf("invalid exit code from supervisor")
			}
			done <- result{code, e}
		}()
		out := result{code: 124}
		select {
		case out = <-done:
		case <-ctx.Done():
			out.err = ctx.Err()
		}
		// Wait has not run: even a dead supervisor still owns its unreaped PID.
		killErr := killProxyGroup(cmd)
		_ = cmd.Wait()
		if killErr != nil && out.err == nil {
			out.err = killErr
		}
		return out.code, out.err
	}
	return cmd, wait, cleanup, nil
}
