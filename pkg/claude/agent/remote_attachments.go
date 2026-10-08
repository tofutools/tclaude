package agent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// ReadRemoteAttachments reads files for a remote (federated) message. The
// CLI reads them as the caller, so an agent can only attach what it can
// read itself; the daemon never opens a caller-supplied path.
func ReadRemoteAttachments(paths []string) ([]proto.AttachmentPayload, error) {
	if len(paths) > proto.MaxAttachments {
		return nil, fmt.Errorf("at most %d attachments per remote message", proto.MaxAttachments)
	}
	var out []proto.AttachmentPayload
	total := 0
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			return nil, fmt.Errorf("%s: not a regular file", path)
		}
		data, err := io.ReadAll(io.LimitReader(f, proto.MaxAttachmentBytes+1))
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		total += len(data)
		if total > proto.MaxAttachmentBytes {
			return nil, fmt.Errorf("attachments exceed %d KiB in total", proto.MaxAttachmentBytes/1024)
		}
		out = append(out, proto.AttachmentPayload{Name: filepath.Base(path), Data: data})
	}
	return out, nil
}
