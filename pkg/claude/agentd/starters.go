package agentd

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Bundled starter task forces (JOH-246). tclaude ships a small library of
// curated, ready-to-run team blueprints so the task-force feature is usable and
// demoable on day one — a fresh install can deploy a working squad in one verb.
//
// A starter is nothing special on disk: it is a portable task-force export
// (the SAME `tclaude-task-force` envelope that `templates export` writes and
// `templates import` reads), embedded in the binary. Installing one runs
// through the EXACT SAME import path a shared/hand-written export would
// (importTemplateEnvelope) — there is no second importer. So a starter is also
// a worked example of the whole feature set: role_ref to the seed roles,
// per-agent launch tuning, a process, staged-spawn waves, seeded rhythms, and a
// routed work pattern.
//
// The starters reference the canonical SEED roles (po/lead/dev/designer/
// reviewer/tester), which tclaude re-seeds on every DB open (see
// db.ensureSeededRoles), so they carry no embedded role definitions and install
// cleanly on a fresh empty DB with zero role warnings. (The envelope's `roles`
// embed is for a template that invents its OWN custom roles; the bundled
// starters deliberately don't.)
//
// Endpoints (starters live under their own /v1/starters prefix, not under
// /v1/templates/, to avoid a Go 1.22 ServeMux pattern conflict between a
// hypothetical /v1/templates/starters/{name} and the existing
// /v1/templates/{name}/{export,instantiate,deploy} routes):
//
//	GET  /v1/starters                  → list the bundled starters (open, read-only)
//	GET  /v1/starters/{name}           → one starter's inner template JSON (open, read-only)
//	POST /v1/starters/{name}/install   → install it locally (templates.manage; ?as=<name>)

//go:embed starters/*.json
var startersFS embed.FS

// starter is one bundled starter task force: its stable name (the inner
// template's name) plus the parsed portable envelope it installs from.
type starter struct {
	Name     string
	Envelope templateExportEnvelope
}

// startersOnce caches the parsed embedded starters — they are compile-time
// constant, so parsing once and reusing is safe across the process lifetime.
var (
	startersOnce  sync.Once
	startersValue []starter
	startersErr   error
)

// loadStarters returns every embedded starter, parsed once and cached (sorted
// by name). An embedded file that isn't a well-formed task-force envelope is a
// build-data bug: it surfaces as an error here (and a flow test installs each
// starter on a fresh DB through the real validator, so schema drift fails CI).
func loadStarters() ([]starter, error) {
	startersOnce.Do(func() { startersValue, startersErr = parseStarters() })
	return startersValue, startersErr
}

func parseStarters() ([]starter, error) {
	entries, err := fs.ReadDir(startersFS, "starters")
	if err != nil {
		return nil, fmt.Errorf("read starters dir: %w", err)
	}
	out := make([]starter, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := startersFS.ReadFile("starters/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read starter %s: %w", e.Name(), err)
		}
		var env templateExportEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("starter %s: not valid task-force JSON: %w", e.Name(), err)
		}
		name := strings.TrimSpace(env.Template.Name)
		if name == "" {
			return nil, fmt.Errorf("starter %s: template.name is empty", e.Name())
		}
		out = append(out, starter{Name: name, Envelope: env})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// getStarter returns the bundled starter of the given name, or nil if none.
func getStarter(name string) (*starter, error) {
	list, err := loadStarters()
	if err != nil {
		return nil, err
	}
	want := strings.TrimSpace(name)
	for i := range list {
		if list[i].Name == want {
			return &list[i], nil
		}
	}
	return nil, nil
}

// starterSummary is the list-view shape for a bundled starter: enough for the
// CLI table and the dashboard's starters section to describe it without
// fetching the whole template.
type starterSummary struct {
	Name        string `json:"name"`
	Descr       string `json:"descr,omitempty"`
	Agents      int    `json:"agents"`
	Waves       int    `json:"waves"`
	Rhythms     int    `json:"rhythms"`
	Process     int    `json:"process"`
	WorkPattern int    `json:"work_pattern"`
}

func starterSummaryOf(s starter) starterSummary {
	t := s.Envelope.Template
	waves := map[int]bool{}
	for _, a := range t.Agents {
		waves[a.Wave] = true
	}
	return starterSummary{
		Name:        s.Name,
		Descr:       t.Descr,
		Agents:      len(t.Agents),
		Waves:       len(waves),
		Rhythms:     len(t.Rhythms),
		Process:     len(t.Process),
		WorkPattern: len(t.WorkPattern),
	}
}

// handleStarters serves GET /v1/starters: the bundled starter list. Open and
// read-only, like GET /v1/templates — the starters are shipped data.
func handleStarters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	list, err := loadStarters()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := make([]starterSummary, 0, len(list))
	for _, s := range list {
		out = append(out, starterSummaryOf(s))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleStarterByName serves GET /v1/starters/{name}: one starter's inner
// template JSON — the same shape GET /v1/templates/{name} returns, so the CLI's
// `starters show` renders it exactly like `templates show`.
func handleStarterByName(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	s, err := getStarter(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if s == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such starter")
		return
	}
	writeJSON(w, http.StatusOK, s.Envelope.Template)
}

// starterInstallResult reports the outcome of installing a bundled starter.
// Installed is false when the install was skipped because a template of that
// name already exists (idempotent, never-clobber); Warnings is always non-nil
// so a CLI/JS consumer can range over it safely.
type starterInstallResult struct {
	Name      string   `json:"name"`
	Installed bool     `json:"installed"`
	Skipped   bool     `json:"skipped,omitempty"`
	Message   string   `json:"message,omitempty"`
	Warnings  []string `json:"warnings"`
}

// handleStarterInstall serves POST /v1/starters/{name}/install: install a
// bundled starter locally through the shared import path. Gated on
// templates.manage (it writes a template, exactly like import/create).
//
// Query knob:
//   - as=<name>   install under a different name (install a fresh copy, or
//     sidestep a collision with an existing template).
//
// Idempotent + never clobbers: if a template of the target name already exists,
// the install is SKIPPED (a user's edited copy is sacred) and reported as such
// — never overwritten. Re-running install is therefore safe, and works on a
// fresh empty DB (the demo path).
func handleStarterInstall(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermTemplatesManage); !ok {
		return
	}
	s, err := getStarter(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if s == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such starter")
		return
	}

	asName := strings.TrimSpace(r.URL.Query().Get("as"))
	// Starters never overwrite: install is skip-or-rename, so update is always
	// false. importTemplateEnvelope leaves an existing template untouched and
	// reports the collision; we turn that into a friendly skip.
	res, existed, fail := importTemplateEnvelope(s.Envelope, asName, false)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if existed {
		writeJSON(w, http.StatusOK, starterInstallResult{
			Name:      res.Imported,
			Installed: false,
			Skipped:   true,
			Message: fmt.Sprintf(
				"a template named %q already exists — skipped (your copy is left untouched; pass a different name to install a fresh copy)",
				res.Imported),
			Warnings: []string{},
		})
		return
	}
	warnings := res.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusCreated, starterInstallResult{
		Name:      res.Imported,
		Installed: true,
		Warnings:  warnings,
	})
}
