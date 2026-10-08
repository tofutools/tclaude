package db

import "sync/atomic"

// statusGeneration invalidates gathered runtime views after local mutations.
// It is independent of permissions: consumers must still authorize projection.
var statusGeneration atomic.Uint64

func StatusSnapshotGeneration() uint64 { return statusGeneration.Load() }

// NotifyStatusChanged is also used after daemon-owned tmux lifecycle actions,
// whose liveness change may precede the accompanying session-row update.
func NotifyStatusChanged() { statusGeneration.Add(1) }
