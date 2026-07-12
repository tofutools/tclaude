package agentd

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Sandbox-profile scribe drafts are deliberately ephemeral daemon state. They
// are a capability-reducing handoff from an agent chat to the human dashboard:
// submitting one neither writes the profile registry nor changes assignments.
// The dashboard loads the validated structure into its ordinary editor, where
// the human must explicitly press Save (and the normal CRUD validation runs
// again). A daemon restart merely discards an unsaved draft.
type sandboxProfileDraftJSON struct {
	Profile sandboxProfileJSON `json:"profile"`
}

type sandboxProfileDraftEntry struct {
	Draft     sandboxProfileDraftJSON
	CreatedAt time.Time
}

var (
	sandboxProfileDraftTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	sandboxProfileDraftMu      sync.Mutex
	sandboxProfileDrafts       = map[string]sandboxProfileDraftEntry{}
)

const sandboxProfileDraftLimit = 128

func sandboxProfileDraftToken(r *http.Request) (string, bool) {
	token := strings.TrimSpace(r.PathValue("token"))
	return token, sandboxProfileDraftTokenRE.MatchString(token)
}

// handleSandboxProfileDraftSubmit accepts the only operation available to a
// sandbox scribe. buildSandboxProfile applies the same canonicalization and
// protected-root/reserved-variable validation as an actual create/edit, but
// this handler intentionally has no DB mutation or assignment path.
func handleSandboxProfileDraftSubmit(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesDraft); !ok {
		return
	}
	token, ok := sandboxProfileDraftToken(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_token", "draft token must be 16-128 URL-safe characters")
		return
	}
	var body sandboxProfileDraftJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "json", err.Error())
		return
	}
	p, _, err := buildSandboxProfile(body.Profile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_sandbox_profile", err.Error())
		return
	}
	body.Profile = sandboxProfileToJSON(p, false)

	sandboxProfileDraftMu.Lock()
	// Keep the map bounded if a human abandons several chats.
	cutoff := time.Now().Add(-24 * time.Hour)
	for key, entry := range sandboxProfileDrafts {
		if entry.CreatedAt.Before(cutoff) {
			delete(sandboxProfileDrafts, key)
		}
	}
	if _, replacing := sandboxProfileDrafts[token]; !replacing && len(sandboxProfileDrafts) >= sandboxProfileDraftLimit {
		var oldestKey string
		var oldest time.Time
		for key, entry := range sandboxProfileDrafts {
			if oldestKey == "" || entry.CreatedAt.Before(oldest) {
				oldestKey, oldest = key, entry.CreatedAt
			}
		}
		delete(sandboxProfileDrafts, oldestKey)
	}
	sandboxProfileDrafts[token] = sandboxProfileDraftEntry{Draft: body, CreatedAt: time.Now()}
	sandboxProfileDraftMu.Unlock()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted": true,
		"message":  "draft validated and handed to the dashboard; it has not been saved",
	})
}

// handleDashboardSandboxProfileDraft is human-only by construction: dashboard
// cookie + Origin auth is the consent boundary. GET atomically consumes the
// handoff so a submit racing immediately after it cannot be deleted by a
// separate acknowledgement request. Consuming a draft does not mutate policy.
func handleDashboardSandboxProfileDraft(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	token, ok := sandboxProfileDraftToken(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_token", "draft token must be 16-128 URL-safe characters")
		return
	}
	sandboxProfileDraftMu.Lock()
	entry, found := sandboxProfileDrafts[token]
	if found && entry.CreatedAt.Before(time.Now().Add(-24*time.Hour)) {
		delete(sandboxProfileDrafts, token)
		found = false
	}
	if r.Method == http.MethodGet && found {
		delete(sandboxProfileDrafts, token)
	}
	sandboxProfileDraftMu.Unlock()

	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "sandbox profile draft is not ready")
		return
	}
	writeJSON(w, http.StatusOK, entry.Draft)
}
