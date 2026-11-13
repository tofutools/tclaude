package session

import (
	"database/sql"
	"errors"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The launch operations that are not about the session row: the read that
// settles a launch's identity, the conversation and agent lookups, and the
// Codex and Darwin route registrations. See launch_ops.go.

type launchConvArgs struct {
	launchSessionArgs
	ConvID string `json:"conv_id"`
}

type launchConvTitleArgs struct {
	launchSessionArgs
	ConvID  string `json:"conv_id"`
	Title   string `json:"title"`
	Harness string `json:"harness"`
}

// launchResumedConvArgs keys a read by the conversation a launch resumes.
type launchResumedConvArgs struct {
	ConvID string `json:"conv_id"`
}

func (a launchResumedConvArgs) launchSessionID() string  { return "" }
func (a launchResumedConvArgs) launchGeneration() string { return "" }
func (a launchResumedConvArgs) launchConvKey() string    { return a.ConvID }

type launchSessionStatesResult struct {
	States []*SessionState `json:"states,omitempty"`
}

type launchAgentIDResult struct {
	AgentID string `json:"agent_id"`
}

type launchSessionStateResult struct {
	// State is nil when the session has no row.
	State *SessionState `json:"state,omitempty"`
}

// launchCodexProfileArgs registers the permission profile this launch
// generated. The definition travels as content: the daemon never opens a path
// the launcher names.
type launchCodexProfileArgs struct {
	launchGenerationArgs
	ProfileName string `json:"profile_name"`
	ProfileTOML string `json:"profile_toml"`
	OwnerConvID string `json:"owner_conv_id,omitempty"`
}

type launchCodexProfileResult struct {
	// NativeGeneration is "" when no registry is installed on the host.
	NativeGeneration string `json:"native_generation,omitempty"`
}

// launchCodexRuntimeProfileArgs and launchCodexRuntimeValueArgs name the
// Codex app-server runtime agentd started for this launch; the daemon checks
// the name against the one it minted the ticket with.
type launchCodexRuntimeProfileArgs struct {
	launchSessionArgs
	RuntimeGeneration string `json:"runtime_generation"`
	ProfileName       string `json:"profile_name"`
	ProfileTOML       string `json:"profile_toml"`
}

func (a launchCodexRuntimeProfileArgs) launchCodexRuntime() string { return a.RuntimeGeneration }

type launchCodexRuntimeValueArgs struct {
	launchSessionArgs
	RuntimeGeneration string `json:"runtime_generation"`
	Value             string `json:"value"`
}

func (a launchCodexRuntimeValueArgs) launchCodexRuntime() string { return a.RuntimeGeneration }

type launchDarwinRouteArgs struct {
	launchGenerationArgs
	AgentID string `json:"agent_id"`
	ConvID  string `json:"conv_id"`
	Slots   []int  `json:"slots,omitempty"`
}

// codexLaunchNativeGeneration is the registry generation of the profile an
// ordinary launch generates: derived from the launch generation, so an
// operation on it is fenced by the launch generation it carries.
func codexLaunchNativeGeneration(launchGeneration string) string {
	return "launch:" + launchGeneration
}

var (
	// opLoadSessionState reads the row a launch is about to take over, to
	// refuse a label a live session still owns. It runs before the launch
	// has a generation, so it is authorized by the ticket alone.
	opLoadSessionState = defineLaunchOp("session.load_state", fenceTicketOnly,
		func(a launchSessionArgs) (launchSessionStateResult, error) {
			state, err := LoadSessionState(a.SessionID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return launchSessionStateResult{}, err
			}
			return launchSessionStateResult{State: state}, nil
		})

	// opSessionsForConv reads the session rows of the conversation a launch
	// resumes, to refuse a second live pane on it. Like opLoadSessionState it
	// runs before the launch has a generation.
	opSessionsForConv = defineLaunchOp("session.for_resumed_conv", fenceTicketOnly,
		func(a launchResumedConvArgs) (launchSessionStatesResult, error) {
			rows, err := db.FindSessionsByConvID(a.ConvID)
			if err != nil {
				return launchSessionStatesResult{}, err
			}
			var out launchSessionStatesResult
			for _, row := range rows {
				if row != nil {
					out.States = append(out.States, fromRow(row))
				}
			}
			return out, nil
		})

	opAgentIDForConv = defineLaunchOp("agent.id_for_conv", fenceSessionOnly,
		func(a launchConvArgs) (launchAgentIDResult, error) {
			agentID, err := db.AgentIDForConv(a.ConvID)
			return launchAgentIDResult{AgentID: agentID}, err
		})

	opSetConvTitle = defineLaunchOp("conv.set_custom_title", fenceSessionOnly,
		func(a launchConvTitleArgs) (launchNoResult, error) {
			return noResult(db.SetConvIndexCustomTitle(a.ConvID, a.Title, a.Harness))
		})

	opCodexRegisterLaunchProfile = defineLaunchOp("codex.register_launch_profile", fenceSessionOnly,
		func(a launchCodexProfileArgs) (launchCodexProfileResult, error) {
			generation, err := registerCodexLaunchNativeProfile(
				a.Generation, a.ProfileName, "", a.ProfileTOML, a.OwnerConvID, a.SessionID)
			return launchCodexProfileResult{NativeGeneration: generation}, err
		})

	opCodexUnregisterLaunchProfile = defineLaunchOp("codex.unregister_launch_profile", fenceSessionOnly,
		func(a launchGenerationArgs) (launchNoResult, error) {
			return noResult(UnregisterCodexNativePermissionProfile(codexLaunchNativeGeneration(a.Generation)))
		})

	opCodexActivateLaunchProfile = defineLaunchOp("codex.activate_launch_profile", fenceCurrentGeneration,
		func(a launchGenerationArgs) (launchNoResult, error) {
			return noResult(ActivateCodexNativePermissionProfile(codexLaunchNativeGeneration(a.Generation)))
		})

	opCodexRegisterAppServerProfile = defineLaunchOp("codex.register_app_server_profile", fenceSessionOnly,
		func(a launchCodexRuntimeProfileArgs) (launchNoResult, error) {
			return noResult(registerCodexAppServerProfileTOML(a.RuntimeGeneration, a.ProfileName, a.ProfileTOML))
		})

	opCodexSetAppServerVersion = defineLaunchOp("codex.set_app_server_version", fenceCurrentGeneration,
		func(a launchCodexRuntimeValueArgs) (launchNoResult, error) {
			return noResult(db.SetCodexAppServerRuntimeVersion(a.RuntimeGeneration, a.Value))
		})

	opCodexMarkAppServerUnavailable = defineLaunchOp("codex.mark_app_server_unavailable", fenceSessionOnly,
		func(a launchCodexRuntimeValueArgs) (launchNoResult, error) {
			return noResult(db.MarkCodexAppServerRuntimeUnavailable(a.RuntimeGeneration, a.Value))
		})

	// The Darwin route claim. Each statement is keyed by the launch
	// generation it carries.
	opDarwinRegisterRoute = defineLaunchOp("darwin.register_route_launch", fenceSessionOnly,
		func(a launchDarwinRouteArgs) (launchNoResult, error) {
			return noResult(db.RegisterDarwinRouteLaunch(a.AgentID, a.ConvID, a.Generation, a.Slots))
		})

	opDarwinActivateRoute = defineLaunchOp("darwin.activate_route_launch", fenceCurrentGeneration,
		func(a launchDarwinRouteArgs) (launchNoResult, error) {
			return noResult(db.ActivateDarwinRouteLaunch(a.AgentID, a.ConvID, a.Generation))
		})

	opDarwinDeleteRoute = defineLaunchOp("darwin.delete_route_launch", fenceSessionOnly,
		func(a launchDarwinRouteArgs) (launchNoResult, error) {
			return noResult(db.DeleteDarwinRouteLaunch(a.AgentID, a.ConvID, a.Generation))
		})
)

func launchConv(sessionID, convID string) launchConvArgs {
	return launchConvArgs{launchSessionArgs: launchSession(sessionID), ConvID: convID}
}
