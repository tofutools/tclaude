package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (DirectoryBrowser) ReadExecutionFile(ctx context.Context, in ports.ExecutionFileReadRequest) (ports.ExecutionFileContent, error) {
	var empty ports.ExecutionFileContent
	if !filepath.IsAbs(in.Root) || len(in.Root) > 4096 || !utf8.ValidString(in.Root) || in.Path == "" || len(in.Path) > 4096 || !utf8.ValidString(in.Path) || strings.IndexFunc(in.Path, unicode.IsControl) >= 0 {
		return empty, fmt.Errorf("invalid file request")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	relative := in.Path
	if filepath.IsAbs(relative) {
		var err error
		relative, err = filepath.Rel(filepath.Clean(in.Root), filepath.Clean(relative))
		if err != nil {
			return empty, err
		}
	}
	relative = filepath.Clean(relative)
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return empty, fmt.Errorf("file is outside execution directory")
	}
	root, err := os.OpenRoot(in.Root)
	if err != nil {
		return empty, err
	}
	defer func() { _ = root.Close() }()
	// NONBLOCK prevents a raced FIFO/device substitution from blocking at open.
	file, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return empty, err
	}
	defer func() { _ = file.Close() }()
	before, err := file.Stat()
	if err != nil {
		return empty, err
	}
	if !before.Mode().IsRegular() || before.Size() > 32<<20 {
		return empty, fmt.Errorf("file must be regular and at most 32 MiB")
	}
	content := make([]byte, 0, int(before.Size()))
	buffer := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return empty, err
		}
		n, readErr := file.Read(buffer)
		if len(content)+n > 32<<20 {
			return empty, fmt.Errorf("file exceeded read bound")
		}
		content = append(content, buffer[:n]...)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return empty, readErr
		}
	}
	after, err := file.Stat()
	if err != nil {
		return empty, err
	}
	current, err := root.Stat(relative)
	if err != nil {
		return empty, err
	}
	if before.Size() != after.Size() || int64(len(content)) != before.Size() || !before.ModTime().Equal(after.ModTime()) || !os.SameFile(after, current) {
		return empty, fmt.Errorf("file changed during read")
	}
	return ports.ExecutionFileContent{Filename: filepath.Base(relative), Content: content}, nil
}
