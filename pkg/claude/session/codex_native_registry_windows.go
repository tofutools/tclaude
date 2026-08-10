//go:build windows

package session

import (
	"errors"
	"os"
)

func fileOwnerUID(os.FileInfo) (uint32, bool) { return 0, false }
func currentUID() uint32                      { return 0 }

type nativeRegistryLock struct{}

func acquireNativeRegistryLock(string) (*nativeRegistryLock, error) {
	return nil, errors.New("native Codex permission registry is supported on Linux and macOS")
}

func (*nativeRegistryLock) Close() error { return nil }
