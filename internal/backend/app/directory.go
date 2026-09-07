package app

import (
	"context"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type BrowseDirectoryRequest struct {
	Principal     model.Principal
	Path, After   string
	Limit         int
	IncludeHidden bool
}
type DirectoryAPI interface {
	BrowseDirectory(context.Context, BrowseDirectoryRequest) (model.DirectoryListing, error)
}

func (s *Service) WithDirectoryBrowser(browser ports.DirectoryBrowser) *Service {
	s.directoryBrowser = browser
	return s
}
func (s *Service) BrowseDirectory(ctx context.Context, req BrowseDirectoryRequest) (model.DirectoryListing, error) {
	if err := requireOperator(req.Principal); err != nil {
		return model.DirectoryListing{}, err
	}
	if !filepath.IsAbs(req.Path) || len(req.Path) > 4096 || !utf8.ValidString(req.Path) || strings.ContainsRune(req.Path, 0) || len(req.After) > 255 || !utf8.ValidString(req.After) || strings.ContainsAny(req.After, "/\x00") || req.Limit < 0 || req.Limit > 200 {
		return model.DirectoryListing{}, ErrInvalid
	}
	if s.directoryBrowser == nil {
		return model.DirectoryListing{}, ErrUnsupported
	}
	if req.Limit == 0 {
		req.Limit = 100
	}
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := s.directoryBrowser.ReadDirectory(bounded, ports.DirectoryReadRequest{Path: req.Path, After: req.After, Limit: req.Limit, IncludeHidden: req.IncludeHidden})
	if err != nil {
		return model.DirectoryListing{}, fail(ErrUnavailable, "directory cannot be browsed: %v", err)
	}
	return result, nil
}
