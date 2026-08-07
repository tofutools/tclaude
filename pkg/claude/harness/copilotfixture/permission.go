package copilotfixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the measurement apparatus for TCL-973's Phase 0: what Copilot
// 1.0.77 actually does to an UNATTENDED launch.
//
// The distinction that organizes everything here is between a launch that
// finishes and a launch that sits forever waiting for a human. tclaude spawns
// Copilot into a tmux pane with nobody watching it, so a prompt is not a
// nuisance — it is a permanent deadlock, and an agent that deadlocks looks
// exactly like an agent that is thinking.
//
// Two rules the scenarios in this package follow, both learned by getting them
// wrong first:
//
//  1. Permission behavior is only measurable on a PTY. See pty.go — without a
//     terminal the CLI cannot draw a prompt, so it does not, and a headless run
//     reports "no prompt" for a launch that would deadlock a real pane.
//  2. A bypass must not be a promotion. Several ways of getting past Copilot's
//     first gate also change the permission posture, so using one to reach a
//     later measurement silently confounds it. TrustFolder exists to clear the
//     first gate WITHOUT touching tool, path or URL permissions.

// TrustPromptMarker is the folder-trust dialog's title. It is the single most
// important string in this package: a fresh COPILOT_HOME makes it the FIRST
// thing an unattended pane hits, before the provider is contacted at all.
const TrustPromptMarker = "Confirm folder trust"

// TrustFolder pre-grants folder trust for dir by writing Copilot's config
// before launch, and is the only trust bypass the permission scenarios use.
//
// It is deliberately not a flag, because no flag does this. Measured against
// 1.0.77, every one of `--allow-all-tools`, `--allow-all`, `--allow-all-paths`
// and `--add-dir <dir>` still leaves the trust dialog blocking the launch with
// zero provider requests. Only a pre-seeded `trustedFolders` entry clears it.
//
// That is the whole reason this helper exists rather than a flag constant, and
// it is a finding rather than an implementation detail: any detached Copilot
// agent tclaude spawns needs a CONFIG-FILE write before launch, which no part
// of the argv-rendering design in TCL-973's plan can express.
//
// The other reason it is a config write is the confounding rule above. The one
// environment variable that also clears the dialog, COPILOT_ALLOW_ALL, clears
// it by promoting the whole session to allow-all — so a scenario that used it
// to reach the tool-approval question would be measuring a launch that had
// already granted every tool. TrustFolder grants trust and nothing else.
func TrustFolder(t *testing.T, home, dir string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("copilotfixture: mkdir %s: %v", home, err)
	}
	// config.json rather than settings.json: 1.0.77 reads the legacy file and
	// migrates it, which TestCopilotLegacyConfigMigratesIntoSettings already
	// pins. Writing the file the CLI demonstrably honours keeps this helper
	// tied to a measured behavior instead of a guessed one.
	path := filepath.Join(home, "config.json")
	enc, err := json.Marshal(map[string]any{"trustedFolders": []string{dir}})
	if err != nil {
		t.Fatalf("copilotfixture: marshal trustedFolders: %v", err)
	}
	if err := os.WriteFile(path, enc, 0o600); err != nil {
		t.Fatalf("copilotfixture: write %s: %v", path, err)
	}
}

// PathPromptMarker titles the directory-access dialog, which is its own
// permission surface: neither folder trust (which gates the whole launch) nor
// tool approval (which gates the command).
const PathPromptMarker = "Allow directory access"

// PermissionOutcome is what a scenario could establish about a launch FROM THE
// RUN IT JUST PERFORMED.
type PermissionOutcome string

