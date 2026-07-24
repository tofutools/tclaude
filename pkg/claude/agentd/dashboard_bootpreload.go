package agentd

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// The dashboard is a no-build native-ES-module app: dashboard.js statically
// imports a transitive graph of ~70 modules, and the browser discovers each one
// only AFTER parsing its importer — a deep discover→parse→fetch→repeat waterfall
// that runs on every load because /static/ assets are served Cache-Control:
// no-store (see handleDashboardStatic). That waterfall is the bulk of the
// "Loading dashboard…" wait.
//
// bootPreloadModules computes that static entry graph once at init so the page
// can emit a <link rel="modulepreload"> for every module in it. The browser then
// fetches the whole graph in parallel instead of serially, and — because the set
// is now known and finite — the boot curtain can show an honest N-of-M progress
// bar (see the inline counter in dashboard.html) instead of an indefinite label.
//
// Only STATIC imports are followed: dynamically imported (import(...)) island
// modules load lazily per tab and must stay off the boot path. Paths that don't
// resolve to a real embedded file (a "from '...'" that is actually inside a
// string or comment) are dropped, so the emitted set never 404s.

// bootStaticImportRe matches a static relative import/export specifier:
//
//	import x from './m.js'   import { a } from '../m.js'   import './m.js'
//	export { a } from './m.js'   export * from './m.js'
//
// Both ./ and ../ are followed (today every module is flat under js/, but a
// future ../ import must not silently drop out of the graph and waterfall).
// path.Join + the FS existence check below resolve and validate the target.
//
// It deliberately does NOT match dynamic import('./m.js'): after `import` a
// dynamic form has `(`, which the `\s*['"]` here cannot consume. `\bfrom` /
// `\bimport` word boundaries keep it off `Array.from(` and identifiers like
// `dateFrom`.
var bootStaticImportRe = regexp.MustCompile(`(?:\bfrom|\bimport)\s*['"](\.\.?\/[^'"]+)['"]`)

// bootVendorPreload lists the import-map vendor targets (bare specifiers like
// "preact" / "@preact/signals" resolve to these). They sit outside the relative
// crawl but are pulled in eagerly by the entry graph, so they belong in both the
// preload set and the progress denominator. Kept in sync with the import map in
// dashboard.html.
var bootVendorPreload = []string{
	"vendor/preact/preact.module.js",
	"vendor/preact/hooks.module.js",
	"vendor/preact/signals-core.module.js",
	"vendor/preact/signals.module.js",
	"vendor/preact/htm.module.js",
}

// bootPreloadEntry is the entry module the SPA's <script type="module"> loads.
const bootPreloadEntry = "js/dashboard.js"

// bootPreloadModules returns the ordered list of embedded asset paths (relative
// to dashboardAssetsFS, e.g. "js/foo.js") to modulepreload on boot: the static
// import graph rooted at dashboard.js plus the vendor import-map targets. The
// order is deterministic (vendor first, then the js graph sorted) so the emitted
// HTML and its tests are stable.
func bootPreloadModules(fsys fs.FS) []string {
	visited := map[string]bool{}
	var stack []string
	if _, err := fs.ReadFile(fsys, bootPreloadEntry); err == nil {
		stack = append(stack, bootPreloadEntry)
	}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		src, err := fs.ReadFile(fsys, cur)
		if err != nil {
			continue
		}
		dir := path.Dir(cur)
		for _, m := range bootStaticImportRe.FindAllSubmatch(src, -1) {
			dep := path.Join(dir, string(m[1]))
			if visited[dep] {
				continue
			}
			// Drop specifiers that don't resolve to a real embedded file:
			// a "from '...'" caught inside a string or comment. Validating
			// existence here is what guarantees the emitted set never 404s.
			if _, err := fs.ReadFile(fsys, dep); err != nil {
				continue
			}
			stack = append(stack, dep)
		}
	}

	jsGraph := make([]string, 0, len(visited))
	for m := range visited {
		jsGraph = append(jsGraph, m)
	}
	sort.Strings(jsGraph)

	out := make([]string, 0, len(bootVendorPreload)+len(jsGraph))
	for _, v := range bootVendorPreload {
		if _, err := fs.ReadFile(fsys, v); err == nil {
			out = append(out, v)
		}
	}
	out = append(out, jsGraph...)
	return out
}

// bootPreloadMarker is the placeholder in dashboard.html <head> that
// injectBootPreload replaces with the preload <link>s and the module-total
// script the inline progress counter reads.
const bootPreloadMarker = "<!--BOOT_PRELOAD-->"

// injectBootPreload replaces bootPreloadMarker in the dashboard HTML with a
// modulepreload block for the boot import graph and a script that publishes the
// module total to the inline progress counter. It panics if the marker is
// absent — a build-time invariant guarded by a test, not a runtime condition.
func injectBootPreload(html []byte, fsys fs.FS) []byte {
	if !strings.Contains(string(html), bootPreloadMarker) {
		panic("agentd: dashboard.html is missing the " + bootPreloadMarker + " boot-preload marker")
	}
	modules := bootPreloadModules(fsys)
	var b strings.Builder
	// The counter denominator must be set before the inline observer script
	// runs; both live in <head>, in source order.
	fmt.Fprintf(&b, "<script>window.__BOOT_MODULE_TOTAL__=%d;</script>\n", len(modules))
	for _, m := range modules {
		fmt.Fprintf(&b, `<link rel="modulepreload" href="/static/%s">`+"\n", m)
	}
	return []byte(strings.Replace(string(html), bootPreloadMarker, b.String(), 1))
}
