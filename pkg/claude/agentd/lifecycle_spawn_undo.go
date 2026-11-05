package agentd

// spawnUndo is executeSpawn's unwind list for the state a launch creates
// before its pane starts: a step registers its undo once, right after it
// creates that state, instead of every later failure return repeating the
// undo by hand. The deferred unwindUnlessCommitted undoes whatever is still
// registered, newest first, unless the launch committed; keying it on the
// commit rather than on a returned failure means a return path that forgets
// to report its failure still cleans up.
type spawnUndo struct {
	steps     []*spawnUndoStep
	committed bool
}

// spawnUndoStep is one registered undo.
type spawnUndoStep struct {
	undo func()
}

// add registers undo and returns its step, for a caller that later runs or
// releases it early.
func (u *spawnUndo) add(undo func()) *spawnUndoStep {
	s := &spawnUndoStep{undo: undo}
	u.steps = append(u.steps, s)
	return s
}

// run undoes the step now; a later unwind skips it.
func (s *spawnUndoStep) run() {
	if s.undo != nil {
		undo := s.undo
		s.undo = nil
		undo()
	}
}

// release drops the step without undoing it: its state belongs elsewhere now.
func (s *spawnUndoStep) release() { s.undo = nil }

// commit hands the registered state on: the deferred unwind leaves it alone.
// An explicit unwind (a confirmed failure after the commit) still runs it.
func (u *spawnUndo) commit() { u.committed = true }

// unwind runs every step not yet run or released, newest first.
func (u *spawnUndo) unwind() {
	for i := len(u.steps) - 1; i >= 0; i-- {
		u.steps[i].run()
	}
	u.steps = nil
}

// unwindUnlessCommitted is the deferred unwind of an uncommitted launch.
func (u *spawnUndo) unwindUnlessCommitted() {
	if !u.committed {
		u.unwind()
	}
}
