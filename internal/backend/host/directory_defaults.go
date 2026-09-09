package host

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"os"
	"path/filepath"
	"strings"
)

// NormalizeDefaultDirectory preserves v1 future-directory authoring: expand the
// human home shorthand and normalize, but defer existence checks until launch.
func (DirectoryBrowser) NormalizeDefaultDirectory(ctx context.Context, raw string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := strings.TrimSpace(raw)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if path != "" && filepath.IsAbs(path) {
		path = filepath.Clean(path)
	}
	return path, model.ValidateDefaultDirectory(path)
}
func (DirectoryBrowser) DefaultWorkingDirectory(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return os.Getwd()
}