const (
	// PermissionAllowed: the tool ran and its result was posted back to the
	// provider. A second provider request is the proof — it cannot happen
	// unless the CLI executed the tool and had a result to send.
	PermissionAllowed PermissionOutcome = "allowed"

	// PermissionBlocked: no follow-up request, and the process is still alive
	// with its output settled. This is the deadlock shape a detached agent
	// would exhibit forever.
	PermissionBlocked PermissionOutcome = "blocked"

	// PermissionDenied: the tool was refused WITHOUT asking, and the refusal
	// itself was posted back to the provider as the tool's result.
	//
	// This is the outcome that makes the follow-up request an ambiguous signal
	// on its own, and getting it wrong would have been the most damaging
	// mistake in the suite. A denial produces exactly the same coarse
	// observable as a success — a second provider request — because the CLI
	// answers the model with "you may not do that" rather than with output. A
	// classifier that stopped at "a follow-up arrived" would therefore report
	// every silent denial as EXECUTION, which is the single worst direction to
	// be wrong in for a ticket about what a detached agent is permitted to do.
	//
	// It is a distinct outcome rather than a flavour of blocked because the two
	// have opposite consequences for a detached agent: a denial lets the turn
	// continue (the model gets an answer and can adapt), while a block parks
	// the pane forever.
	PermissionDenied PermissionOutcome = "denied"

	// PermissionRefused: no follow-up request, and the CLI exited on its own.
	// The launch was rejected rather than parked — a bad flag, a refused
	// startup, a denial that terminates.
	PermissionRefused PermissionOutcome = "refused"
)

// permissionDenialMarkers are phrases Copilot posts back as a TOOL RESULT when
// it refused the call outright instead of prompting.
//
// Matched on the tool result rather than on the terminal, deliberately: the
// result is what the CLI told the model, which is the fact that decides whether
// the tool ran. Screen text would be a rendering of it at best, and this suite
// treats TUI chrome as corroboration rather than contract everywhere else.
var permissionDenialMarkers = []string{
	// A path outside every granted root, with no terminal to ask through. The
	// wording is the CLI describing its own no-TTY fallback, and it is why this
	// marker is here rather than in a scenario: on a PTY the same launch draws
	// PathPromptMarker and waits instead, so this string is what a headless run
	// sees and a real pane never does.
	"Permission denied and could not request permission from user",
	// An explicit --deny-tool rule matched. Denial rules never prompt.
	"Permission to run this tool was denied",
	// The URL permission layer refused before any network access.
	"Permission to access this URL was denied",
	// Generic form, kept last so a more specific marker wins the explanation.
	"Permission denied",
}

// PermissionDenialMarkers returns the denial phrases, so a test can assert a
// property of the WHOLE set rather than of a hand-copied sample of it.
//
// The accessor exists because the alternative silently rots. A test that lists
// the markers itself keeps passing when a marker is added to the real set, and
// its stated guarantee -- that denial beats execution for every marker -- then
// covers only the spellings someone happened to copy. Returning a copy keeps
// the set itself immutable from outside.
func PermissionDenialMarkers() []string {
	return append([]string(nil), permissionDenialMarkers...)
}

// ToolResults extracts the tool-role message contents from one recorded
// provider request on the COMPLETIONS wire, which is how a scenario reads what
// the CLI told the model a tool did.
//
// Only the tool role is returned. Assistant and user content is model or
// fixture text and says nothing about permissions, and this function's output
// feeds a classifier rather than a golden, so it is never committed.
//
// Completions only, deliberately. An earlier version also read the Responses
// wire's input[] looking for role=="tool", which would have made this function
// APPEAR wire-agnostic on the strength of a guess — nothing in this suite has
// ever observed how that wire spells a tool result, and every permission
// scenario runs on completions. A fallback nobody has measured is worse than
// no fallback: it returns empty for an unrecognized shape, which now reads as
// "no tool result" and lands in ClassifyPermission's undecidable arm, exactly
// where an unmeasured case belongs. Add the Responses arm when a fixture
// establishes its shape, not before.
func ToolResults(r RecordedRequest) []string {
	var out []string
	entries, ok := r.Body["messages"].([]any)
	if !ok {
		return nil
	}
	for _, raw := range entries {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "tool" {
			continue
		}
		if content, ok := m["content"].(string); ok {
			out = append(out, content)
		}
	}
	return out
}

// DenialMarker returns the denial phrase found in a tool result, or "" when the
// result records an ordinary execution.
func DenialMarker(toolResults []string) string {
	for _, result := range toolResults {
		for _, marker := range permissionDenialMarkers {
			if strings.Contains(result, marker) {
				return marker
			}
		}
	}
	return ""
}

