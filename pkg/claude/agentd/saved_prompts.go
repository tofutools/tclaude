package agentd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

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
		if err := db.DeleteSavedPrompt(name); err != nil {
			writeSavedPromptError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, PATCH or DELETE")
	}
}
