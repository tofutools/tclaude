package harness

import (
	"strings"
	"testing"
)

// The catalog's whole justification is that every flag it emits was measured
// against the pinned binary, so these tests are written against the FINDINGS in
// copilotfixture/testdata/1.0.77/permission_contract.json rather than against
// the implementation. Each one names the contract entry it stands on.

func TestCopilotApprovalCatalog(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	if !h.SupportsApproval() {
		t.Fatal("copilot must advertise a launch-time approval catalog")
	}
	if got := h.Approval.DefaultPolicy(); got != CopilotApprovalAllowTools {
		t.Fatalf("DefaultPolicy() = %q, want %q — an unattended pane must not default to a prompting posture", got, CopilotApprovalAllowTools)
	}
	// The default must survive its own validator: agentd.approvalForHarness
	// resolves a launch by validating DefaultPolicy(), and a default that fails
	// validation silently degrades to "no policy at all".
	if got, err := h.Approval.ValidatePolicy(h.Approval.DefaultPolicy()); err != nil || got != CopilotApprovalAllowTools {
		t.Fatalf("ValidatePolicy(DefaultPolicy()) = %q, %v; want the default to validate to itself", got, err)
	}
	modes := h.Approval.Modes()
	if len(modes) != 2 || modes[0] != CopilotApprovalAllowTools || modes[1] != CopilotApprovalInherit {
		t.Fatalf("Modes() = %v, want the default first, then inherit", modes)
	}
	// Mutating the returned slice must not reach the validation source.
	modes[0] = "tampered"
	if again := h.Approval.Modes(); again[0] != CopilotApprovalAllowTools {
		t.Fatal("Modes() handed out the shared backing array")
	}
	for _, mode := range h.Approval.Modes() {
		if strings.TrimSpace(h.Approval.ModeHelp(mode)) == "" {
			t.Errorf("ModeHelp(%q) is empty; the spawn dialog renders it", mode)
		}
	}
	if h.Approval.ModeHelp("no-such-mode") != "" {
		t.Fatal("ModeHelp must return \"\" for an unrecognized policy")
	}
}

// Both mode-help strings must warn, because NEITHER token makes a Copilot pane
// unconditionally nonblocking: `inherit` is Copilot's prompting posture, and
// even `allow-tools` leaves the out-of-grant path dialog and the folder-trust
// gate standing (contract: out-of-cwd-paths, folder-trust). A reassuring
// default would be the exact overstatement this ticket is about.
func TestCopilotApprovalModeHelpWarnsAboutEveryRemainingPromptSource(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	for _, mode := range h.Approval.Modes() {
		help := h.Approval.ModeHelp(mode)
		if !strings.Contains(help, "⚠") {
			t.Errorf("ModeHelp(%q) carries no caveat: %q", mode, help)
		}
	}
	allowTools := h.Approval.ModeHelp(CopilotApprovalAllowTools)
	for _, want := range []string{"granted directory", "trust"} {
		if !strings.Contains(allowTools, want) {
			t.Errorf("ModeHelp(%q) must name the prompt sources it does NOT close (missing %q): %q",
				CopilotApprovalAllowTools, want, allowTools)
		}
	}
}

func TestCopilotApprovalValidatePolicy(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ""},
		{in: CopilotApprovalAllowTools, want: CopilotApprovalAllowTools},
		{in: "  " + CopilotApprovalInherit + "  ", want: CopilotApprovalInherit},
		// Other harnesses' tokens are not Copilot's, and a validator that let
		// them through would render nothing while recording an authority the
		// launch never had.
		{in: "never", wantErr: true},
		{in: "auto", wantErr: true},
		{in: "allow-all", wantErr: true},
		{in: "yolo", wantErr: true},
		{in: "plan", wantErr: true},
		{in: "ALLOW-TOOLS", wantErr: true},
	} {
		got, err := h.Approval.ValidatePolicy(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ValidatePolicy(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ValidatePolicy(%q) = %q, %v; want %q, nil", tc.in, got, err, tc.want)
		}
	}
}

// ResolveApprovalPolicy is the daemon spawn boundary: an unchosen Copilot spawn
// must land on the nonblocking default, while `tclaude session new` (the human
// trust root, who can attach and answer a prompt) must be left alone.
func TestCopilotApprovalResolution(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	got, err := ResolveApprovalPolicy(h, "")
	if err != nil || got != CopilotApprovalAllowTools {
		t.Fatalf("ResolveApprovalPolicy(copilot, \"\") = %q, %v; want %q", got, err, CopilotApprovalAllowTools)
	}
	got, err = ValidateApprovalPolicy(h, "")
	if err != nil || got != "" {
		t.Fatalf("ValidateApprovalPolicy(copilot, \"\") = %q, %v; want \"\" (no forced posture for a human launch)", got, err)
	}
	if _, err := ResolveApprovalPolicy(h, "never"); err == nil {
		t.Fatal("a Codex policy must be refused for copilot rather than silently dropped")
	}
	// Auto-review is a Codex guardian axis; Copilot has no reviewer, and
	// silently dropping the flag would hide the mistake.
	if _, err := ResolveAutoReview(h, true); err == nil {
		t.Fatal("ResolveAutoReview(copilot, true) must be refused")
	}
}

func TestCopilotPermissionArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  string
		addDirs []string
		want    []string
	}{
		{
			// contract: default-interactive-blocking (--allow-all-tools closes
			// the tool gate) + no-ask-user (--no-ask-user removes ask_user).
			name:   "the default renders exactly the two measured flags",
			policy: CopilotApprovalAllowTools,
			want:   []string{"--allow-all-tools", "--no-ask-user"},
		},
		{
			name:   "inherit renders nothing at all",
			policy: CopilotApprovalInherit,
		},
		{
			// contract: out-of-cwd-paths — --add-dir <dir> clears the directory
			// dialog for that directory, and nothing wider is emitted.
			name:    "granted directories become one --add-dir each, sorted",
			policy:  CopilotApprovalAllowTools,
			addDirs: []string{"/srv/b", "/srv/a"},
			want: []string{"--allow-all-tools", "--no-ask-user",
				"--add-dir", "/srv/a", "--add-dir", "/srv/b"},
		},
		{
			// The grants come from the sandbox profile, not from the approval
			// token: dropping them under `inherit` would leave Copilot's own
			// path check prompting for a directory tclaude's outer sandbox
			// already opened, and SandboxReadDirs forbids silently dropping
			// them.
			name:    "inherit still renders the profile's directory grants",
			policy:  CopilotApprovalInherit,
			addDirs: []string{"/srv/a"},
			want:    []string{"--add-dir", "/srv/a"},
		},
		{
			name:    "duplicate and relative entries are dropped",
			policy:  CopilotApprovalAllowTools,
			addDirs: []string{"/srv/a", "/srv/a/", "relative/path", "", "  ", "."},
			want:    []string{"--allow-all-tools", "--no-ask-user", "--add-dir", "/srv/a"},
		},
		{
			name:    "an unrecognized policy renders nothing, not the default",
			policy:  "allow-all",
			addDirs: []string{"/srv/a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := copilotPermissionArgs(tc.policy, tc.addDirs)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("copilotPermissionArgs(%q, %v)\n got: %v\nwant: %v", tc.policy, tc.addDirs, got, tc.want)
			}
		})
	}
}

// TestCopilotPermissionArgsNeverEmitDisprovenOrEscalatingFlags is the guard the
// disproven half of the contract earned.
//
// `--deny-tool 'url()'` was the TCL-973 plan's proposed default and is REJECTED
// at argument parse with exit 1 and no provider contact (contract: url-access) —
// it would have killed every Copilot pane at launch. `--allow-all-paths` and
// `--allow-all`/`--yolo` are real and measured, but Copilot's built-in edits are
// not OS-confined, so outside a tclaude-layer sandbox the path check is the only
// boundary on what the agent writes and no token here removes it.
//
// Written as a sweep over every mode plus a few non-modes, rather than as
// assertions on the two tokens that exist today, so a third token cannot be
// added without this guard seeing it.
func TestCopilotPermissionArgsNeverEmitDisprovenOrEscalatingFlags(t *testing.T) {
	h, err := Resolve(CopilotName)
	if err != nil {
		t.Fatalf("Resolve(copilot): %v", err)
	}
	forbidden := []string{
		"--deny-tool", "--allow-tool", // tool governance is a separate contract, still nil
		"--allow-all-paths", "--allow-all", "--yolo",
		"--allow-all-urls", "--allow-url", "--deny-url",
		"--disallow-temp-dir", "--mode", "--plan", "--autopilot",
	}
	policies := append(h.Approval.Modes(), "", "bogus")
	for _, policy := range policies {
		args := copilotPermissionArgs(policy, []string{"/srv/a", "/srv/b"})
		joined := strings.Join(args, " ")
		for _, flag := range forbidden {
			for _, arg := range args {
				if arg == flag {
					t.Errorf("policy %q renders %s, which no measurement in the permission contract supports as a default: %s",
						policy, flag, joined)
				}
			}
		}
		// The empty-paren pattern is the exact spelling the binary rejects, in
		// any kind. Matched as a substring so it is caught however it is built.
		for _, spelling := range []string{"url()", "shell()", "write()"} {
			if strings.Contains(joined, spelling) {
				t.Errorf("policy %q renders the disproven %s pattern, which exits 1 at argument parse: %s", policy, spelling, joined)
			}
		}
	}
}

