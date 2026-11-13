package testharness

// simCmd lets the simulator's own tests keep the Run / Output shape they were
// written in. The command has already been applied when it is returned: the
// simulator answers from memory and starts no process.
type simCmd struct {
	out []byte
	err error
}

func (c simCmd) Run() error                      { return c.err }
func (c simCmd) Output() ([]byte, error)         { return c.out, c.err }
func (c simCmd) CombinedOutput() ([]byte, error) { return c.out, c.err }

// Command applies one tmux command to the simulator.
func (t *TmuxSim) Command(args ...string) simCmd {
	out, err := t.Exec(args...)
	return simCmd{out: out, err: err}
}
