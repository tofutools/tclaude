//go:build !windows

package agentd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFetchCopilotEnrichedModelsDeadlineKillsInheritedPipeHolder(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")
	child := filepath.Join(dir, "pipe-holder")
	script := filepath.Join(dir, "copilot-hung-wrapper")
	childBody := fmt.Sprintf(`#!/bin/sh
trap '' TERM
echo $$ > %s
while :; do sleep 10; done
`, shellSingleQuote(pidFile))
	require.NoError(t, os.WriteFile(child, []byte(childBody), 0o700))
	body := fmt.Sprintf(`#!/bin/sh
%s &
wait
`, shellSingleQuote(child))
	require.NoError(t, os.WriteFile(script, []byte(body), 0o700))

	// The deadline fires only once the descendant is up and holding the
	// pipe: a fixed short timeout could kill the group before the child had
	// even recorded its pid, which proves nothing and fails the read below
	// on a loaded runner.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var descendantPID int
	cancelled := make(chan time.Time, 1)
	go func() {
		defer func() { cancelled <- time.Now(); cancel() }()
		limit := time.Now().Add(10 * time.Second)
		for time.Now().Before(limit) {
			if raw, readErr := os.ReadFile(pidFile); readErr == nil {
				if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
					descendantPID = pid
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	_, err := fetchCopilotEnrichedModels(ctx, script, "test-token")
	require.Error(t, err)
	cancelledAt := <-cancelled
	require.Positive(t, descendantPID, "the inherited-pipe descendant never started")
	assert.Less(t, time.Since(cancelledAt), 3*time.Second,
		"a descendant holding stdout must not defeat the catalog deadline")

	require.Eventually(t, func() bool {
		err := syscall.Kill(descendantPID, 0)
		return errors.Is(err, syscall.ESRCH)
	}, 2*time.Second, 20*time.Millisecond,
		"the private process group must not leave the inherited-pipe descendant behind")
}
