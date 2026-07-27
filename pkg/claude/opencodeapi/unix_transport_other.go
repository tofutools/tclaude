//go:build !linux

package opencodeapi

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func CreateUnixListener(string) (*net.UnixListener, int64, int64, error) {
	return nil, 0, 0, fmt.Errorf("OpenCode Unix relay is Linux-only")
}

func ExecUnixRelayLaunch(string, []string) error {
	return fmt.Errorf("OpenCode Unix relay is Linux-only")
}

func RemoveUnixSocket(runtime db.OpenCodeRuntime) error {
	if runtime.Transport != db.OpenCodeTransportUnixRelay {
		return nil
	}
	return fmt.Errorf("OpenCode Unix relay is Linux-only")
}

func doUnixRequest(*http.Client, *http.Request, db.OpenCodeRuntime) (*http.Response, error) {
	return nil, fmt.Errorf("OpenCode Unix relay is Linux-only")
}

func dialVerifiedUnix(context.Context, db.OpenCodeRuntime) (net.Conn, error) {
	return nil, fmt.Errorf("OpenCode Unix relay is Linux-only")
}
