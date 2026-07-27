package agentd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// TCL-791 removed break-glass. This file is what makes the removal LOUD on the
// wire.
//
// The sandbox-profile endpoints decode with a plain json.Decoder and no
// DisallowUnknownFields, so deleting the struct field alone would make
// `break_glass_filesystem` silently ignored — the caller would be told their
// profile saved, and it would not be the profile they sent. For operator input
// that is the exact failure mode this ticket exists to prevent, so every input
// surface refuses it instead.
//
// The complementary rule is that STORED state (session snapshots, registry
// rows) narrows silently rather than refusing: there is no operator present at
// decode time to receive an error, and dropping a grant is fail-closed. See
// NormalizeSnapshotVersion and migrateV162toV163.

// breakGlassRemovedErrorKind is the stable wire kind for input that still
// carries break-glass. It replaces the retired
// "break_glass_acknowledgement_required": the answer is no longer "acknowledge
// it", so reusing that code would send clients down a path that cannot succeed.
const breakGlassRemovedErrorKind = "break_glass_removed"

// breakGlassRemovedReason is the shared explanation. The CLI tombstone
// (agent.breakGlassFlagRemoved) says the same thing in the same words, so an
// operator gets one consistent story whichever surface they hit first.
const breakGlassRemovedReason = "The break-glass feature was removed. It exposed protected tclaude/harness state " +
	"(~/.tclaude/data, ~/.claude/sessions), which is now unreachable from any sandboxed agent — there is no profile, " +
	"include, launch contract, acknowledgement, or flag that reopens it. To work without the protected-root wall, " +
	"launch with the sandbox disabled."

// breakGlassFieldsPresent reports whether a decoded payload still carries
// either tombstoned field.
//
// An empty `break_glass_filesystem: []` counts as ABSENT: a client that
// round-trips an old export through a serializer emitting empty slices is not
// asking for protected access, and refusing it would block a profile that is
// already exactly what this ticket wants. A non-empty list, or the
// acknowledgement flag in any form, counts as present.
func breakGlassFieldsPresent(rules json.RawMessage, acknowledged *bool) bool {
	if acknowledged != nil {
		return true
	}
	if len(bytes.TrimSpace(rules)) == 0 {
		return false
	}
	// Decode rather than compare against the literal "[]": a client that pretty
	// prints its payload sends "[\n]", which is the same empty list and must get
	// the same answer. Anything that does not parse as a JSON array counts as
	// present, so a malformed value is refused rather than waved through.
	// "null" decodes to a nil slice, i.e. absent, which is what we want.
	var entries []json.RawMessage
	if err := json.Unmarshal(rules, &entries); err != nil {
		return true
	}
	return len(entries) > 0
}

// breakGlassRemovedFailure builds the typed 422. what names the operation
// ("save", "import", "assign globally", …) so the message reads as an
// instruction rather than a bare refusal.
func breakGlassRemovedFailure(what string) *spawnFailure {
	return &spawnFailure{http.StatusUnprocessableEntity, breakGlassRemovedErrorKind,
		"refusing to " + what + " a sandbox profile carrying break_glass_filesystem: it is no longer accepted. " +
			breakGlassRemovedReason + " Remove the field and retry."}
}

// rejectBreakGlassPayload gates one profile payload.
func rejectBreakGlassPayload(what string, body sandboxProfileJSON) *spawnFailure {
	if !breakGlassFieldsPresent(body.BreakGlassFilesystem, body.BreakGlassAcknowledged) {
		return nil
	}
	return breakGlassRemovedFailure(what)
}

// rejectBreakGlassEnvelope gates a whole import bundle, naming EVERY profile
// inside it that still carries the field. An operator hand-editing a bundle
// needs the full list, not a first-offender error that makes them re-run the
// import once per profile.
func rejectBreakGlassEnvelope(env sandboxProfileExportEnvelope) *spawnFailure {
	if env.BreakGlassAcknowledged != nil {
		return &spawnFailure{http.StatusUnprocessableEntity, breakGlassRemovedErrorKind,
			"refusing to import a bundle carrying break_glass_acknowledged: there is nothing left to acknowledge. " +
				breakGlassRemovedReason + " Remove the field and retry."}
	}
	carriers := []string{}
	for _, profile := range env.Profiles {
		if breakGlassFieldsPresent(profile.BreakGlassFilesystem, profile.BreakGlassAcknowledged) {
			carriers = append(carriers, profile.Name)
		}
	}
	if len(carriers) == 0 {
		return nil
	}
	sort.Strings(carriers)
	return &spawnFailure{http.StatusUnprocessableEntity, breakGlassRemovedErrorKind,
		"refusing to import: sandbox profile(s) " + strings.Join(carriers, ", ") +
			" in this bundle carry break_glass_filesystem, which is no longer accepted. " +
			breakGlassRemovedReason + " Remove the field from those profiles and retry."}
}

// assignmentBreakGlassBody is the decode target for the two assignment
// endpoints, whose body is just a profile name plus the retired
// acknowledgement.
type assignmentBreakGlassBody struct {
	BreakGlassAcknowledged *bool `json:"break_glass_acknowledged,omitempty"`
}

// rejectBreakGlassSpawn gates the launch surface. A spawn payload was the one
// place an agent could try to acknowledge on its own behalf, so an ignored
// field here would read to the caller as "the escalation went through".
func rejectBreakGlassSpawn(acknowledged *bool) *spawnFailure {
	if acknowledged == nil {
		return nil
	}
	return &spawnFailure{http.StatusUnprocessableEntity, breakGlassRemovedErrorKind,
		"refusing to spawn with break_glass_acknowledged: there is nothing left to acknowledge. " +
			breakGlassRemovedReason + " Remove the field and retry."}
}

// rejectBreakGlassAssignment gates a global or group assignment.
func rejectBreakGlassAssignment(scope string, acknowledged *bool) *spawnFailure {
	if acknowledged == nil {
		return nil
	}
	return &spawnFailure{http.StatusUnprocessableEntity, breakGlassRemovedErrorKind,
		"refusing to set the " + scope + " sandbox profile with break_glass_acknowledged: there is nothing left to " +
			"acknowledge. " + breakGlassRemovedReason + " Remove the field and retry."}
}
