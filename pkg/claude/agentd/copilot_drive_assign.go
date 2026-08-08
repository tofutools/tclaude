package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Choosing the Copilot API drive has always been possible from several
// surfaces. Un-choosing it durably was possible from none of them (TCL-1082).
//
// TCL-1076 and TCL-1084 removed the ACCIDENTAL routes to clearing the posture —
// a `conv/` surface and a hand-typed `session new` both write nil, meaning "this
// surface did not resolve the drive", because a surface that may not create the
// API channel must not silently revoke a choice it cannot reproduce. Both were
// deliberate narrowings, and both sharpened this gap: they left no DELIBERATE
// route either. This endpoint is that route, and it lives here rather than being
// bolted onto those surfaces so that revocation sits next to selection and
// authority stays exactly as narrow as it was.
//
// # Why the daemon owns the write
//
// The relaunch profile is a whole-blob column, so every writer of it
// read-modify-writes, which is sound only while there is exactly one writer.
// agentd is that writer. The CLI prefers this endpoint for the same reason, and
// falls back to a direct compare-and-set only when the daemon is unreachable and
// only to turn the drive OFF — because a rollback path for an unverified
// mechanism that itself requires the mechanism owner to be healthy is the
// rollback path failing in the case it exists for.

// copilotDriveSendKeys / copilotDriveAPI are the two spellings the wire and the
// CLI share. They name CHANNELS rather than a boolean, because "false" reads as
// a disabled feature while the truth is that the agent is driven the way every
// Copilot agent was driven before the flag existed.
const (
	copilotDriveSendKeys = "send-keys"
	copilotDriveAPI      = "api"
)

// copilotDriveWire is what both directions of the endpoint report.
//
// It reports the RECORD as well as the value on purpose. Two shapes of "durably
// off" that look identical to an operator is a future incident: one is an agent
// whose stable profile says send-keys, the other a conversation whose fallback
// does, and only the second can be silently outvoted later by an agent profile
// that starts answering. Naming the record is what makes the second shape
// debuggable at all.
type copilotDriveWire struct {
	ConvID  string `json:"conv_id"`
	AgentID string `json:"agent_id,omitempty"`
	// Drive is the channel now recorded: "send-keys", "api", or "" when nothing
	// records a drive for this conversation.
	Drive string `json:"drive"`
	// Record names which durable record answered, in the same precedence routing
	// reads: "agent profile", "conversation fallback", or "none".
	Record string `json:"record"`
	// Created distinguishes a record that already carried a drive from one where
	// this write is the first thing to say anything. An operator told "created"
	// learns nothing was recorded before, which is itself the diagnostic for "a
	// lower tier was speaking for this agent".
	Created bool `json:"created,omitempty"`
	// Changed is false when the record already said what was asked for. A no-op
	// reported as a change is a small lie that costs an operator a debugging
	// session later.
	Changed bool `json:"changed"`
	// Live reports that the conversation is running on a connected API channel
	// right now. A pin is durable immediately and does not redirect that channel:
	// copilotAPIDriven answers from the live handle first and reads the records
	// only when there is no handle. Unsaid, that is a rollback the operator
	// believes took effect on this pane.
	Live bool `json:"live,omitempty"`
}

// handleAgentCopilotDrive serves GET (which record decides this conversation's
// drive, and what it says) and POST (write it) on
// /v1/agent/{selector}/copilot-drive.
//
// requirePermission rather than requireCrossAgentPermission, for the same reason
// sandbox-impl does it: that gate confers the slug structurally on an owner of
// any group containing the target, and one direction of this capability puts an
// agent onto a mechanism the operator has NOT verified. A group owner is not
// automatically the person who gets to make that call. Humans pass; an agent
// needs a real grant.
func handleAgentCopilotDrive(w http.ResponseWriter, r *http.Request, convID string) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST only")
		return
	}
	if _, ok := requirePermission(w, r, PermAgentCopilotDrive); !ok {
		return
	}
	if r.Method == http.MethodGet {
		writeCopilotDriveResponse(w, convID, copilotDriveWire{})
		return
	}
	assignCopilotDrive(w, r, convID)
}

