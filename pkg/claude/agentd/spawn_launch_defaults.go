package agentd

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// launchDefaults is the answer to "if a launch left these fields blank, what
// would the daemon fill in?" — the resolved defaults, as opposed to the
// mechanism that produces them.
//
// It exists because only the daemon can answer that. A browser knows the
// dialog's own fields, but the tiers beneath them (a group's default spawn
// profile, then the global one) are resolved server-side at spawn, and clearing
// the dialog's profile row does NOT remove them: the daemon still fills blank
// launch fields from those tiers. A dialog that guessed would eventually
// disagree with the launch it is describing, which is why the spawn dialog asks
// instead.
type launchDefaults struct {
	harness        *harness.Harness
	sandbox        string
	implementation sandboxpolicy.Implementation
	// resolvedBy names the tiers the values came from, in the daemon's own
	// provenance vocabulary (`group default profile "x"`, `harness default`, …),
	// deduplicated across the three fields.
	resolvedBy string
}

// resolveLaunchDefaults walks the same tiers a real spawn walks — the named
// spawn profile, then the group default spawn profile, then the global default
// spawn profile, then the harness default — for the fields the
// sandbox-implementation row cares about.
//
// profileHandle is the dialog's selected spawn profile, and it belongs here even
// though picking a profile pre-fills the dialog: the operator can set the
// implementation select back to blank (or a harness switch can clear it) while
// the profile stays selected. The spawn request still carries `profile`, and
// handleGroupSpawn ranks that named profile ABOVE the group and global tiers, so
// omitting it here would name an implementation the launch does not use.
//
// requestedHarness is the caller's already-chosen harness, if it has one: the
// spawn dialog's harness select is an explicit choice that outranks every
// profile tier, and the implementation a profile carries is only valid relative
// to it. Blank means "resolve the harness too", which is what the sandbox
// profile editor's preview does — it has neither a named profile nor a harness
// choice, and passes "" for both.
//
// A caller fault (an unknown harness, a profile whose stored value is invalid
// for its own harness) comes back as *spawnFailure with the status that belongs
// to it; err is reserved for storage.
func resolveLaunchDefaults(
	groupName, profileHandle, requestedHarness string,
) (launchDefaults, *spawnFailure, error) {
	var group *db.AgentGroup
	if strings.TrimSpace(groupName) != "" {
		var err error
		group, err = db.GetAgentGroupByName(strings.TrimSpace(groupName))
		if err != nil {
			return launchDefaults{}, nil, err
		}
	}
	capture := captureFreshLaunchConfiguration(group, profileHandle,
		freshLaunchConfigurationRequest{Harness: requestedHarness})
	if capture.Issue != nil {
		if capture.Issue.Kind == freshLaunchProfileMissing {
			return launchDefaults{}, &spawnFailure{
				http.StatusBadRequest, "not_found", capture.Issue.Message}, nil
		}
		return launchDefaults{}, nil, fmt.Errorf("%s: %w", capture.Issue.Message, capture.Issue.Err)
	}
	resolved, refusal := resolveFreshLaunchConfiguration(capture.Input)
	if refusal != nil {
		return launchDefaults{}, freshLaunchRefusalSpawnFailure(refusal), nil
	}
	return launchDefaults{
		harness:        resolved.Harness,
		sandbox:        resolved.HarnessBuiltinMode.Effective,
		implementation: sandboxpolicy.Implementation(resolved.SandboxImplementation.Effective),
		resolvedBy: joinProvenanceSources(
			resolved.HarnessSelection.Source,
			resolved.HarnessBuiltinMode.Source,
			resolved.SandboxImplementation.Source,
		),
	}, nil, nil
}

func freshLaunchRefusalSpawnFailure(refusal *freshLaunchConfigurationRefusal) *spawnFailure {
	if refusal == nil {
		return nil
	}
	status := http.StatusBadRequest
	if refusal.Kind == freshLaunchInvalidImplementation {
		status = sandboxImplementationValidationStatus(refusal.Err)
	}
	return &spawnFailure{status, string(refusal.Kind), refusal.Error()}
}

// joinProvenanceSources renders the distinct tiers a set of fields resolved
// from, preserving order and dropping the empty sources of fields nothing set.
func joinProvenanceSources(sources ...string) string {
	distinct := make([]string, 0, len(sources))
	for _, source := range sources {
		if source == "" {
			continue
		}
		if !slices.Contains(distinct, source) {
			distinct = append(distinct, source)
		}
	}
	return strings.Join(distinct, "; ")
}

type spawnLaunchDefaultsJSON struct {
	Harness        string `json:"harness"`
	Sandbox        string `json:"sandbox"`
	Implementation string `json:"implementation"`
	ResolvedBy     string `json:"resolved_by,omitempty"`
}

// handleSpawnLaunchDefaults answers
// GET /v1/spawn-launch-defaults?group=&profile=&harness=
// with the values a spawn into that group would resolve for the launch fields
// left blank.
//
// Read-only and ungated, matching GET /v1/spawn-profiles: every input it reads
// (the profile library, a group's default profile, the global default) is
// already readable through those endpoints, and this only reports which of them
// a launch would pick. It grants nothing and changes nothing.
func handleSpawnLaunchDefaults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	query := r.URL.Query()
	defaults, fail, err := resolveLaunchDefaults(
		query.Get("group"), query.Get("profile"), query.Get("harness"),
	)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, spawnLaunchDefaultsJSON{
		Harness:        defaults.harness.Name,
		Sandbox:        defaults.sandbox,
		Implementation: string(defaults.implementation),
		ResolvedBy:     defaults.resolvedBy,
	})
}