// dialogMarkersPresent names the permission dialogs visible in a transcript,
// or "none" when it shows no dialog this package knows how to name.
//
// It exists for the INCONCLUSIVE arm. A run that cannot be classified is the
// one case where the screen is the only remaining evidence, and it was also
// the one case that discarded it: the error named the request counts and the
// settle state and said nothing about what was actually drawn. So a failure
// that could have been resolved by looking — is the CLI parked on a dialog, or
// still working toward one? — instead required guessing, and a whole class of
// macOS failures (TCL-1029) stayed undiagnosable across several sightings for
// exactly that reason.
//
// This lists EVERY dialog drawn during the run, including ones ClassifyPermission
// draws a verdict from. The two are not in tension, but the difference is worth
// stating: a marker means the dialog was drawn at some point, not that the CLI
// is parked on it now, so a verdict may only rest on one where nothing stronger
// contradicts it. That is why the classifier checks tool results FIRST — a run
// that answered its prompt and went on to execute still carries the marker for
// the rest of the transcript, and reading it as blocked would manufacture a
// finding out of stale screen text (pinned by TestClassifyPermissionArms'
// path-dialog-does-not-beat-a-tool-result row). Reaching this diagnostic means
// no such evidence exists, so here the marker is simply reported.
func dialogMarkersPresent(transcript string) string {
	var found []string
	for _, marker := range []string{TrustPromptMarker, PathPromptMarker} {
		if containsMarker(transcript, marker) {
			found = append(found, strconv.Quote(marker))
		}
	}
	if len(found) == 0 {
		return "none"
	}
	return strings.Join(found, ", ")
}

// containsMarker matches a dialog title the way PTYResult.Contains does, case
// insensitively, so the classifier and the scenarios that corroborate it cannot
// disagree about whether a dialog was on screen. They used different
// comparisons until the verdict started depending on one of them: a re-cased
// TUI title would then have left the scenario assertion passing while the
// verdict silently fell back to the sampled-quiescence arm.
func containsMarker(transcript, marker string) bool {
	return strings.Contains(
		strings.ToLower(transcript), strings.ToLower(marker))
}

// PermissionVerdict pairs an outcome with the observation behind it.
type PermissionVerdict struct {
	Outcome PermissionOutcome
	// Evidence is what a human reads on failure and what CI greps for. CI
	// asserting the VALUE, not merely that the test passed, is what stops a
	// scenario from quietly starting to measure a different arm.
	Evidence string
}

