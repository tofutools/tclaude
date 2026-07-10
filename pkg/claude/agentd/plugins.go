package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common/executil"
)

// plugins.go backs the dashboard's "Plugins" tab — a human-managed
// registry of external integrations (MCP servers, sidecar containers,
// …) that tclaude agents rely on but tclaude itself doesn't own.
//
// The model is deliberately a flexible primitive, not a workflow: a
// plugin is just a name plus an ordered list of steps, and a step is a
// pair of shell commands —
//
//   - check: exit 0 means "this step is satisfied" (container running,
//     MCP registered, …). Optional; a step without one is run-only.
//   - run:   performs the step (docker run, claude mcp add, …).
//     Optional; a step without one is check-only.
//   - stop:  undoes the step (docker stop, claude mcp remove, …).
//     Optional; powers the plugin-level deactivate.
//
// That's enough to express "install X" (one-shot run, check = is it
// registered) and "keep Y running" (run = start it, check = is it up)
// without tclaude growing per-plugin knowledge. A built-in catalog
// ships ready-made definitions (currently the Excalidraw MCP) that the
// human can install — i.e. copy into their own plugins.json — and then
// edit freely.
//
// A plugin can be deactivated on demand: deactivate runs every step's
// stop command in reverse order and records the intent as a persisted
// Disabled flag; activate clears the flag and runs the steps forward
// (skipping ones whose check already passes). Disabled flips the
// status semantics — failing checks are then the EXPECTED state ("off")
// and only a stoppable step that is still active warns.
//
// Definitions persist in ~/.tclaude/plugins.json. Step statuses live
// only in memory: a background sweep re-checks every plugin each
// minute, and the snapshot serves the cached results so the 2s
// dashboard poll never spawns a subprocess. The Plugins nav button
// shows a warning badge when any plugin has a failing check — that is
// the "installed but not started" signal the tab exists for.
//
// Trust model: definitions are arbitrary shell run as the daemon's
// user. They are authored and triggered exclusively by the human
// through the cookie-authed dashboard (same trust as the Config tab
// editing config.json, or the human's own terminal). Agents have no
// route to create or run plugins.

// PluginStep is one step of a plugin definition. See the file comment
// for the check/run/stop semantics. At least one of check/run must be
// set; stop is always optional.
type PluginStep struct {
	Name  string `json:"name"`
	Check string `json:"check,omitempty"`
	Run   string `json:"run,omitempty"`
	Stop  string `json:"stop,omitempty"`
}

// Plugin is one plugin definition, as persisted in plugins.json.
// Disabled records the human's intent after a deactivate: checks keep
// running, but a failing check is then the expected state, not a
// warning. Only the activate/deactivate endpoints flip it — the edit
// modal's PUT preserves whatever is stored.
type Plugin struct {
	Name     string       `json:"name"`
	Descr    string       `json:"descr,omitempty"`
	Disabled bool         `json:"disabled,omitempty"`
	Steps    []PluginStep `json:"steps"`
}

// pluginsFile is the on-disk shape of ~/.tclaude/plugins.json. Wrapped
// in an object (not a bare array) so the format can grow fields
// without a breaking change.
type pluginsFile struct {
	Plugins []Plugin `json:"plugins"`
}

const (
	// pluginCheckTimeout bounds one check command. Checks are meant to
	// be probes (docker inspect, claude mcp get); a hung probe must not
	// stall the sweep or a synchronous re-check response for long.
	pluginCheckTimeout = 15 * time.Second
	// pluginRunTimeout bounds one run command. Runs can legitimately be
	// slow (first docker pull), so this is generous.
	pluginRunTimeout = 5 * time.Minute
	// pluginCheckInterval is the background sweep period. Statuses only
	// need to be fresh on a human timescale; the tab's "check" buttons
	// cover "I want to know right now".
	pluginCheckInterval = 60 * time.Second
	// pluginOutputMax caps the stored command output per step. Keeps
	// the snapshot payload bounded; the tail is kept because the end of
	// the output is where errors land.
	pluginOutputMax = 4096

	pluginNameMax    = 64
	pluginStepsMax   = 32
	pluginCommandMax = 4096
)

