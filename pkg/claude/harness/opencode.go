package harness

// OpenCodeName is the stable identifier persisted for OpenCode sessions.
const OpenCodeName = "opencode"

func init() {
	Register(&Harness{
		Name:                OpenCodeName,
		DisplayName:         "OpenCode",
		Spawn:               openCodeSpawner{},
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
// while rename is deliberately out-of-band through ConvStore.SetTitle: the
// managed server's authenticated PATCH endpoint is safer than typing a
// user-controlled title into the pane.
type openCodeLifecycle struct{}

func (openCodeLifecycle) RenameCommand() string        { return "" }
func (openCodeLifecycle) CompactCommand() string       { return "/compact" }
func (openCodeLifecycle) SoftExitCommand() string      { return "/exit" }
func (openCodeLifecycle) RemoteControlCommand() string { return "" }
