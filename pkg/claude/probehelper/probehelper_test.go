package probehelper

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

func TestDispatchPrivateModesRequireExactShape(t *testing.T) {
	for _, args := range [][]string{
		{"tclaude", OpenCodeUnixRelayMode},
		{"tclaude", opencodeapi.UnixLaunchMode, "socket"},
		{"tclaude", opencodeapi.UnixAttachShimMode, "1"},
		{"tclaude", internalPrefix + "stub"},
		{"tclaude", internalPrefix + "unknown"},
	} {
		handled, code := Dispatch(args)
		assert.True(t, handled)
		assert.Equal(t, invalidInvocationExit, code)
	}
	handled, code := Dispatch([]string{"tclaude", "session", "new"})
	assert.False(t, handled)
	assert.Zero(t, code)
}

func TestOpenCodeUnixRelayClosesAdjacentExecutableDescriptor(t *testing.T) {
	previousClose := closeFD
	previousServe := serveInheritedUnixRelay
	t.Cleanup(func() {
		closeFD = previousClose
		serveInheritedUnixRelay = previousServe
	})
	serveInheritedUnixRelay = func(context.Context, int, string, []string) error {
		return errors.New("stop after descriptor check")
	}

	for _, listenerFD := range []int{3, 8} {
		t.Run(fmt.Sprintf("listener-fd-%d", listenerFD), func(t *testing.T) {
			closed := -1
			closeFD = func(fd int) error {
				closed = fd
				return nil
			}
			handled, code := Dispatch([]string{
				"tclaude", OpenCodeUnixRelayMode, strconv.Itoa(listenerFD),
				"127.0.0.1:1", "--", "/missing-opencode",
			})
			assert.True(t, handled)
			assert.Equal(t, helperFailureExit, code)
			assert.Equal(t, listenerFD+1, closed)
		})
	}
}