// assignCopilotDrive validates and records one drive change.
//
// The check order mirrors the sandbox-impl assignment beside it: the requested
// VALUE first, so a misspelling is a 400 rather than a report on whatever the
// agent happens to be doing; then the harness, which decides whether the field
// means anything here at all; then the state conflicts; then the write.
func assignCopilotDrive(w http.ResponseWriter, r *http.Request, convID string) {
	var body struct {
		// Drive is required. There is no "clear" spelling: clearing a record back
		// to unknown is what the surfaces this ticket exists to complement already
		// do, and an operator asking to un-choose the drive wants a posture that
		// STAYS, not an absence a lower tier can fill in.
		Drive string `json:"drive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "decode request: "+err.Error())
		return
	}
	requested := strings.TrimSpace(body.Drive)
	switch requested {
	case copilotDriveSendKeys, copilotDriveAPI:
	case "":
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"drive is required ("+copilotDriveSendKeys+" or "+copilotDriveAPI+")")
		return
	default:
		writeError(w, http.StatusBadRequest, "invalid_copilot_drive",
			"unknown drive "+requested+" (expected "+copilotDriveSendKeys+" or "+
				copilotDriveAPI+")")
		return
	}
	wantAPI := requested == copilotDriveAPI

	// The drive is Copilot-specific by design (TCL-1051 declined to generalise
	// it). Refusing rather than writing keeps a stray field out of a profile no
	// launch would ever read it from — the same reason the spawn boundary
	// harness-gates it instead of dropping it quietly.
	if h := harnessForConv(convID); h == nil || h.Name != harness.CopilotName {
		name := "unknown"
		if h != nil {
			name = h.Name
		}
		writeError(w, http.StatusUnprocessableEntity, "copilot_drive_wrong_harness",
			"the Copilot drive is a "+harness.CopilotName+"-only posture; conversation "+
				short8(convID)+" runs on "+name)
		return
	}

	// Serialize against the launch paths on the lock a resume takes, so a drive
	// change cannot land between a wake's liveness check and its launch — the
	// window in which a launch would read the old posture and record it back.
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()

	live := copilotAPIConnected(convID)
	// Escalation onto the API drive is refused while the agent is running.
	// --ui-server is a LAUNCH flag: a pane that came up without it has no server,
	// and recording "api" for it would route this conversation's mail into a
	// channel that does not exist. Under TCL-1058 that mail HOLDS rather than
	// falling back to keystrokes, so the visible result is an agent that silently
	// stops receiving messages. Stop the agent, set the drive, wake it.
	if wantAPI && !live && isConvOnline(convID) {
		writeError(w, http.StatusConflict, "copilot_drive_needs_relaunch",
			"the API drive is created by a launch (copilot --ui-server), and this agent is "+
				"running without one; recording it now would route its mail into a channel "+
				"that does not exist. Stop the agent, set the drive, then wake it")
		return
	}

	target, err := db.CopilotDriveTargetForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if target.Record != db.CopilotDriveRecordNone && target.Value == wantAPI {
		writeCopilotDriveResponse(w, convID, copilotDriveWire{Live: live})
		return
	}

	created, record, err := writeCopilotDrive(convID, target, wantAPI)
	if err != nil {
		writeCopilotDriveWriteError(w, err)
		return
	}
	writeCopilotDriveResponse(w, convID, copilotDriveWire{
		Drive: requested, Record: string(record), Created: created, Changed: true, Live: live,
	})
}

// copilotDriveWriteError is a write outcome an operator must be able to tell
// apart from a failure: the record moved under us between the read and the
// compare-and-set, so nothing was written and a retry is the right response.
type copilotDriveWriteError struct{ reason string }

func (e *copilotDriveWriteError) Error() string { return e.reason }

// writeCopilotDrive performs the durable change and reports which record took it
// and whether this write is the first thing to record a drive there.
//
// # Why the seeds live here and not in db
//
// The db compare-and-set functions refuse to invent a parent, deliberately: a
// compare-and-set on a leaf whose parent may not exist is two operations
// pretending to be one. Bringing a record into existence is a policy call, and it
// belongs where it can be named and reported — which is here, where the response
// says CREATED rather than edited.
//
// Two holes need a seed, and only two. json_set APPENDS a missing leaf when the
// parent exists and '$' always exists, so a pin already works wherever any record
// exists to edit inside; what it cannot do is edit inside a record that is not
// there.
func writeCopilotDrive(
	convID string, target db.CopilotDriveTarget, wantAPI bool,
) (created bool, record db.CopilotDriveRecord, err error) {
	switch target.Record {
	case db.CopilotDriveRecordAgentProfile:
		ok, err := db.CompareAndSetAgentCopilotAPI(target.AgentID, wantAPI, target.Raw)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return false, "", &copilotDriveWriteError{
				"this agent's relaunch profile changed while the drive was being written"}
		}
		return false, db.CopilotDriveRecordAgentProfile, nil
	case db.CopilotDriveRecordConversationFallback:
		ok, err := db.CompareAndSetConversationCopilotAPI(convID, wantAPI, target.Raw)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return false, "", &copilotDriveWriteError{
				"this conversation's resume profile changed while the drive was being written"}
		}
		return false, db.CopilotDriveRecordConversationFallback, nil
	}

	// Nothing records a drive. Seed the record the ROUTER would consult first, so
	// the posture that gets written is the one that decides — a write into the
	// lower record while a higher one exists would report success and change
	// nothing that routes.
	if target.AgentID != "" {
		raw, rawErr := db.AgentRelaunchProfileRaw(target.AgentID)
		if rawErr != nil {
			return false, "", rawErr
		}
		if strings.TrimSpace(raw) != "" {
			// A profile exists and simply never recorded a drive: json_set appends
			// the leaf, so this is the ordinary compare-and-set rather than a seed.
			ok, casErr := db.CompareAndSetAgentCopilotAPI(target.AgentID, wantAPI, raw)
			if casErr != nil {
				return false, "", casErr
			}
			if !ok {
				return false, "", &copilotDriveWriteError{
					"this agent's relaunch profile changed while the drive was being written"}
			}
			return true, db.CopilotDriveRecordAgentProfile, nil
		}
		// An empty blob is the one hole a targeted edit cannot fill. Seeding a
		// minimal profile loses nothing, because there is nothing there to lose —
		// which is exactly why this is safe here and would not be safe as a general
		// replace.
		value := wantAPI
		if err := db.SetAgentRelaunchProfile(target.AgentID, db.AgentRelaunchProfile{
			Version: db.RelaunchProfileVersion, CopilotAPI: &value,
		}); err != nil {
			return false, "", err
		}
		return true, db.CopilotDriveRecordAgentProfile, nil
	}

	// No stable agent — a clone, or a plain `session new`. Its drive lives in the
	// conversation fallback, and if that object does not exist yet it is seeded
	// from the conversation's own harness and cwd, the same way a relaunch write
	// already seeds one.
	config, err := durableRelaunchConfigForConv(convID)
	if err != nil {
		return false, "", err
	}
	if err := db.SetConversationCopilotAPI(
		convID, config.Harness, config.Cwd, wantAPI); err != nil {
		return false, "", err
	}
	return true, db.CopilotDriveRecordConversationFallback, nil
}

// writeCopilotDriveWriteError maps a write failure onto the status an operator
// can act on: a guard miss is a retry, everything else is a server fault.
func writeCopilotDriveWriteError(w http.ResponseWriter, err error) {
	var conflict *copilotDriveWriteError
	if errors.As(err, &conflict) {
		writeError(w, http.StatusConflict, "copilot_drive_record_changed",
			conflict.reason+"; nothing was written, read it again and retry")
		return
	}
	writeError(w, http.StatusInternalServerError, "db", err.Error())
}

// writeCopilotDriveResponse answers with the record that currently decides this
// conversation's drive. Re-read rather than echoed, so what an operator is told
// is what a launch would find.
func writeCopilotDriveResponse(w http.ResponseWriter, convID string, base copilotDriveWire) {
	target, err := db.CopilotDriveTargetForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	out := base
	out.ConvID = convID
	out.AgentID = target.AgentID
	out.Record = string(target.Record)
	out.Drive = ""
	if target.Record != db.CopilotDriveRecordNone {
		out.Drive = copilotDriveSendKeys
		if target.Value {
			out.Drive = copilotDriveAPI
		}
	}
	writeJSON(w, http.StatusOK, out)
}
