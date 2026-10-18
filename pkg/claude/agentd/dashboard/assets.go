// Package dashboard embeds the dashboard's static assets: the page shells,
// stylesheets, ES modules, sprites and vendored runtimes that agentd serves.
//
// It is its own package, not an embed inside agentd, so that what reads these
// files depends on them alone. pkg/claude/agentd/jstest imports it to make
// the asset bytes part of its test binary's build ID; through agentd it would
// also depend on every package agentd imports, and PR CI would re-run the
// Node suites for Go changes they cannot observe.
//
// The patterns are listed rather than `*`, which would also embed this file.
// TestFSCoversTheDirectory fails when a new top-level entry is left out.
package dashboard

import "embed"

// FS is the dashboard tree, addressed as "dashboard.html", "js/...".
//
//go:embed *.css *.html *.svg js sprites third_party
var FS embed.FS
