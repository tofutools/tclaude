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

// DirectoryBrowser is a composition-selected, read-only host capability.
type DirectoryBrowser interface {
	ReadDirectory(context.Context, DirectoryReadRequest) (model.DirectoryListing, error)
}
