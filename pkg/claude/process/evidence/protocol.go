package evidence

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

// DualWriteProtocol documents the storage contract that TCL-269 implementations
// must follow for every state-changing process event.
//
// The ordered write discipline is:
//  1. append exactly one LogEntry to the owning node log;
//  2. append exactly one ManifestEntry for that log entry, using the next
//     run-global seq, the log entry's content checksum, and the rolling
//     manifest checksum;
//  3. write the state checkpoint whose LastLogSeq and LogChecksum match the
//     manifest head after applying the entry's state.Event payload.
//
// Crash interpretation:
//   - node log ahead of manifest: side-effect evidence exists but was not
//     indexed; recovery may append the missing manifest entry or truncate the
//     log tail after operator review;
//   - manifest ahead of state: evidence is durable and indexed, but the
//     checkpoint is stale; recovery may replay manifest-backed entries and
//     rewrite state;
//   - state ahead of manifest/checksum mismatch: checkpoint is inconsistent and
//     must not auto-advance until repaired.
//
// This package does not perform atomic file operations, fsync, locking, or
// renames. It only defines the portable formats and pure verification helpers
// that filesystem, SQLite, and external stores must honor.
const DualWriteProtocol = "append-log-entry -> append-manifest-entry -> write-state-checkpoint"

func VerifyStateAnchors(st *state.State, manifest []ManifestEntry) Diagnostics {
	if st == nil {
		return Diagnostics{diagError("nil_state", "state", "process state is nil")}
	}
	var diagnostics Diagnostics
	var headSeq int64
	if len(manifest) > 0 {
		headSeq = manifest[len(manifest)-1].Seq
	}
	if st.LastLogSeq < headSeq {
		diagnostics = append(diagnostics, diagError("state_behind_manifest", "state.lastLogSeq", fmt.Sprintf("state lastLogSeq %d is behind manifest head %d", st.LastLogSeq, headSeq)))
	} else if st.LastLogSeq > headSeq {
		diagnostics = append(diagnostics, diagError("state_ahead_of_manifest", "state.lastLogSeq", fmt.Sprintf("state lastLogSeq %d is ahead of manifest head %d", st.LastLogSeq, headSeq)))
	}
	checksum, err := ManifestChecksum(manifest)
	if err != nil {
		diagnostics = append(diagnostics, diagError("checksum_error", "manifest", err.Error()))
		return diagnostics
	}
	if st.LogChecksum != checksum {
		diagnostics = append(diagnostics, diagError("state_checksum_mismatch", "state.logChecksum", fmt.Sprintf("state logChecksum %q does not match manifest checksum %q", st.LogChecksum, checksum)))
	}
	return diagnostics
}
