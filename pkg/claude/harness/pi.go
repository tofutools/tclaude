package harness

// PiName is the --harness value for pi, the open-source coding agent from
// earendil-works/pi (npm @earendil-works/pi-coding-agent, MIT).
const PiName = "pi"

// PiMinimumVersion is the oldest pi release tclaude drives. Every claim in the
// pi adapter was observed against it with the real binary: `--session-id`
// creating a missing session, per-launch extensions with the ui_prompt events,
// and system messages in the session transcript are all recent.
const PiMinimumVersion = "1.1.0"

func init() {
	Register(&Harness{
		Name:        PiName,
		DisplayName: "pi",

		Spawn:  piSpawner{},
		Models: piModels{},
		// Conversations are pi's session files; a transfer carries one as
		// is, and the transcript codec translates it for a cross-harness
		// morph. See pi_convstore.go, pi_history.go and transcript_pi.go.
		Convs:           piConvStore{},
		History:         piHistory{},
		Transcript:      piTranscript{},
		StatusExtension: MaterializePiExtension,
		// `tclaude ask`, auto-naming and the séance; see pi_asker.go. A
		// generation recorded under tclaude's sandbox replays inside it.
		Ask:               piAsker{},
		OneShotReplay:     OneShotReplayTclaudeLayer,
		ConfinedApprovals: []string{PiApprovalAuto},
		CheckVersion:      CheckPiVersion,
		// The model's provider names one endpoint; see pi_model_transport.go.
		ModelTransport: piModelTransport{},
		Life:           piLifecycle{},

		// `pi --session-id <id>` opens the session with that exact id, creating
		// it when it does not exist, so the id is known before the pane starts.
		// `session new` mints one for a fresh launch that was not handed one,
		// and a full id is all a resume needs: the spawner resolves it to the
		// session file itself.
		LaunchEnrollment:         true,
		PresetSessionID:          PresetSessionIDUUID,
		MintFreshSessionID:       true,
		ResumeFullIDWithoutStore: true,

		// pi has no sandbox of any kind; its documentation delegates isolation
		// to containers. The catalog's single mode records that honestly, and
		// tclaude's own layer is the boundary. See pi_sandbox.go.
		Sandbox: piSandbox{},
		// tclaude's sandbox is the single wall: pi runs against tclaude's
		// walled copy of its agent directory, the operator's is read-only
		// apart from its session store (pi_walled_agent.go), and the
		// extension and pi's runtime are mounted read-only.
		TclaudeLayerMode:              PiSandboxOff,
		BuiltinOSSandbox:              false,
		BuiltinOSSandboxAbsenceReason: PiBuiltinOSSandboxAbsenceReason,

		// pi never asks before a tool call. The catalog names that posture
		// (`auto`), tclaude's human-gated permission extension (`ask`) and a
		// read-only tool set. See pi_approval.go.
		Approval: piApproval{},

		// A working directory with project-local pi resources (.pi/settings.json,
		// .pi/extensions, .agents/skills, …) parks an interactive launch on a
		// project-trust dialog. Its decision store is pi's trust.json, which
		// --trust-dir seeds. See pi_dir_trust.go.
		DirTrust: true,

		// pi runs session_shutdown from its own exit path only; a killed
		// process never reports it. Exit detection stays with the reaper.
		SessionEndBestEffort: true,

		// pi's fullscreen TUI owns its scroll-back and mouse wheel.
		TmuxScrollback: false,
	})
}

// piLifecycle names pi's in-pane control commands. They are compile-time
// constants because tclaude types them into a tmux pane (an injection sink);
// never interpolate user input into them. Each was observed in a tmux pane
// against PiMinimumVersion.
type piLifecycle struct{}

// `/name <title>` sets the session's display name: pi appends a session_info
// entry to the transcript and retitles the pane.
func (piLifecycle) RenameCommand() string { return "/name" }

// `/compact [instructions]` summarizes older history in place.
func (piLifecycle) CompactCommand() string { return "/compact" }

// `/quit` exits with status 0.
func (piLifecycle) SoftExitCommand() string { return "/quit" }

// pi has no hosted remote-access relay (`/share` uploads a static export) and
// no fast-mode toggle.
func (piLifecycle) RemoteControlCommand() string { return "" }
func (piLifecycle) FastModeCommand() string      { return "" }

// Escape aborts a running turn (queued messages return to the editor) and
// Ctrl+C then clears the editor, so the typed command lands on an empty
// prompt. One Ctrl+C on an empty editor does not exit.
func (piLifecycle) SoftExitPrefixKeys() []string { return []string{"Escape", "C-c"} }

// Two Ctrl+C presses on an empty editor exit pi with status 0. Escape leads to
// abort a running turn first; the third press is margin for one that lands
// after pi's repeat window, and a surplus press on an exited pane is tolerated
// by the injector.
func (piLifecycle) SignalExitKeys() []string {
	return []string{"Escape", "C-c", "C-c", "C-c"}
}
