package evidence

import (
	"fmt"
	"sort"
)

func VerifySequence(manifest []ManifestEntry, nodeLogs []NodeLog) Diagnostics {
	var diagnostics Diagnostics
	diagnostics = append(diagnostics, verifyManifestSequence(manifest)...)
	diagnostics = append(diagnostics, verifyManifestShape(manifest)...)
	diagnostics = append(diagnostics, VerifyManifestChecksums(manifest)...)
	diagnostics = append(diagnostics, verifyManifestLogCrossRefs(manifest, nodeLogs)...)
	return diagnostics
}

func verifyManifestSequence(manifest []ManifestEntry) Diagnostics {
	var diagnostics Diagnostics
	var previous int64
	for i, entry := range manifest {
		path := "manifest[" + itoa(i) + "].seq"
		if entry.Seq <= 0 {
			diagnostics = append(diagnostics, diagError("seq_invalid", path, fmt.Sprintf("manifest seq must be positive, got %d", entry.Seq)))
		}
		if i == 0 {
			previous = entry.Seq
			continue
		}
		if entry.Seq == previous {
			diagnostics = append(diagnostics, diagError("seq_duplicate", path, fmt.Sprintf("duplicate manifest seq %d", entry.Seq)))
		} else if entry.Seq < previous {
			diagnostics = append(diagnostics, diagError("seq_regression", path, fmt.Sprintf("manifest seq regressed from %d to %d", previous, entry.Seq)))
		} else if entry.Seq != previous+1 {
			diagnostics = append(diagnostics, diagError("seq_gap", path, fmt.Sprintf("manifest seq jumped from %d to %d", previous, entry.Seq)))
		}
		previous = entry.Seq
	}
	return diagnostics
}

func verifyManifestShape(manifest []ManifestEntry) Diagnostics {
	var diagnostics Diagnostics
	for i, entry := range manifest {
		path := "manifest[" + itoa(i) + "]"
		diagnostics = append(diagnostics, validateScope(path+".scope", entry.Scope)...)
		if entry.EventRef == "" {
			diagnostics = append(diagnostics, diagError("missing_event_ref", path+".eventRef", "manifest eventRef is required"))
		}
		if entry.EntryChecksum == "" {
			diagnostics = append(diagnostics, diagError("missing_entry_checksum", path+".entryChecksum", "manifest entryChecksum is required"))
		}
	}
	return diagnostics
}

