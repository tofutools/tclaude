package agentd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func writeSavedPromptError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrPromptNameTaken):
		writeError(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", "no such saved prompt")
	default:
		writeError(w, http.StatusInternalServerError, "io", err.Error())
	}
}

func savePrompt(w http.ResponseWriter, r *http.Request, previousName string) {
	if _, ok := requirePermission(w, r, PermPromptsManage); !ok {
		return
	}
	var p db.SavedPrompt
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128*1024)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	var err error
	if p, err = normalizeSavedPrompt(p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	if err := db.SaveSavedPrompt(previousName, p); err != nil {
		writeSavedPromptError(w, err)
		return
	}
	status := http.StatusOK
	if previousName == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, p)
}

func handleSavedPrompts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		prompts, err := db.ListSavedPrompts()
		if err != nil {
			writeSavedPromptError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, prompts)
	case http.MethodPost:
		savePrompt(w, r, "")
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

func handleSavedPromptByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch r.Method {
	case http.MethodGet:
		p, err := db.GetSavedPrompt(name)
		if err != nil {
			writeSavedPromptError(w, err)
			return
		}
		if p == nil {
			writeSavedPromptError(w, sql.ErrNoRows)
			return
		}
		writeJSON(w, http.StatusOK, p)
	case http.MethodPatch:
		savePrompt(w, r, name)
	case http.MethodDelete:
		if _, ok := requirePermission(w, r, PermPromptsManage); !ok {
			return
		}
		profiles, groups, err := db.SavedPromptReferences(name)
		if err != nil {
			writeSavedPromptError(w, err)
			return
		}
		if len(profiles) > 0 || len(groups) > 0 {
			var users []string
			if len(profiles) > 0 {
				users = append(users, "spawn profiles "+strings.Join(profiles, ", "))
			}
			if len(groups) > 0 {
				users = append(users, "groups "+strings.Join(groups, ", "))
			}
			writeError(w, http.StatusConflict, "prompt_in_use",
				fmt.Sprintf("saved prompt %q is still used by %s; remove it there first", name, strings.Join(users, " and ")))
			return
		}
		if err := db.DeleteSavedPrompt(name); err != nil {
			writeSavedPromptError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PATCH or DELETE")
	}
}

// normalizeSavedPrompt applies the library's name and body rules — the ones
// every write path, including bundle imports, holds a prompt to.
func normalizeSavedPrompt(p db.SavedPrompt) (db.SavedPrompt, error) {
	p.Name = strings.TrimSpace(p.Name)
	if err := validateGroupName(p.Name); err != nil {
		return p, fmt.Errorf("prompt name: %w", err)
	}
	p.Body = strings.ReplaceAll(p.Body, "\r\n", "\n")
	if strings.TrimSpace(p.Body) == "" || !isValidInitialMessage(p.Body) {
		return p, errors.New("prompt body must contain 1–16384 UTF-8 bytes, without control characters except tabs and newlines")
	}
	return p, nil
}

// collectReferencedSavedPrompts returns the library entries the given
// reference sets name, each once, in first-reference order. A dangling name is
// skipped: import degrades it anyway.
func collectReferencedSavedPrompts(refSets ...[]string) ([]db.SavedPrompt, error) {
	seen := map[string]bool{}
	out := []db.SavedPrompt{}
	for _, refs := range refSets {
		for _, ref := range cleanRoleRefs(refs) {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			p, err := db.GetSavedPrompt(ref)
			if err != nil {
				return nil, err
			}
			if p != nil {
				out = append(out, *p)
			}
		}
	}
	return out, nil
}

// recreateMissingSavedPrompts creates the embedded prompts a bundle carries
// that this library lacks. Like roles, an existing prompt is never
// overwritten: a differing local version is kept and reported. Prompts are
// shared task text with no authority of their own, so a bundle import does
// not additionally require prompts.manage.
func recreateMissingSavedPrompts(prompts []db.SavedPrompt) ([]string, *spawnFailure) {
	warnings := []string{}
	for _, raw := range prompts {
		p, err := normalizeSavedPrompt(raw)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("embedded saved prompt %q is invalid here (%s) — skipped", raw.Name, err))
			continue
		}
		existing, err := db.GetSavedPrompt(p.Name)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if existing != nil {
			if existing.Body != p.Body {
				warnings = append(warnings, fmt.Sprintf(
					"saved prompt %q already exists here with different text — kept the local version", p.Name))
			}
			continue
		}
		if err := db.SaveSavedPrompt("", p); errors.Is(err, db.ErrPromptNameTaken) {
			warnings = append(warnings, fmt.Sprintf("saved prompt %q already exists here — kept the local version", p.Name))
		} else if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		} else {
			warnings = append(warnings, fmt.Sprintf("imported saved prompt %q", p.Name))
		}
	}
	return warnings, nil
}

// resolveSavedPromptRefs validates an ordered saved-prompt reference set and
// returns it trimmed and de-duplicated. Every name must exist in the library.
func resolveSavedPromptRefs(refs []string) ([]string, *spawnFailure) {
	out := cleanRoleRefs(refs)
	for _, ref := range out {
		p, err := db.GetSavedPrompt(ref)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if p == nil {
			return nil, &spawnFailure{http.StatusBadRequest, "invalid_prompt",
				fmt.Sprintf("no saved prompt named %q", ref)}
		}
	}
	return out, nil
}

// composeSavedPrompts builds an initial message from saved prompts: each
// prompt's body in order, then the typed brief, separated by blank lines. A
// missing prompt or a composition over the initial-message cap is refused
// rather than silently trimmed.
func composeSavedPrompts(refs []string, brief string) (string, *spawnFailure) {
	parts := make([]string, 0, len(refs)+1)
	for _, ref := range refs {
		p, err := db.GetSavedPrompt(ref)
		if err != nil {
			return "", &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		if p == nil {
			return "", &spawnFailure{http.StatusBadRequest, "invalid_prompt",
				fmt.Sprintf("saved prompt %q does not name a prompt in the library", ref)}
		}
		if body := strings.TrimSpace(p.Body); body != "" {
			parts = append(parts, body)
		}
	}
	if brief = strings.TrimSpace(brief); brief != "" {
		parts = append(parts, brief)
	}
	msg := strings.Join(parts, "\n\n")
	if !isValidInitialMessage(msg) {
		return "", &spawnFailure{http.StatusBadRequest, "invalid_initial_message",
			fmt.Sprintf("the selected saved prompts and initial message together exceed %d bytes", agent.MaxInitialMessageBytes)}
	}
	return msg, nil
}
