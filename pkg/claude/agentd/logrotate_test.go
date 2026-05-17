package agentd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
)

// startLogRotation must rotate a log that is already oversized when the
// daemon starts (the immediate first check) and keep rotating on the
// ticker thereafter — exercising the goroutine wiring and stop channel.
func TestStartLogRotation_RotatesViaTicker(t *testing.T) {
	prev := logRotationInterval
	logRotationInterval = 25 * time.Millisecond
	t.Cleanup(func() { logRotationInterval = prev })

	path := filepath.Join(t.TempDir(), "output.log")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), 800), 0644))

	rw, err := common.OpenRotatingWriter(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rw.Close() })

	cfg := &config.Config{LogRotation: &config.LogRotationConfig{MaxSize: "500", Keep: 3}}
	stop := make(chan struct{})
	defer close(stop)
	startLogRotation(stop, rw, cfg)

	// The immediate first check rotates the pre-existing oversized file.
	assert.Eventually(t, func() bool {
		info, err := os.Stat(path + ".1")
		return err == nil && info.Size() == 800
	}, 2*time.Second, 10*time.Millisecond, "an oversized log must rotate at startup")

	// The ticker keeps rotating: grow the fresh active log past the cap
	// again and expect a second rotation (the first one cascades to .2).
	_, err = rw.Write(bytes.Repeat([]byte("y"), 700))
	require.NoError(t, err)
	assert.Eventually(t, func() bool {
		info, err := os.Stat(path + ".1")
		return err == nil && info.Size() == 700
	}, 2*time.Second, 10*time.Millisecond, "the rotation ticker must keep firing")
}

// With max_size "0" rotation is disabled: startLogRotation returns
// without configuring the writer, so the log grows unbounded.
func TestStartLogRotation_DisabledByZeroMaxSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.log")
	rw, err := common.OpenRotatingWriter(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rw.Close() })

	cfg := &config.Config{LogRotation: &config.LogRotationConfig{MaxSize: "0"}}
	stop := make(chan struct{})
	defer close(stop)
	startLogRotation(stop, rw, cfg)

	_, err = rw.Write(bytes.Repeat([]byte("x"), 5000))
	require.NoError(t, err)
	// The writer was never Configure'd, so even a direct check no-ops.
	require.NoError(t, rw.MaybeRotate())
	_, statErr := os.Stat(path + ".1")
	assert.True(t, os.IsNotExist(statErr),
		"disabled rotation must leave the writer unconfigured — no rotated file")
}

// A nil writer (logging setup failed, e.g. no home dir) must not crash
// the daemon — rotation is simply skipped.
func TestStartLogRotation_NilWriterIsSafe(t *testing.T) {
	stop := make(chan struct{})
	defer close(stop)
	assert.NotPanics(t, func() {
		startLogRotation(stop, nil, &config.Config{})
	})
}
