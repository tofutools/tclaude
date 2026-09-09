package ports

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
)

type DirectoryReadRequest struct {
	Path, After   string
	Limit         int
	IncludeHidden bool
}

// DirectoryDefaults resolves host-relative directory intent without creating paths.
type DirectoryDefaults interface {
	NormalizeDefaultDirectory(context.Context, string) (string, error)
	DefaultWorkingDirectory(context.Context) (string, error)
}

// DirectoryBrowser is a composition-selected, read-only host capability.
type DirectoryBrowser interface {
	ReadDirectory(context.Context, DirectoryReadRequest) (model.DirectoryListing, error)
}
