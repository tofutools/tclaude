package harness

// OpenCodeName is the stable identifier persisted for OpenCode sessions.
const OpenCodeName = "opencode"

func init() {
	Register(&Harness{
		Name:                OpenCodeName,
		DisplayName:         "OpenCode",
		Spawn:               openCodeSpawner{},
		Models:              openCodeModels{},
		Life:                openCodeLifecycle{},
		TmuxScrollback:      true,
		LaunchEnrollment:    true,
		ServerAuthoritative: true,
	})
}

// OpenCode's pane is an attach client. These commands are interpreted by that
// TUI while the conversation itself remains in the managed server.
type openCodeLifecycle struct{}

func (openCodeLifecycle) RenameCommand() string        { return "/rename" }
func (openCodeLifecycle) CompactCommand() string       { return "/compact" }
func (openCodeLifecycle) SoftExitCommand() string      { return "/exit" }
func (openCodeLifecycle) RemoteControlCommand() string { return "" }