// ClassifyPermission decides what happened to a launch that was expected to
// reach a tool call.
//
// followUpRequests is the number of provider requests BEYOND the first. The
// first request only proves the CLI got past its startup gates; the second is
// the one that proves a tool actually executed, because it carries the tool
// result back.
//
// The error arm is the point of the function. "No follow-up request" has two
// completely different causes — parked on a prompt, or dead — and they are the
// difference between "tclaude must render a nonblocking posture" and "tclaude
// passed an argument Copilot rejects". A classifier that collapsed them would
// let a typo'd flag be recorded as proof of a permission gate. So the
// undecidable case, where nothing at all was observed, is an error a scenario
// must fail on rather than an arm it may pick.
// followUpToolResults are the tool-role message contents carried by the
// follow-up request. They are what separates a tool that RAN from one that was
// refused without asking, since both produce a follow-up.
func ClassifyPermission(
	totalRequests, followUpRequests int, stillAlive, quiesced bool,
	transcript string, followUpToolResults []string,
) (PermissionVerdict, error) {
	switch {
	case followUpRequests > 0 && DenialMarker(followUpToolResults) != "":
		// Checked BEFORE the allowed arm, because a denial is a follow-up too.
		return PermissionVerdict{
			Outcome: PermissionDenied,
			Evidence: fmt.Sprintf(
				"the tool was refused without prompting and the refusal was posted back "+
					"as its result (%q)", DenialMarker(followUpToolResults)),
		}, nil

	case followUpRequests > 0 && len(followUpToolResults) > 0:
		return PermissionVerdict{
			Outcome:  PermissionAllowed,
			Evidence: "the tool executed and its result was posted back to the provider",
		}, nil

	case followUpRequests > 0:
		// A follow-up request with NO tool result in it. The count alone was
		// once treated as proof of execution, and it is not: the CLI can issue
		// a further request for reasons that have nothing to do with a tool
		// having run — an internal retry, an auxiliary call, a turn that
		// continued on text. Calling that "allowed" would manufacture evidence
		// of execution out of a request that carries none, in a ticket whose
		// entire subject is what a detached agent is permitted to do.
		return PermissionVerdict{}, fmt.Errorf(
			"cannot classify this launch: %d follow-up provider request(s) arrived but "+
				"none carried a tool result, so nothing establishes that the tool ran. "+
				"This is an auxiliary or retry request, a wire whose tool-result shape "+
				"ToolResults does not read, or a genuinely new behavior — all of which "+
				"need looking at rather than being recorded as execution",
			followUpRequests)

	case totalRequests == 0 && stillAlive && containsMarker(transcript, TrustPromptMarker):
		// Named separately from the generic blocked arm because it is a
		// different gate at a different time: this one blocks BEFORE the model
		// is ever contacted, so no permission flag can be observed past it.
		return PermissionVerdict{
			Outcome: PermissionBlocked,
			Evidence: "blocked at the folder-trust dialog before contacting the provider " +
				"(0 provider requests)",
		}, nil

	case stillAlive && containsMarker(transcript, PathPromptMarker):
		// The directory-access dialog, named for the same reason the trust
		// dialog above is: it identifies WHICH gate stopped the launch.
		//
		// It is also checked BEFORE the quiescence arm, and that ordering is
		// the fix for TCL-1029's B. Quiescence is sampled once, on the tick the
		// run ends on, so an arm cut by the deadline is called blocked or
		// undecidable according to whether that instant fell more or less than
		// PTYQuiescence after the last write — a phase check against the CLI's
		// render cadence, not an observation about the launch. On Linux the
		// silence window catches these arms long before the deadline and the
		// sample is never consulted; on macOS the render cadence keeps output
		// gaps in a band that rarely reaches it (measured: allowed arms go
		// quiet up to 5.8s, blocked arms as little as 4.1s, against a 10s
		// window), so ~4 in 5 blocked arms reached the deadline and had their
		// verdict decided by that coin flip.
		//
		// The dialog is the durable fact the sample was standing in for. The
		// transcript accumulates, so once the CLI has drawn this prompt the
		// marker is there whatever the output is doing when the clock runs out
		// — which is exactly why the coin flip was never necessary. Verified
		// by construction rather than observed: forcing a Linux blocked arm
		// down the deadline path (permissionDeadline shortened below the
		// silence window) reproduces the macOS failure, and the marker is
		// present in every reproduction.
		return PermissionVerdict{
			Outcome: PermissionBlocked,
			Evidence: fmt.Sprintf(
				"no tool-result follow-up after %d provider request(s); the process is alive "+
					"showing the directory-access dialog, i.e. parked on a prompt",
				totalRequests),
		}, nil

	case stillAlive && quiesced:
		return PermissionVerdict{
			Outcome: PermissionBlocked,
			Evidence: fmt.Sprintf(
				"no tool-result follow-up after %d provider request(s); the process is alive "+
					"with settled output, i.e. parked on a prompt", totalRequests),
		}, nil

	case !stillAlive:
		return PermissionVerdict{
			Outcome: PermissionRefused,
			Evidence: fmt.Sprintf(
				"no tool-result follow-up after %d provider request(s), and the CLI exited "+
					"on its own", totalRequests),
		}, nil

	default:
		return PermissionVerdict{}, fmt.Errorf(
			"cannot classify this launch: no tool-result follow-up after %d provider "+
				"request(s), the process is still alive, but its output never settled. "+
				"That is neither a prompt (which stops producing output) nor an exit, so it "+
				"is most likely still working and the scenario's deadline was too short. "+
				"Recording it as 'blocked' would be a guess, and this measurement exists "+
				"precisely because guesses about Copilot's blocking behavior are what "+
				"TCL-973 cannot afford. Permission dialogs drawn during the run: %s "+
				"(a dialog here means the CLI reached a prompt and the run was cut "+
				"before that could be established; none means it never got that far, "+
				"which is a bound or a hang rather than a permission gate)",
			totalRequests, dialogMarkersPresent(transcript))
	}
}

