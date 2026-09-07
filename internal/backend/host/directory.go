package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type DirectoryBrowser struct{}

func (DirectoryBrowser) ReadDirectory(ctx context.Context, req ports.DirectoryReadRequest) (model.DirectoryListing, error) {
	var out model.DirectoryListing
	if !filepath.IsAbs(req.Path) || len(req.Path) > 4096 || !utf8.ValidString(req.Path) || strings.ContainsRune(req.Path, 0) || len(req.After) > 255 || !utf8.ValidString(req.After) || strings.ContainsAny(req.After, "/\x00") || req.Limit < 1 || req.Limit > 200 {
		return out, fmt.Errorf("invalid directory request")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	path, err := filepath.EvalSymlinks(filepath.Clean(req.Path))
	if err != nil {
		return out, err
	}
	dir, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(8193)
	if err != nil && !errors.Is(err, io.EOF) {
		return out, err
	}
	if len(entries) > 8192 {
		return out, fmt.Errorf("directory exceeds the bounded entry inventory")
	}
	out.Path = path
	out.Parent = filepath.Dir(path)
	out.Directories = []model.DirectoryEntry{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return model.DirectoryListing{}, err
		}
		name := entry.Name()
		if !utf8.ValidString(name) || name <= req.After || (!req.IncludeHidden && strings.HasPrefix(name, ".")) {
			continue
		}
		isDir := entry.IsDir()
		if !isDir && entry.Type()&os.ModeSymlink != 0 {
			info, statErr := os.Stat(filepath.Join(path, name))
			isDir = statErr == nil && info.IsDir()
		}
		if isDir {
			out.Directories = append(out.Directories, model.DirectoryEntry{Name: name, Path: filepath.Join(path, name)})
		}
	}
	sort.Slice(out.Directories, func(i, j int) bool { return out.Directories[i].Name < out.Directories[j].Name })
	if len(out.Directories) > req.Limit {
		out.Directories = out.Directories[:req.Limit]
		out.NextAfter = out.Directories[len(out.Directories)-1].Name
	}
	return out, nil
}