// pluginsPath returns the definitions file path
// (~/.tclaude/data/plugins.json — private daemon state).
func pluginsPath() string {
	return filepath.Join(config.DataDir(), "plugins.json")
}

// pluginsMu guards the read-modify-write cycle on plugins.json. The
// dashboard handlers are the only writers, but two browser tabs can
// race each other.
var pluginsMu sync.Mutex

// loadPlugins reads the definitions file. A missing file is an empty
// registry, not an error.
func loadPlugins() ([]Plugin, error) {
	data, err := os.ReadFile(pluginsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f pluginsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", pluginsPath(), err)
	}
	return f.Plugins, nil
}

// savePlugins writes the definitions file atomically (temp + rename),
// mirroring config.Save so a crash mid-write can't corrupt it.
func savePlugins(plugins []Plugin) error {
	// plugins.json lives in the private state dir (~/.tclaude/data). Write the
	// temp file there too so the rename is same-directory and never fails
	// because data/ does not exist yet.
	if err := os.MkdirAll(config.DataDir(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(pluginsFile{Plugins: plugins}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(config.DataDir(), "plugins-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, pluginsPath())
}

// pluginCatalog returns the built-in, ready-made definitions the
// Plugins tab offers for one-click install. Installing copies the
// entry into plugins.json, where the human can edit it like any other
// plugin — the catalog is a source of starting points, not a parallel
// registry.
func pluginCatalog() []Plugin {
	return []Plugin{
		{
			Name: "excalidraw-mcp",
			Descr: "Live Excalidraw canvas + MCP server (yctimlin/mcp_excalidraw). " +
				"Agents draw on a shared real-time canvas at http://127.0.0.1:3000 via 26 MCP tools.",
			Steps: []PluginStep{
				{
					Name:  "canvas server (docker)",
					Check: "docker inspect -f '{{.State.Running}}' mcp-excalidraw-canvas 2>/dev/null | grep -q true",
					Run: "docker start mcp-excalidraw-canvas 2>/dev/null || " +
						"docker run -d -p 3000:3000 --name mcp-excalidraw-canvas ghcr.io/yctimlin/mcp_excalidraw-canvas:latest",
					// stop, not rm — the container survives so the next
					// activate is a fast `docker start`.
					Stop: "docker stop mcp-excalidraw-canvas",
				},
				{
					Name:  "claude mcp registration (user scope)",
					Check: "claude mcp get excalidraw",
					Run: "claude mcp add excalidraw --scope user -- " +
						"docker run -i --rm --add-host=host.docker.internal:host-gateway " +
						"-e EXPRESS_SERVER_URL=http://host.docker.internal:3000 -e ENABLE_CANVAS_SYNC=true " +
						"ghcr.io/yctimlin/mcp_excalidraw:latest",
					Stop: "claude mcp remove excalidraw --scope user",
				},
			},
		},
	}
}

// -- status cache -------------------------------------------------------

// pluginStepResult is the cached outcome of one step's last check.
// The zero value means "never checked" (no check command, or the sweep
// hasn't reached it yet). Check records the command the verdict is FOR:
// a sweep works from a point-in-time load of plugins.json, so a result
// can land after the definition changed — pluginView only trusts a
// cached result whose Check matches the step's current command, which
// rejects those stale writes without a generation counter.
type pluginStepResult struct {
	Check     string
	OK        bool
	Output    string
	CheckedAt time.Time
}

// pluginStatusCache holds the last check results, keyed by plugin name
// with one slot per step (index-aligned with the definition's Steps).
// In-memory only: a daemon restart just means one sweep of staleness.
var pluginStatusCache = struct {
	sync.Mutex
	byPlugin map[string][]pluginStepResult
}{byPlugin: map[string][]pluginStepResult{}}

// tailBuffer is an io.Writer that retains only the last `max` bytes
// written, so a noisy plugin command can't grow the daemon's memory —
// the tail is kept because the end of the output is where errors land.
// No locking needed: it is assigned to BOTH Stdout and Stderr of the
// same exec.Cmd, and os/exec documents that when the two are the same
// writer at most one goroutine calls Write at a time.
type tailBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		copy(t.buf, t.buf[len(t.buf)-t.max:])
		t.buf = t.buf[:t.max]
		t.truncated = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	if t.truncated {
		return "…" + string(t.buf)
	}
	return string(t.buf)
}

// runPluginShell runs one plugin command through the user's shell with
// a timeout, returning combined output (tail-capped at pluginOutputMax
// WHILE streaming, never buffered whole) and whether it exited 0.
// executil kills the whole process group on timeout so a hung
// docker/claude child can't leak.
//
// A package var so tests exercising real catalog entries can stub the
// exec — running an actual `docker inspect` in a unit test would be
// neither hermetic nor fast.
var runPluginShell = func(command string, timeout time.Duration) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := executil.CommandContext(ctx, "sh", "-c", command)
	// Plugin steps routinely invoke `claude` itself (e.g. the catalog's
	// `claude mcp get` probes, every checker sweep). Those one-shot runs
	// execute the user's globally-installed tclaude hooks, feeding
	// throwaway sessions into the status pipeline — which fired an
	// "Exited" desktop notification per probe. Hook callbacks honour
	// TCLAUDE_IGNORE_HOOKS, so set it for everything a plugin step runs.
	cmd.Env = append(os.Environ(), "TCLAUDE_IGNORE_HOOKS=1")
	out := &tailBuffer{max: pluginOutputMax}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	s := out.String()
	if ctx.Err() == context.DeadlineExceeded {
		s += "\n(timed out after " + timeout.String() + ")"
	}
	return strings.TrimSpace(s), err == nil
}

// checkPlugin runs every check command of p and stores the results in
// the cache. Steps without a check keep a zero result. Synchronous —
// callers that must not block (snapshot) read the cache instead.
func checkPlugin(p Plugin) []pluginStepResult {
	results := make([]pluginStepResult, len(p.Steps))
	for i, s := range p.Steps {
		if s.Check == "" {
			continue
		}
		out, ok := runPluginShell(s.Check, pluginCheckTimeout)
		results[i] = pluginStepResult{Check: s.Check, OK: ok, Output: out, CheckedAt: time.Now()}
	}
	pluginStatusCache.Lock()
	pluginStatusCache.byPlugin[p.Name] = results
	pluginStatusCache.Unlock()
	return results
}

// checkAllPlugins sweeps every defined plugin (concurrently across
// plugins, sequentially within one — steps may depend on each other)
// and prunes cache entries whose plugin was deleted.
func checkAllPlugins() {
	plugins, err := loadPlugins()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, p := range plugins {
		wg.Add(1)
		go func(p Plugin) {
			defer wg.Done()
			checkPlugin(p)
		}(p)
	}
	wg.Wait()
	// Prune cache entries for deleted plugins against a FRESH load,
	// not the sweep's own point-in-time snapshot: a plugin created
	// while the sweep ran must not have its just-written first status
	// thrown away. (A residual window remains if a create lands right
	// here — the next sweep self-heals it within a minute.)
	fresh, err := loadPlugins()
	if err != nil {
		return
	}
	names := map[string]bool{}
	for _, p := range fresh {
		names[p.Name] = true
	}
	pluginStatusCache.Lock()
	for name := range pluginStatusCache.byPlugin {
		if !names[name] {
			delete(pluginStatusCache.byPlugin, name)
		}
	}
	pluginStatusCache.Unlock()
}

// startPluginChecker spins up the background status sweep. Mirrors
// startCronScheduler: one immediate tick so the dashboard has statuses
// right after startup, then timer-driven until stop closes.
func startPluginChecker(stop <-chan struct{}) {
	go func() {
		checkAllPlugins()
		t := time.NewTicker(pluginCheckInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				checkAllPlugins()
			}
		}
	}()
}

// -- wire shapes --------------------------------------------------------

// dashboardPluginStep is the snapshot/API view of one step: the
// definition fields plus the cached check outcome.
type dashboardPluginStep struct {
	Name  string `json:"name"`
	Check string `json:"check,omitempty"`
	Run   string `json:"run,omitempty"`
	Stop  string `json:"stop,omitempty"`
	// Checked: a check command exists AND has run at least once —
	// only then are OK/Output/CheckedAt meaningful.
	Checked   bool   `json:"checked"`
	OK        bool   `json:"ok"`
	Output    string `json:"output,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

// dashboardPlugin is the snapshot/API view of one plugin. Status is
// the aggregate the tab's pill + the nav badge key off. Enabled:
//
//	warn    — ≥1 check has run and failed ("installed but not active")
//	ok      — every check has run and passed (and there is ≥1)
//	unknown — no checks defined, or none have run yet
//
// Disabled (deactivated on purpose) inverts the alarm: failing checks
// are the expected state —
//
//	off  — nothing unexpected is active
//	warn — a step WITH a stop command still passes its check, i.e.
//	       deactivation didn't take (steps without a stop command are
//	       exempt: prerequisites like "docker installed" legitimately
//	       stay satisfied while the plugin is off)
type dashboardPlugin struct {
	Name     string                `json:"name"`
	Descr    string                `json:"descr,omitempty"`
	Disabled bool                  `json:"disabled,omitempty"`
	Status   string                `json:"status"`
	Steps    []dashboardPluginStep `json:"steps"`
}

// pluginView merges a definition with its cached check results into
// the wire shape. results may be shorter than Steps (edited plugin not
// yet re-swept) — missing slots render as never-checked.
func pluginView(p Plugin) dashboardPlugin {
	pluginStatusCache.Lock()
	results := pluginStatusCache.byPlugin[p.Name]
	pluginStatusCache.Unlock()

	v := dashboardPlugin{Name: p.Name, Descr: p.Descr, Disabled: p.Disabled, Status: "unknown", Steps: make([]dashboardPluginStep, len(p.Steps))}
	anyFailed, anyUnchecked, anyCheck, stoppableActive := false, false, false, false
	for i, s := range p.Steps {
		step := dashboardPluginStep{Name: s.Name, Check: s.Check, Run: s.Run, Stop: s.Stop}
		if s.Check != "" {
			anyCheck = true
			// Trust a cached slot only when it was produced FOR this
			// command — an in-flight sweep racing an edit can publish
			// results for a previous definition under the same name.
			if i < len(results) && !results[i].CheckedAt.IsZero() && results[i].Check == s.Check {
				r := results[i]
				step.Checked = true
				step.OK = r.OK
				step.Output = r.Output
				step.CheckedAt = r.CheckedAt.Format(time.RFC3339)
				if !r.OK {
					anyFailed = true
				}
				if r.OK && s.Stop != "" {
					stoppableActive = true
				}
			} else {
				anyUnchecked = true
			}
		}
		v.Steps[i] = step
	}
	switch {
	case p.Disabled && stoppableActive:
		v.Status = "warn" // deactivated, yet something stoppable still runs
	case p.Disabled:
		v.Status = "off"
	case anyFailed:
		v.Status = "warn"
	case anyCheck && !anyUnchecked:
		v.Status = "ok"
	}
	return v
}

// collectPluginsSnapshot builds the Plugins tab's snapshot rows from
// the definitions file + the status cache — no subprocess spawns, so
// it is safe on the 2s poll. Returns the rows, the count of plugins in
// "warn" (drives the nav-button badge), and any read/parse error on
// plugins.json — an unreadable registry must surface as an error, not
// masquerade as "no plugins installed".
func collectPluginsSnapshot() ([]dashboardPlugin, int, error) {
	out := []dashboardPlugin{}
	plugins, err := loadPlugins()
	if err != nil {
		return out, 0, err
	}
	warn := 0
	for _, p := range plugins {
		v := pluginView(p)
		if v.Status == "warn" {
			warn++
		}
		out = append(out, v)
	}
	return out, warn, nil
}

// -- validation ---------------------------------------------------------

// validatePlugin sanity-checks a submitted definition. Commands are
// deliberately NOT inspected — they are the human's own shell, same
// trust as their terminal — only bounded so a stray paste can't bloat
// the file/snapshot.
func validatePlugin(p Plugin) error {
	name := strings.TrimSpace(p.Name)
	switch {
	case name == "":
		return fmt.Errorf("plugin name is required")
	case len(name) > pluginNameMax:
		return fmt.Errorf("plugin name exceeds %d characters", pluginNameMax)
	case strings.ContainsAny(name, "/\\\n\r\t"):
		return fmt.Errorf("plugin name must not contain slashes or whitespace control characters")
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("a plugin needs at least one step")
	}
	if len(p.Steps) > pluginStepsMax {
		return fmt.Errorf("too many steps (max %d)", pluginStepsMax)
	}
	for i, s := range p.Steps {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("step %d: name is required", i+1)
		}
		if strings.TrimSpace(s.Check) == "" && strings.TrimSpace(s.Run) == "" {
			return fmt.Errorf("step %d (%s): needs a check command, a run command, or both", i+1, s.Name)
		}
		if len(s.Check) > pluginCommandMax || len(s.Run) > pluginCommandMax || len(s.Stop) > pluginCommandMax {
			return fmt.Errorf("step %d (%s): command exceeds %d characters", i+1, s.Name, pluginCommandMax)
		}
	}
	return nil
}

// normalizePlugin trims the cosmetic whitespace validatePlugin
// tolerated, so the stored definition is canonical.
func normalizePlugin(p Plugin) Plugin {
	p.Name = strings.TrimSpace(p.Name)
	p.Descr = strings.TrimSpace(p.Descr)
	for i := range p.Steps {
		p.Steps[i].Name = strings.TrimSpace(p.Steps[i].Name)
		p.Steps[i].Check = strings.TrimSpace(p.Steps[i].Check)
		p.Steps[i].Run = strings.TrimSpace(p.Steps[i].Run)
		p.Steps[i].Stop = strings.TrimSpace(p.Steps[i].Stop)
	}
	return p
}

// -- HTTP handlers ------------------------------------------------------

// registerDashboardPluginRoutes wires the cookie-authed /api/plugins
// endpoints onto the loopback mux:
//
//	GET    /api/plugins                       → definitions + statuses + catalog
//	POST   /api/plugins                       → create a plugin
//	PUT    /api/plugins/{name}                → replace a plugin (rename allowed)
//	DELETE /api/plugins/{name}                → delete a plugin
//	POST   /api/plugins/check                 → re-check all plugins now (sync)
//	POST   /api/plugins/{name}/check          → re-check one plugin now (sync)
//	POST   /api/plugins/{name}/activate       → run steps forward + clear Disabled (sync)
//	POST   /api/plugins/{name}/deactivate     → run stop commands in reverse + set Disabled (sync)
//	POST   /api/plugins/{name}/steps/{idx}/run  → run one step's run command (sync)
//	POST   /api/plugins/{name}/steps/{idx}/stop → run one step's stop command (sync)
//
// Dashboard-only, like /api/config: there is deliberately no /v1 twin,
// so agents cannot reach these. The literal `check` segment outranks
// the {name} wildcard in Go 1.22 routing, so a plugin can even be
// named "check" without ambiguity (it just can't be re-checked by URL
// — validatePlugin doesn't bother forbidding the word).
func registerDashboardPluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/plugins", dashboardPluginRoute(handlePluginsList))
	mux.HandleFunc("POST /api/plugins", dashboardPluginRoute(handlePluginsCreate))
	mux.HandleFunc("PUT /api/plugins/{name}", dashboardPluginRoute(handlePluginsUpdate))
	mux.HandleFunc("DELETE /api/plugins/{name}", dashboardPluginRoute(handlePluginsDelete))
	mux.HandleFunc("POST /api/plugins/check", dashboardPluginRoute(handlePluginsCheckAll))
	mux.HandleFunc("POST /api/plugins/{name}/check", dashboardPluginRoute(handlePluginsCheckOne))
	mux.HandleFunc("POST /api/plugins/{name}/activate", dashboardPluginRoute(handlePluginsActivate))
	mux.HandleFunc("POST /api/plugins/{name}/deactivate", dashboardPluginRoute(handlePluginsDeactivate))
	mux.HandleFunc("POST /api/plugins/{name}/steps/{idx}/run", dashboardPluginRoute(handlePluginsRunStep))
	mux.HandleFunc("POST /api/plugins/{name}/steps/{idx}/stop", dashboardPluginRoute(handlePluginsStopStep))
}

// dashboardPluginRoute applies the dashboard cookie/Origin auth, same
// as every other /api route.
func dashboardPluginRoute(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		fn(w, r)
	}
}

// pluginsListResponse is the GET /api/plugins (and check-all) body.
type pluginsListResponse struct {
	Plugins []dashboardPlugin `json:"plugins"`
	Catalog []Plugin          `json:"catalog"`
	Warn    int               `json:"warn"`
}

// writeCurrentPlugins writes the full list response, or a 500 when the
// registry file is unreadable — the synchronous endpoints must report
// a broken plugins.json, not an innocent-looking empty list. (The
// snapshot poll instead degrades gracefully via PluginsError.)
func writeCurrentPlugins(w http.ResponseWriter) {
	plugins, warn, err := collectPluginsSnapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, pluginsListResponse{Plugins: plugins, Catalog: pluginCatalog(), Warn: warn})
}

func handlePluginsList(w http.ResponseWriter, _ *http.Request) {
	writeCurrentPlugins(w)
}

// decodePluginBody parses a request body into a Plugin and validates
// it. Writes the 400 itself; the bool says "proceed".
func decodePluginBody(w http.ResponseWriter, r *http.Request) (Plugin, bool) {
	var p Plugin
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad JSON body: "+err.Error(), http.StatusBadRequest)
		return p, false
	}
	p = normalizePlugin(p)
	if err := validatePlugin(p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return p, false
	}
	return p, true
}

func handlePluginsCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := decodePluginBody(w, r)
	if !ok {
		return
	}
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	plugins, err := loadPlugins()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, existing := range plugins {
		if existing.Name == p.Name {
			http.Error(w, "a plugin named "+p.Name+" already exists", http.StatusConflict)
			return
		}
	}
	plugins = append(plugins, p)
	if err := savePlugins(plugins); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// First statuses arrive asynchronously — the response must not
	// hang on a slow probe; the 2s snapshot poll picks the result up.
	goBackground(func() { checkPlugin(p) })
	writeJSON(w, http.StatusCreated, pluginView(p))
}

func handlePluginsUpdate(w http.ResponseWriter, r *http.Request) {
	oldName := r.PathValue("name")
	p, ok := decodePluginBody(w, r)
	if !ok {
		return
	}
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	plugins, err := loadPlugins()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	idx := -1
	for i, existing := range plugins {
		switch existing.Name {
		case oldName:
			idx = i
		case p.Name:
			http.Error(w, "a plugin named "+p.Name+" already exists", http.StatusConflict)
			return
		}
	}
	if idx < 0 {
		http.Error(w, "no plugin named "+oldName, http.StatusNotFound)
		return
	}
	// Enablement is owned by the activate/deactivate endpoints — an
	// edit (whose modal doesn't carry the flag) must not silently
	// re-enable a deactivated plugin.
	p.Disabled = plugins[idx].Disabled
	plugins[idx] = p
	if err := savePlugins(plugins); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Drop stale results — the steps may have changed shape — then
	// re-check in the background, same non-blocking deal as create.
	pluginStatusCache.Lock()
	delete(pluginStatusCache.byPlugin, oldName)
	delete(pluginStatusCache.byPlugin, p.Name)
	pluginStatusCache.Unlock()
	goBackground(func() { checkPlugin(p) })
	writeJSON(w, http.StatusOK, pluginView(p))
}

func handlePluginsDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	plugins, err := loadPlugins()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	kept := plugins[:0]
	for _, p := range plugins {
		if p.Name != name {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(plugins) {
		http.Error(w, "no plugin named "+name, http.StatusNotFound)
		return
	}
	if err := savePlugins(kept); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pluginStatusCache.Lock()
	delete(pluginStatusCache.byPlugin, name)
	pluginStatusCache.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

// handlePluginsCheckAll re-probes every plugin synchronously — the
// human pressed "check now" and wants the verdict in the response.
func handlePluginsCheckAll(w http.ResponseWriter, _ *http.Request) {
	checkAllPlugins()
	writeCurrentPlugins(w)
}

// findPlugin resolves {name} against the definitions file. Writes the
// error response itself; ok==false means "stop".
func findPlugin(w http.ResponseWriter, name string) (Plugin, bool) {
	plugins, err := loadPlugins()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return Plugin{}, false
	}
	for _, p := range plugins {
		if p.Name == name {
			return p, true
		}
	}
	http.Error(w, "no plugin named "+name, http.StatusNotFound)
	return Plugin{}, false
}

func handlePluginsCheckOne(w http.ResponseWriter, r *http.Request) {
	p, ok := findPlugin(w, r.PathValue("name"))
	if !ok {
		return
	}
	checkPlugin(p)
	writeJSON(w, http.StatusOK, pluginView(p))
}

// pluginRunResponse is the body of every command-executing endpoint
// (run-step, stop-step, activate, deactivate): the commands' own
// outcome plus the plugin's freshly re-checked state, so the UI can
// show "ran OK, and the check now passes" in one round-trip. Ran is
// false when nothing needed executing (e.g. activate of an
// already-satisfied plugin just clears the Disabled flag).
type pluginRunResponse struct {
	Ran    bool            `json:"ran"`
	OK     bool            `json:"ok"`
	Output string          `json:"output,omitempty"`
	Plugin dashboardPlugin `json:"plugin"`
}

// handlePluginsStepCommand is the shared run-step/stop-step body:
// resolve {name}/{idx}, pick the step's command via sel, execute it,
// re-probe the whole plugin (one step's run often flips a later step's
// check — start container → registration check can connect).
func handlePluginsStepCommand(w http.ResponseWriter, r *http.Request, verb string, sel func(PluginStep) string) {
	p, ok := findPlugin(w, r.PathValue("name"))
	if !ok {
		return
	}
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil || idx < 0 || idx >= len(p.Steps) {
		http.Error(w, "step index out of range", http.StatusBadRequest)
		return
	}
	step := p.Steps[idx]
	cmd := sel(step)
	if cmd == "" {
		http.Error(w, "step "+step.Name+" has no "+verb+" command", http.StatusBadRequest)
		return
	}
	out, runOK := runPluginShell(cmd, pluginRunTimeout)
	checkPlugin(p)
	writeJSON(w, http.StatusOK, pluginRunResponse{Ran: true, OK: runOK, Output: out, Plugin: pluginView(p)})
}

func handlePluginsRunStep(w http.ResponseWriter, r *http.Request) {
	handlePluginsStepCommand(w, r, "run", func(s PluginStep) string { return s.Run })
}

func handlePluginsStopStep(w http.ResponseWriter, r *http.Request) {
	handlePluginsStepCommand(w, r, "stop", func(s PluginStep) string { return s.Stop })
}

// setPluginDisabled persists the Disabled flag for one plugin under
// the registry mutex and returns the updated definition. Kept separate
// from the slow command execution so the lock is never held across a
// shell run.
func setPluginDisabled(name string, disabled bool) (Plugin, error) {
	pluginsMu.Lock()
	defer pluginsMu.Unlock()
	plugins, err := loadPlugins()
	if err != nil {
		return Plugin{}, err
	}
	for i := range plugins {
		if plugins[i].Name == name {
			plugins[i].Disabled = disabled
			if err := savePlugins(plugins); err != nil {
				return Plugin{}, err
			}
			return plugins[i], nil
		}
	}
	return Plugin{}, fmt.Errorf("no plugin named %s", name)
}

// appendStepOutput accumulates one executed step's output into the
// combined response body, prefixed with the step name so a multi-step
// activate/deactivate stays readable.
func appendStepOutput(b *strings.Builder, stepName, out string) {
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString("[")
	b.WriteString(stepName)
	b.WriteString("] ")
	if out == "" {
		out = "(no output)"
	}
	b.WriteString(out)
}

// handlePluginsActivate brings a plugin up on demand: clear the
// Disabled flag, then walk the steps IN ORDER running each one whose
// check doesn't already pass (steps may depend on their predecessors,
// so the walk aborts on the first failed run). Skipping satisfied
// steps makes activate cheap and idempotent — re-activating a healthy
// plugin executes nothing.
func handlePluginsActivate(w http.ResponseWriter, r *http.Request) {
	p, err := setPluginDisabled(r.PathValue("name"), false)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.HasPrefix(err.Error(), "no plugin named") {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	var combined strings.Builder
	ran, allOK := false, true
	for _, s := range p.Steps {
		if s.Run == "" {
			continue
		}
		if s.Check != "" {
			if _, ok := runPluginShell(s.Check, pluginCheckTimeout); ok {
				continue // already satisfied
			}
		}
		out, ok := runPluginShell(s.Run, pluginRunTimeout)
		ran = true
		appendStepOutput(&combined, s.Name, out)
		if !ok {
			allOK = false
			break // later steps likely depend on this one
		}
	}
	checkPlugin(p)
	writeJSON(w, http.StatusOK, pluginRunResponse{Ran: ran, OK: allOK, Output: strings.TrimSpace(combined.String()), Plugin: pluginView(p)})
}

// handlePluginsDeactivate takes a plugin down on demand: set the
// Disabled flag (so the badge stops nagging even while commands run),
// then walk the steps IN REVERSE running each stop command whose check
// still passes. Teardown is best-effort — a failing stop doesn't abort
// the walk, it just flips the response's OK so the human looks at the
// output.
func handlePluginsDeactivate(w http.ResponseWriter, r *http.Request) {
	p, err := setPluginDisabled(r.PathValue("name"), true)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.HasPrefix(err.Error(), "no plugin named") {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	var combined strings.Builder
	ran, allOK := false, true
	for i := len(p.Steps) - 1; i >= 0; i-- {
		s := p.Steps[i]
		if s.Stop == "" {
			continue
		}
		if s.Check != "" {
			if _, ok := runPluginShell(s.Check, pluginCheckTimeout); !ok {
				continue // already down
			}
		}
		out, ok := runPluginShell(s.Stop, pluginRunTimeout)
		ran = true
		appendStepOutput(&combined, s.Name, out)
		if !ok {
			allOK = false
		}
	}
	checkPlugin(p)
	writeJSON(w, http.StatusOK, pluginRunResponse{Ran: ran, OK: allOK, Output: strings.TrimSpace(combined.String()), Plugin: pluginView(p)})
}
