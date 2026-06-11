package agentd

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// This file owns the user-level Claude Code default model — the
// "model" key in ~/.claude/settings.json. That key is what `claude`
// falls back to when launched without --model, so it is the bottom of
// tclaude's spawn-model resolution chain:
//
//	explicit spawn model > group default_model > settings.json model
//	> claude's built-in default
//
// The value is stored verbatim, exactly as the human would type it to
// `claude --model`: an alias ("sonnet", "fable[1m]", "opusplan") or a
// full model ID ("claude-fable-5[1m]"). Claude Code resolves either
// form through the same alias table at startup, so tclaude never
// translates — it only validates the shape (clcommon.ValidateModel)
// and reports what is there.

// readUserDefaultModel returns the "model" value from the user-level
// Claude Code settings.json, or "" when the file, the key, or the
// home dir is missing — all of which mean "claude decides on its
// own". Read fresh on every call: the file is tiny, the human can
// edit it (or Claude Code can rewrite it) at any time, and the
// dashboard snapshot wants the current truth, not a daemon-lifetime
// cache.
func readUserDefaultModel() string {
	path := session.ClaudeSettingsPath()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var tree struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(data, &tree); err != nil {
		return ""
	}
	return strings.TrimSpace(tree.Model)
}

// writeUserDefaultModel sets (model != "") or removes (model == "")
// the "model" key in the user-level settings.json, preserving every
// other key and the file's permission bits (a private 0600 settings
// file stays 0600 — same care as setup's sandbox hardening). A
// missing file is created with just the one key; clearing the key in
// a missing file is a no-op. The caller has already validated model.
func writeUserDefaultModel(model string) error {
	path := session.ClaudeSettingsPath()
	if path == "" {
		return os.ErrNotExist
	}

	tree := map[string]any{}
	mode := os.FileMode(0o644)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if info, statErr := os.Stat(path); statErr == nil {
			mode = info.Mode().Perm()
		}
		// A corrupt settings.json must fail the write rather than be
		// silently replaced by {"model": ...} — the file also carries
		// hooks, permissions, sandbox config.
		if err := json.Unmarshal(data, &tree); err != nil {
			return err
		}
		if tree == nil { // file held a literal `null`
			tree = map[string]any{}
		}
	case os.IsNotExist(err):
		if model == "" {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	default:
		return err
	}

	if model == "" {
		delete(tree, "model")
	} else {
		tree["model"] = model
	}

	out, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), mode)
}

// handleClaudeDefaultModel serves /v1/claude-settings/default-model:
//
//   - GET returns {"model": "<current>"} — "" when unset. Open to any
//     identified caller: the value is a model name, not a secret, and
//     agents may legitimately want to know what a default-model spawn
//     would run.
//   - PUT {"model": "<alias-or-id>"} sets it; {"model": ""} clears it
//     (removes the key so claude falls back to its built-in default).
//     Gated on the settings.default-model slug — this rewrites a file
//     in the human's ~/.claude, so it is not default-granted
//     (effectively human-only unless the human grants it).
//
// The dashboard's cookie-authed twin is /api/claude-settings/
// default-model (registerDashboardEditRoutes).
func handleClaudeDefaultModel(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"model": readUserDefaultModel()})
	case http.MethodPut:
		if _, ok := requirePermission(w, r, PermSettingsDefaultModel); !ok {
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "json", err.Error())
			return
		}
		model, err := clcommon.ValidateModel(body.Model)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_model", err.Error())
			return
		}
		if err := writeUserDefaultModel(model); err != nil {
			writeError(w, http.StatusInternalServerError, "io",
				"update "+session.ClaudeSettingsPath()+": "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"model": model})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or PUT")
	}
}

// handleDashboardClaudeDefaultModel is the cookie-auth twin of
// /v1/claude-settings/default-model. The cookie + Origin pin is the
// human-consent layer; the shared handler then sees a classHuman
// caller via asDashboardHumanPeer, so the settings.default-model slug
// stays structurally enforced on every path.
func handleDashboardClaudeDefaultModel(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	handleClaudeDefaultModel(w, asDashboardHumanPeer(r))
}