// TestCopilotBuildCommandScrubsAmbientAllowAll pins the treatment of the one
// environment variable measured to promote a launch on its own.
//
// COPILOT_ALLOW_ALL is strictly stronger than the --allow-all-tools flag it
// documents: exported alone it also cleared the folder-trust gate that no flag
// clears (contract: ambient-allow-all-env). tclaude forwards the operator's
// whole environment into the pane, so without this an operator with it exported
// would run every Copilot pane as allow-all while tclaude recorded `inherit`.
func TestCopilotBuildCommandScrubsAmbientAllowAll(t *testing.T) {
	s := copilotSpawner{}
	for _, tc := range []struct {
		name string
		spec SpawnSpec
	}{
		{name: "bare launch", spec: SpawnSpec{}},
		{name: "inherit", spec: SpawnSpec{ApprovalPolicy: CopilotApprovalInherit}},
		{name: "allow-tools", spec: SpawnSpec{ApprovalPolicy: CopilotApprovalAllowTools}},
		{name: "resume", spec: SpawnSpec{ResumeID: "11111111-2222-3333-4444-555555555555"}},
		{
			name: "an operator who exported it",
			spec: SpawnSpec{EnvExports: "export COPILOT_ALLOW_ALL='true'; export A=1; "},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := s.BuildCommand(tc.spec)
			scrub := strings.Index(cmd, "unset COPILOT_ALLOW_ALL;")
			if scrub < 0 {
				t.Fatalf("the ambient allow-all scrub is missing: %s", cmd)
			}
			// Position is the assertion, not mere presence: an unset BEFORE the
			// operator's exports would be undone by them.
			if exported := strings.Index(cmd, "export COPILOT_ALLOW_ALL"); exported >= 0 && exported > scrub {
				t.Fatalf("the scrub runs before the export that would defeat it: %s", cmd)
			}
			if strings.Index(cmd, "copilot") < scrub {
				t.Fatalf("the scrub must precede the binary: %s", cmd)
			}
			// Unset, never pinned to a falsy value: the value parse is strict
			// equality against "true" TODAY, and a pinned false would be
			// defeated silently if that ever widened.
			if strings.Contains(cmd, "COPILOT_ALLOW_ALL=false") || strings.Contains(cmd, "COPILOT_ALLOW_ALL=''") {
				t.Fatalf("the ambient variable must be unset, not pinned falsy: %s", cmd)
			}
		})
	}
}

// A directory with a shell metacharacter in its name must land as ONE argument.
// --add-dir values come from a resolved sandbox profile, which is operator data
// rather than a hostile input, but the whole command is handed to `sh -c` and
// this file is the only place that quotes it.
func TestCopilotAddDirQuotesHostilePaths(t *testing.T) {
	cmd := copilotSpawner{}.BuildCommand(SpawnSpec{
		ApprovalPolicy:  CopilotApprovalAllowTools,
		SandboxReadDirs: []string{"/srv/a b; rm -rf /"},
	})
	// filepath.Clean has already dropped the trailing separator by this point;
	// what matters is that the space and the `;` stay inside one argument.
	if !strings.Contains(cmd, "--add-dir '/srv/a b; rm -rf '") {
		t.Fatalf("a path with shell metacharacters was not quoted as one argument: %s", cmd)
	}
}

// Read and write roots both feed --add-dir: Copilot's directory check is not
// read/write split (its dialog is a single "Allow directory access"), so
// modelling a distinction the CLI does not have would only invent one. Deny
// roots are NOT rendered — --add-dir has no negative form, and turning a deny
// into an omission would be indistinguishable from never having had the rule.
func TestCopilotSpawnAddDirsMergesReadAndWriteButNotDenies(t *testing.T) {
	got := copilotSpawnAddDirs(SpawnSpec{
		SandboxReadDirs:  []string{"/srv/read"},
		SandboxWriteDirs: []string{"/srv/write"},
		SandboxDenyDirs:  []string{"/srv/secret"},
	})
	if strings.Join(got, " ") != "/srv/read /srv/write" {
		t.Fatalf("copilotSpawnAddDirs() = %v, want the read and write roots only", got)
	}
	cmd := copilotSpawner{}.BuildCommand(SpawnSpec{
		ApprovalPolicy:   CopilotApprovalAllowTools,
		SandboxReadDirs:  []string{"/srv/read"},
		SandboxWriteDirs: []string{"/srv/write"},
		SandboxDenyDirs:  []string{"/srv/secret"},
	})
	if strings.Contains(cmd, "/srv/secret") {
		t.Fatalf("a denied root reached the launch command: %s", cmd)
	}
	if !strings.Contains(cmd, "--add-dir /srv/read --add-dir /srv/write") {
		t.Fatalf("both grant kinds must be rendered: %s", cmd)
	}
}
