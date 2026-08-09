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

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := fetchCopilotEnrichedModels(ctx, script, "test-token")
	require.Error(t, err)
	assert.Less(t, time.Since(started), 3*time.Second,
		"a descendant holding stdout must not defeat the catalog deadline")

	var descendantPID int
	require.Eventually(t, func() bool {
		raw, readErr := os.ReadFile(pidFile)
		if readErr != nil {
			return false
		}
		descendantPID, readErr = strconv.Atoi(strings.TrimSpace(string(raw)))
		return readErr == nil && descendantPID > 0
	}, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		err := syscall.Kill(descendantPID, 0)
		return errors.Is(err, syscall.ESRCH)
	}, 2*time.Second, 20*time.Millisecond,
		"the private process group must not leave the inherited-pipe descendant behind")
}
