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
	p.Name = strings.TrimSpace(p.Name)
	if err := validateGroupName(p.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "prompt name: "+err.Error())
		return
	}
	p.Body = strings.ReplaceAll(p.Body, "\r\n", "\n")
	if strings.TrimSpace(p.Body) == "" || !isValidInitialMessage(p.Body) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "prompt body must contain 1–16384 UTF-8 bytes, without control characters except tabs and newlines")
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
