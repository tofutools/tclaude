package agentd

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func federationJobLivePath(id string) (string, error) {
	if !proto.ValidStreamID(id) {
		return "", errors.New("invalid job identity")
	}
	return filepath.Join(config.DataDir(), "federation", "job-live", id+".frames"), nil
}
func createFederationJobLiveFile(id string) (string, error) {
	path, e := federationJobLivePath(id)
	if e != nil {
		return "", e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return "", e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return "", e
	}
	return path, f.Close()
}
func openFederationJobLiveWriter(path string) (*os.File, error) {
	id := filepath.Base(path)
	if filepath.Ext(id) != ".frames" {
		return nil, errors.New("invalid live output file")
	}
	id = id[:len(id)-len(".frames")]
	expected, e := federationJobLivePath(id)
	if e != nil || path != expected {
		return nil, errors.New("live output outside private job spool")
	}
	fd, e := syscall.Open(path, syscall.O_APPEND|syscall.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		_ = f.Close()
		return nil, errors.New("live output must be a private regular file")
	}
	return f, nil
}
