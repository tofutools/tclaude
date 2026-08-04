package harness

// OpenCodeName is the stable identifier persisted for OpenCode sessions.
const OpenCodeName = "opencode"

func init() {
	Register(&Harness{
		Name:                OpenCodeName,
		DisplayName:         "OpenCode",
		Spawn:               openCodeSpawner{},
		Ask:                 openCodeAsker{},
		Models:              openCodeModels{},
		ModelTransport:      unresolvedOpenCodeModelTransport{},
		Sandbox:             openCodeSandbox{},
		TclaudeLayerMode:    OpenCodeSandboxTclaudeLayer,
		Approval:            openCodeApproval{},
		ToolGovernance:      openCodeToolGovernance{},
		ApprovalsReviewer:   false,
		Convs:               openCodeConvStore{},
		Life:                openCodeLifecycle{},
		TmuxScrollback:      true,
		LaunchEnrollment:    true,
		ServerAuthoritative: true,

		// Verbatim the sentence this refusal has always carried; it moved onto
		// the descriptor when Copilot needed a different one.
		BuiltinOSSandboxAbsenceReason: "OpenCode has no built-in OS sandbox; " +
			"its access-control mode is a command filter, not confinement",
	})
}

// OpenCode's pane is an attach client. Compact and exit remain TUI commands,
// but agentd dispatches them through the managed server's authenticated
// tui.command.execute event API rather than prompt keystrokes. Rename is
// deliberately out-of-band through ConvStore.SetTitle for the same reason.
type openCodeLifecycle struct{}

func (openCodeLifecycle) RenameCommand() string        { return "" }
func (openCodeLifecycle) CompactCommand() string       { return "/compact" }
func (openCodeLifecycle) SoftExitCommand() string      { return "/exit" }
func (openCodeLifecycle) RemoteControlCommand() string { return "" }

// OpenCode never receives its soft exit as keystrokes — the daemon dispatches
// app.exit through the managed TUI API — so a pane-input prefix has nothing to
// prepare.
func (openCodeLifecycle) SoftExitPrefixKeys() []string { return nil }
