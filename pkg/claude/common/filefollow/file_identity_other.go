//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd

package filefollow

import "os"

func fileIdentity(os.FileInfo) (device, inode uint64, ok bool) { return 0, 0, false }