func verifyManifestLogCrossRefs(manifest []ManifestEntry, nodeLogs []NodeLog) Diagnostics {
	manifestBySeq := map[int64]ManifestEntry{}
	manifestRefs := map[string]ManifestEntry{}
	for _, entry := range manifest {
		manifestBySeq[entry.Seq] = entry
		manifestRefs[entry.EventRef] = entry
	}

	logBySeq := map[int64]LogEntry{}
	logRefs := map[string]LogEntry{}
	var diagnostics Diagnostics
	for _, nodeLog := range nodeLogs {
		for i, entry := range nodeLog.Entries {
			path := "nodeLogs." + nodeLog.NodeID + "[" + itoa(i) + "]"
			if entry.Seq <= 0 {
				diagnostics = append(diagnostics, diagError("seq_invalid", path+".seq", fmt.Sprintf("log seq must be positive, got %d", entry.Seq)))
			}
			diagnostics = append(diagnostics, validateScope(path+".scope", entry.Scope)...)
			if !entry.Kind.IsValid() {
				diagnostics = append(diagnostics, diagError("invalid_entry_kind", path+".kind", fmt.Sprintf("invalid log entry kind %q", entry.Kind)))
			}
			if nodeLog.NodeID != "" && entry.Scope.Kind == ScopeNode && entry.Scope.ID != nodeLog.NodeID {
				diagnostics = append(diagnostics, diagError("scope_mismatch", path+".scope", fmt.Sprintf("entry scope node %q does not match node log %q", entry.Scope.ID, nodeLog.NodeID)))
			}
			if entry.Event != nil && entry.Event.Seq != 0 && entry.Event.Seq != entry.Seq {
				diagnostics = append(diagnostics, diagError("event_seq_mismatch", path+".event.seq", fmt.Sprintf("embedded event seq %d does not match envelope seq %d", entry.Event.Seq, entry.Seq)))
			}
			ref := EventRefForLogEntry(entry)
			if _, exists := logBySeq[entry.Seq]; exists {
				diagnostics = append(diagnostics, diagError("seq_duplicate", path+".seq", fmt.Sprintf("duplicate log seq %d", entry.Seq)))
			}
			logBySeq[entry.Seq] = entry
			logRefs[ref] = entry
		}
	}

	for seq, manifestEntry := range manifestBySeq {
		logEntry, ok := logBySeq[seq]
		if !ok {
			diagnostics = append(diagnostics, diagError("manifest_ahead_of_log", "manifest.seq."+itoa64(seq), fmt.Sprintf("manifest seq %d has no corresponding log entry", seq)))
			continue
		}
		ref := EventRefForLogEntry(logEntry)
		if manifestEntry.EventRef != ref {
			diagnostics = append(diagnostics, diagError("event_ref_mismatch", "manifest.seq."+itoa64(seq)+".eventRef", fmt.Sprintf("manifest ref %q does not match log ref %q", manifestEntry.EventRef, ref)))
		}
		if manifestEntry.Scope != logEntry.Scope {
			diagnostics = append(diagnostics, diagError("scope_mismatch", "manifest.seq."+itoa64(seq)+".scope", "manifest scope does not match log entry scope"))
		}
		if !manifestEntry.Timestamp.Equal(logEntry.At) {
			diagnostics = append(diagnostics, diagError("timestamp_mismatch", "manifest.seq."+itoa64(seq)+".ts", "manifest timestamp does not match log entry timestamp"))
		}
		entryChecksum, err := LogEntryChecksum(logEntry)
		if err != nil {
			diagnostics = append(diagnostics, diagError("entry_checksum_error", "manifest.seq."+itoa64(seq)+".entryChecksum", err.Error()))
		} else if manifestEntry.EntryChecksum != entryChecksum {
			diagnostics = append(diagnostics, diagError("entry_checksum_mismatch", "manifest.seq."+itoa64(seq)+".entryChecksum", fmt.Sprintf("manifest entryChecksum %q does not match log entry checksum %q", manifestEntry.EntryChecksum, entryChecksum)))
		}
	}

	for seq, logEntry := range logBySeq {
		if _, ok := manifestBySeq[seq]; !ok {
			diagnostics = append(diagnostics, diagError("log_ahead_of_manifest", EventRefForLogEntry(logEntry), fmt.Sprintf("log seq %d is not present in manifest", seq)))
		}
	}

	for ref := range manifestRefs {
		if _, ok := logRefs[ref]; !ok {
			diagnostics = append(diagnostics, diagError("manifest_ref_missing", "manifest.ref."+ref, fmt.Sprintf("manifest eventRef %q does not resolve to a log entry", ref)))
		}
	}

	sortDiagnostics(diagnostics)
	return diagnostics
}

func validateScope(path string, scope Scope) Diagnostics {
	switch scope.Kind {
	case ScopeNode:
		if scope.ID == "" {
			return Diagnostics{diagError("missing_scope_id", path+".id", "node scope requires id")}
		}
	case ScopeRun:
		if scope.ID != "" {
			return Diagnostics{diagError("run_scope_has_id", path+".id", "run scope must not set id")}
		}
	default:
		return Diagnostics{diagError("invalid_scope_kind", path+".kind", fmt.Sprintf("invalid scope kind %q", scope.Kind))}
	}
	return nil
}

func sortDiagnostics(diagnostics Diagnostics) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		if diagnostics[i].Code != diagnostics[j].Code {
			return diagnostics[i].Code < diagnostics[j].Code
		}
		if diagnostics[i].Path != diagnostics[j].Path {
			return diagnostics[i].Path < diagnostics[j].Path
		}
		return diagnostics[i].Message < diagnostics[j].Message
	})
}
