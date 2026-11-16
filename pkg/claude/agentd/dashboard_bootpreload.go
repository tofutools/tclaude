package agentd

import (
	"encoding/json"
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
// Only STATIC imports are followed, with one exception: the islands a boot
// awaits before anything else shows load by dynamic import(), and their graphs
// are added as further roots. Those differ by layout (tk_33321868): both
// layouts await the shell, then a desktop awaits the Jobs island and a phone
// the phone Jobs island and the phone shell. So the preload is layout-aware:
// the graph both share is emitted as <link>s, and a small inline script, which
// runs after js/layout-boot.js has decided the layout, adds the links of the
// layout in force (bootLayoutIslands). Other islands' dynamic imports load
// lazily per tab and stay off the boot path. Paths that don't resolve to a
// real embedded file (a "from '...'" that is actually inside a string or
// comment) are dropped, so the emitted set never 404s.

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
	"third_party/preact/preact.module.js",
	"third_party/preact/hooks.module.js",
	"third_party/preact/signals-core.module.js",
	"third_party/preact/signals.module.js",
	"third_party/preact/htm.module.js",
}

// bootPreloadEntry is the entry module the SPA's <script type="module"> loads.
const bootPreloadEntry = "js/dashboard.js"

// bootIslandLoader is the module that defines the island descriptors.
const bootIslandLoader = "js/preact-loader.js"

// bootIslandDescriptors names the island descriptors (in bootIslandLoader)
// that every boot awaits before the other features mount: the shell
// ("Starting shell…"). Their load() bodies reach their modules by dynamic
// import(), so without preloading them each boot walks their graphs as a
// discover→parse→fetch waterfall.
var bootIslandDescriptors = []string{"shellDescriptor"}

// bootLayoutIslands are the descriptors a boot awaits in one layout only: the
// desktop Jobs island; on a phone, the phone Jobs island (awaited in its
// place) and the phone shell (the top bar, bottom nav and Now view the phone
// boots into). Keyed by the layout js/layout-boot.js marks on <html>. A
// descriptor's every import() is a root, so the phone shell's also brings the
// few modules of its agent menu (phone-agent-menu.js and its graph), which
// load later: a small price against parsing which import() runs when.
var bootLayoutIslands = map[string][]string{
	"desktop": {"jobsDescriptor"},
	"phone":   {"phoneJobsDescriptor", "phoneShellDescriptor"},
}

// bootDynamicImportRe matches a dynamic import of a relative module.
var bootDynamicImportRe = regexp.MustCompile(`\bimport\(\s*['"](\.\.?\/[^'"]+)['"]\s*\)`)

// bootIslandRoots returns the modules the named descriptors dynamically
// import: the text from "const <name> = createIslandDescriptor(" to the next
// descriptor (or the end of the file).
func bootIslandRoots(fsys fs.FS, names []string) []string {
	src, err := fs.ReadFile(fsys, bootIslandLoader)
	if err != nil {
		return nil
	}
	text := string(src)
	var roots []string
	for _, name := range names {
		start := strings.Index(text, "const "+name+" = createIslandDescriptor(")
		if start < 0 {
			continue
		}
		const call = "createIslandDescriptor("
		body := text[start+strings.Index(text[start:], call)+len(call):]
		if next := strings.Index(body, call); next >= 0 {
			body = body[:next]
		}
		for _, m := range bootDynamicImportRe.FindAllStringSubmatch(body, -1) {
			roots = append(roots, path.Join(path.Dir(bootIslandLoader), m[1]))
		}
	}
	return roots
}

// bootPreloadModules returns the ordered list of embedded asset paths (relative
// to dashboardAssetsFS, e.g. "js/foo.js") to modulepreload on a boot in layout
// ("desktop" or "phone"): the shared set, then the layout's own. The order is
// deterministic (vendor first, then the js graphs sorted) so the emitted HTML
// and its tests are stable.
func bootPreloadModules(fsys fs.FS, layout string) []string {
	common, own := bootPreloadSets(fsys, layout)
	return append(common, own...)
}

// bootPreloadSets splits a layout's preload into the set every layout shares
// (the vendor targets and the static graph of dashboard.js and the shell) and
// the modules only that layout's boot islands add.
func bootPreloadSets(fsys fs.FS, layout string) (common, own []string) {
	shared := bootImportGraph(fsys, append([]string{bootPreloadEntry}, bootIslandRoots(fsys, bootIslandDescriptors)...))
	for _, v := range bootVendorPreload {
		if _, err := fs.ReadFile(fsys, v); err == nil {
			common = append(common, v)
		}
	}
	common = append(common, shared...)
	seen := map[string]bool{}
	for _, m := range shared {
		seen[m] = true
	}
	for _, m := range bootImportGraph(fsys, bootIslandRoots(fsys, bootLayoutIslands[layout])) {
		if !seen[m] {
			own = append(own, m)
		}
	}
	return common, own
}

// bootImportGraph is the sorted static import graph reachable from roots.
func bootImportGraph(fsys fs.FS, roots []string) []string {
	visited := map[string]bool{}
	var stack []string
	for _, root := range roots {
		if _, err := fs.ReadFile(fsys, root); err == nil {
			stack = append(stack, root)
		}
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
	return jsGraph
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
	common, desktop := bootPreloadSets(fsys, "desktop")
	_, phone := bootPreloadSets(fsys, "phone")
	var b strings.Builder
	for _, m := range common {
		fmt.Fprintf(&b, `<link rel="modulepreload" href="/static/%s">`+"\n", m)
	}
	// The layout's own modules, added once js/layout-boot.js (a synchronous
	// script above) has marked the layout. The links and the total are in
	// place before the inline progress counter below reads them.
	// JSON, which escapes <, > and &, so a list can never close the script.
	// Each path is a "/static/… string, which versionStaticRefs versions.
	list := func(mods []string) string {
		hrefs := make([]string, len(mods))
		for i, m := range mods {
			hrefs[i] = "/static/" + m
		}
		out, err := json.Marshal(hrefs)
		if err != nil {
			panic(err)
		}
		return string(out)
	}
	fmt.Fprintf(&b, "<script>(function(){var own=document.documentElement.getAttribute('data-layout')==='phone'?%s:%s;"+
		"for(var i=0;i<own.length;i++){var l=document.createElement('link');l.rel='modulepreload';l.href=own[i];document.head.appendChild(l);}"+
		"window.__BOOT_MODULE_TOTAL__=%d+own.length;})();</script>\n", list(phone), list(desktop), len(common))
	return []byte(strings.Replace(string(html), bootPreloadMarker, b.String(), 1))
}
