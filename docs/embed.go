// Package docs carries the user documentation into the binary, so the Steward's
// knowledge base (pkg/claude/knowledge) reads the pages this build shipped with
// even with no checkout and no network. Screenshots stay out to keep the binary
// small, and so do the preliminary v2 designs, which are not guidance.
package docs

import "embed"

//go:embed *.md examples learn paf pwf server
var FS embed.FS
