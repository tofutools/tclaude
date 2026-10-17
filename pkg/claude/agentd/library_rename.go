package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The library is the dashboard's right panel: spawn profiles, group
// templates, roles, saved prompts and sandbox profiles. Each kind already
// renames through its full-replace update (references are stored by ID, and
// the db writers refresh the name snapshots). A rename here is that update
// with only the name changed, built server-side from the same projection the
// kind's GET returns, so a client never has to round-trip the whole item to
// change its name.

type libraryRenameReq struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	To   string `json:"to"`
}

type libraryRenameResp struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	From string `json:"from"`
}

func handleLibraryRename(w http.ResponseWriter, r *http.Request) {
	var req libraryRenameReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	req.Name, req.To = strings.TrimSpace(req.Name), strings.TrimSpace(req.To)
	if req.Name == "" || req.To == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "name and to are required")
		return
	}
	rename, perm := libraryRenamer(req.Kind)
	if rename == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "kind is one of profiles, templates, roles, saved_prompts, sandbox_profiles")
		return
	}
	if _, ok := requirePermission(w, r, perm); !ok {
		return
	}
	if req.Name == req.To {
		writeJSON(w, http.StatusOK, libraryRenameResp{Kind: req.Kind, Name: req.To, From: req.Name})
		return
	}
	if fail := rename(req.Name, req.To); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	writeJSON(w, http.StatusOK, libraryRenameResp{Kind: req.Kind, Name: req.To, From: req.Name})
}

func libraryRenamer(kind string) (func(from, to string) *spawnFailure, string) {
	switch kind {
	case "profiles":
		return renameSpawnProfile, PermProfilesManage
	case "templates":
		return renameTemplate, PermTemplatesManage
	case "roles":
		return renameRole, PermRolesManage
	case "saved_prompts":
		return renameSavedPrompt, PermPromptsManage
	case "sandbox_profiles":
		return renameSandboxProfile, PermSandboxProfilesManage
	}
	return nil, ""
}

func renameIO(err error) *spawnFailure {
	return &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
}

func renameMissing(what string) *spawnFailure {
	return &spawnFailure{http.StatusNotFound, "not_found", "no such " + what}
}

func renameTaken(err error) *spawnFailure {
	return &spawnFailure{http.StatusConflict, "exists", err.Error()}
}

func renameSpawnProfile(from, to string) *spawnFailure {
	existing, err := db.ResolveSpawnProfile(from)
	if err != nil {
		return renameIO(err)
	}
	// An alias resolves too, but renaming through one would rename a profile
	// the operator did not name.
	if existing == nil || existing.Name != from {
		return renameMissing("profile")
	}
	body := profileToJSON(existing)
	body.Name = to
	p, fail := buildProfileFromJSON(body)
	if fail != nil {
		return fail
	}
	p.ID = existing.ID
	if err := db.UpdateSpawnProfile(p); errors.Is(err, db.ErrSpawnProfileNameTaken) {
		return renameTaken(err)
	} else if err != nil {
		return renameIO(err)
	}
	return nil
}

func renameTemplate(from, to string) *spawnFailure {
	existing, err := db.GetGroupTemplate(from)
	if err != nil {
		return renameIO(err)
	}
	if existing == nil {
		return renameMissing("template")
	}
	body := templateToJSON(existing)
	body.Name = to
	t, fail := buildTemplateFromJSON(body)
	if fail != nil {
		return fail
	}
	t.ID = existing.ID
	if err := db.UpdateGroupTemplate(t); errors.Is(err, db.ErrGroupTemplateNameTaken) {
		return renameTaken(err)
	} else if err != nil {
		return renameIO(err)
	}
	return nil
}

func renameRole(from, to string) *spawnFailure {
	existing, err := db.GetRole(from)
	if err != nil {
		return renameIO(err)
	}
	if existing == nil {
		return renameMissing("role")
	}
	body := roleToJSON(existing)
	body.Name = to
	rl, fail := buildRoleFromJSON(body)
	if fail != nil {
		return fail
	}
	rl.ID = existing.ID
	if err := db.UpdateRole(rl); errors.Is(err, db.ErrRoleNameTaken) {
		return renameTaken(err)
	} else if err != nil {
		return renameIO(err)
	}
	return nil
}

func renameSavedPrompt(from, to string) *spawnFailure {
	existing, err := db.GetSavedPrompt(from)
	if err != nil {
		return renameIO(err)
	}
	if existing == nil {
		return renameMissing("saved prompt")
	}
	p := *existing
	p.Name = to
	if p, err = normalizeSavedPrompt(p); err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_arg", err.Error()}
	}
	if err := db.SaveSavedPrompt(from, p); errors.Is(err, db.ErrPromptNameTaken) {
		return renameTaken(err)
	} else if err != nil {
		return renameIO(err)
	}
	return nil
}

func renameSandboxProfile(from, to string) *spawnFailure {
	existing, err := db.GetSandboxProfile(from)
	if err != nil {
		return renameIO(err)
	}
	if existing == nil {
		return renameMissing("sandbox profile")
	}
	body := sandboxProfileToJSON(existing, true)
	body.Name = to
	p, _, err := buildSandboxProfile(body)
	if err != nil {
		return &spawnFailure{http.StatusBadRequest, "invalid_sandbox_profile", err.Error()}
	}
	p.ID = existing.ID
	switch err := db.UpdateSandboxProfile(p); {
	case errors.Is(err, db.ErrSandboxProfileNameTaken):
		return renameTaken(err)
	case err != nil:
		return renameIO(err)
	}
	return nil
}