// ContractStatus is how much a measurement established.
type ContractStatus string

const (
	// StatusProven: measured against the real pinned binary, claim holds.
	StatusProven ContractStatus = "proven"
	// StatusDisproven: measured against the real pinned binary, claim fails.
	StatusDisproven ContractStatus = "disproven"
	// StatusUnverified: NOT established. The brief is explicit that a
	// documented inability to measure something credential-free beats a guess,
	// so this is a first-class outcome and not a to-do marker.
	StatusUnverified ContractStatus = "unverified"
)

// ContractEntry is one measurement from the Phase 0 brief.
type ContractEntry struct {
	// ID is the stable identifier used by the contract file, the scenario
	// registration and the CI gate alike.
	ID string `json:"id"`
	// Claim is the question the brief asked, in one sentence.
	Claim string `json:"claim"`
	// Status is what this suite established.
	Status ContractStatus `json:"status"`
	// Finding states the measured answer, including where it contradicts the
	// documentation or the plan that preceded it.
	Finding string `json:"finding"`
	// Scenarios are the test names that back a proven/disproven status, as Go
	// prints them (`TestX/subtest`). An unverified entry MUST name none — that
	// is what makes "unverified" an assertion rather than a comment.
	Scenarios []string `json:"scenarios,omitempty"`
	// Blocker, on an unverified entry, states why it could not be measured.
	Blocker string `json:"blocker,omitempty"`

	// Corroborating holds claims about this measurement that came from
	// somewhere OTHER than the scenarios above — typically an independent rig
	// that is not committed here.
	//
	// It exists because of a real mistake caught in review: an entry whose
	// status was correctly established by its scenarios had a Finding that also
	// asserted several neighbouring behaviors NO committed scenario measured.
	// The contract guard blessed the entry, and the entry blessed the extra
	// claims by association. Splitting them out is what keeps a proven status
	// from laundering unfixtured detail — a reader can see exactly where the
	// evidence stops, and the guard can require that separation.
	//
	// A claim listed here is NOT proven by this suite. It is a lead worth
	// fixturing, recorded so it is not lost and not mistaken for measurement.
	Corroborating []string `json:"corroborating,omitempty"`
}

// PermissionContract is the committed contract table.
type PermissionContract struct {
	CLIVersion string          `json:"cliVersion"`
	Entries    []ContractEntry `json:"entries"`
}

// registeredScenarios are the scenario names the test tables DECLARE, filled
// at table-construction time rather than when a scenario runs.
//
// Declaration rather than execution, deliberately: `go test -run` is how
// anyone iterates on one row, and a registry keyed on execution would make the
// contract check fail for every partial run — which trains people to ignore
// it. Keyed on declaration it answers the question that actually matters, "is
// this contract entry backed by a scenario that still exists".
//
// The other question, "did that scenario really run", is answered elsewhere
// and answered more weakly than it used to be. CI enumerated every scenario by
// name until TCL-1046; these are lab scenarios now, and the lab gates on pass
// COUNTS — one for scenarios and one for table rows, since these entries cite
// subtests and a parent whose rows all skipped still prints PASS. A floor
// catches the whole matrix going quiet, not a single row. Anything finer would
// have to come back here, as an execution-keyed registry, and pay the
// partial-run cost described above.
var registeredScenarios = map[string]bool{}

// RegisterScenario records that a scenario with this name exists. Call it from
// the test table's construction.
func RegisterScenario(name string) string {
	registeredScenarios[name] = true
	return name
}

// RegisteredScenarios returns the declared scenario names.
func RegisteredScenarios() []string {
	out := make([]string, 0, len(registeredScenarios))
	for name := range registeredScenarios {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// LoadPermissionContract reads the committed contract table.
func LoadPermissionContract(t *testing.T, path string) PermissionContract {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("copilotfixture: reading the permission contract: %v", err)
	}
	var contract PermissionContract
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("copilotfixture: parsing the permission contract: %v", err)
	}
	return contract
}
