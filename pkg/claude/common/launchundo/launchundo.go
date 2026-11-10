// Package launchundo is the unwind list for the state a launch creates
// before it commits. A step registers its undo once, right after it creates
// that state, instead of every later failure return repeating the undo by
// hand. The deferred UnwindUnlessCommitted undoes whatever is still
// registered, newest first, unless the launch committed. Keying the unwind on
// the commit rather than on a returned failure means a return path that
// forgets to report its failure still cleans up.
//
// Usage:
//
//	var undo launchundo.Undo
//	defer undo.UnwindUnlessCommitted()
//	createA()
//	undo.Add(removeA)
//	...
//	undo.Commit()
package launchundo

// Undo is one launch's unwind list. The zero value is ready to use.
type Undo struct {
	steps     []*Step
	committed bool
}

// Step is one registered undo.
type Step struct {
	undo func()
}

// Add registers undo and returns its step, for a caller that later runs or
// releases it early. Call it once, as soon as the state it undoes exists.
func (u *Undo) Add(undo func()) *Step {
	s := &Step{undo: undo}
	u.steps = append(u.steps, s)
	return s
}

// Run undoes the step now; a later unwind skips it.
func (s *Step) Run() {
	if s.undo != nil {
		undo := s.undo
		s.undo = nil
		undo()
	}
}

// Release drops the step without undoing it: its state belongs elsewhere now.
func (s *Step) Release() { s.undo = nil }

// Commit hands the registered state on: the deferred UnwindUnlessCommitted
// leaves it alone. An explicit Unwind (a confirmed failure after the commit)
// still runs it.
func (u *Undo) Commit() { u.committed = true }

// Unwind runs every step not yet run or released, newest first.
func (u *Undo) Unwind() {
	for i := len(u.steps) - 1; i >= 0; i-- {
		u.steps[i].Run()
	}
	u.steps = nil
}

// UnwindUnlessCommitted is the deferred unwind of an uncommitted launch.
func (u *Undo) UnwindUnlessCommitted() {
	if !u.committed {
		u.Unwind()
	}
}
