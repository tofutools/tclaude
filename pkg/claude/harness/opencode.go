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
		Sandbox:             openCodeSandbox{},
		Convs:               openCodeConvStore{},
		Life:                openCodeLifecycle{},
		TmuxScrollback:      true,
		LaunchEnrollment:    true,
		ServerAuthoritative: true,
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
