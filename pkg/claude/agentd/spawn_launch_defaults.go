package agentd

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
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

// resolveLaunchDefaults walks the same tiers a real spawn walks — group default
// spawn profile, then global default spawn profile, then the harness default —
// for the fields the sandbox-implementation row cares about.
//
// requestedHarness is the caller's already-chosen harness, if it has one: the
// spawn dialog's harness select is an explicit choice that outranks every
// profile tier, and the implementation a profile carries is only valid relative
// to it. Blank means "resolve the harness too", which is what the sandbox
// profile editor's preview does.
//
// The two tiers below explicit are deliberately the ONLY ones here. The named
// spawn profile and an explicit launch choice sit above them in the full chain,
// but both are dialog state: when either sets a value the field is not blank,
// so this function is never the one answering.
func resolveLaunchDefaults(groupName, requestedHarness string) (launchDefaults, error) {
	var groupProfile *db.SpawnProfile
	if strings.TrimSpace(groupName) != "" {
		group, err := db.GetAgentGroupByName(strings.TrimSpace(groupName))
		if err != nil {
			return launchDefaults{}, err
		}
		groupProfile = groupDefaultProfile(group)
	}
	globalProfile := globalDefaultProfile()
	tiers := []launchProfileTier{
		{profile: groupProfile, source: profileSource(groupProfile, agent.ProvGroupProfileSource)},
		{profile: globalProfile, source: profileSource(globalProfile, agent.ProvGlobalProfileSource)},
	}

	harnessName := harness.DefaultName
	harnessSource := agent.ProvHarnessDefault
	if explicit := strings.TrimSpace(requestedHarness); explicit != "" {
		harnessName = harnessOrDefault(explicit)
		harnessSource = agent.ProvExplicit
	} else {
		for _, tier := range tiers {
			if tier.profile != nil {
				harnessName = harnessOrDefault(tier.profile.Harness)
				harnessSource = tier.source
				break
			}
		}
	}
	resolvedHarness, err := harness.Resolve(harnessName)
	if err != nil {
		return launchDefaults{}, err
	}

	requestedSandbox, sandboxSource, _, fail := resolveStringLaunchField(
		"sandbox", "", resolvedHarness.Name, tiers,
		func(profile *db.SpawnProfile) string { return profile.Sandbox },
		func(raw string) (string, error) { return harness.ValidateSandboxMode(resolvedHarness, raw) },
	)
	if fail != nil {
		return launchDefaults{}, fmt.Errorf("%s", fail.Msg)
	}
	requestedImplementation, implementationSource, _, fail := resolveStringLaunchField(
		sandboxImplementationField, "", resolvedHarness.Name, tiers,
		func(profile *db.SpawnProfile) string { return profile.SandboxImplementation },
		func(raw string) (string, error) {
			return validateSandboxImplementationForHarness(resolvedHarness, raw)
		},
	)
	if fail != nil {
		return launchDefaults{}, fmt.Errorf("%s", fail.Msg)
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(requestedImplementation)
	if err != nil {
		return launchDefaults{}, err
	}
	sandboxMode, err := harness.ResolveSandboxMode(resolvedHarness, requestedSandbox)
	if err != nil {
		return launchDefaults{}, err
	}
	return launchDefaults{
		harness:        resolvedHarness,
		sandbox:        sandboxMode,
		implementation: implementation,
		resolvedBy:     joinProvenanceSources(harnessSource, sandboxSource, implementationSource),
	}, nil
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

// handleSpawnLaunchDefaults answers GET /v1/spawn-launch-defaults?group=&harness=
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
	defaults, err := resolveLaunchDefaults(
		r.URL.Query().Get("group"), r.URL.Query().Get("harness"),
	)
	if err != nil {
		// A harness the caller invented is a bad request, not a daemon fault;
		// everything else here is storage or an inconsistent profile.
		if strings.TrimSpace(r.URL.Query().Get("harness")) != "" {
			if _, resolveErr := harness.Resolve(
				harnessOrDefault(strings.TrimSpace(r.URL.Query().Get("harness"))),
			); resolveErr != nil {
				writeError(w, http.StatusBadRequest, "invalid_arg", resolveErr.Error())
				return
			}
		}
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
