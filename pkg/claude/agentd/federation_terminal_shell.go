package agentd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// A peer's shell open (SessionOpenPayload.Shell) reaches one of this node's
// tclaude shells: a group's shell in its default directory, or a member
// agent's directory shell. Both are the very shells the browser's local
// routes open (agentShellSpec, groupShellSpec), so a peer lands in the same
// tmux session a click here would. Host shell access is its own grant,
// sessions.shell on the group, which unrestricted trust holds.

// resolveFedShell pins the shell p names for peer. create starts it when it
// is not running (the open); the periodic re-checks pass false, so a shell
// that exits ends the viewer instead of being started again.
func resolveFedShell(peer string, p proto.SessionOpenPayload, create bool) (*fedPanePin, error) {
	trusted, _ := db.GetFederationPeer(peer)
	if trusted == nil {
		return nil, errors.New("peer no longer trusted")
	}
	if !proto.ValidShell(p.Shell) {
		return nil, errors.New("unknown shell")
	}
	g, err := db.GetAgentGroupByName(p.Group)
	if err != nil || g == nil || g.IsArchived() || !fedPeerAllows(peer, g.ID, PermSessionsShell) {
		return nil, errors.New("shell access not granted (needs " + PermSessionsShell + ")")
	}
	var spec shellSpec
	agentID, conv := "", ""
	if p.Shell == proto.ShellGroup {
		if p.Agent != "" {
			return nil, errors.New("a group shell names no agent")
		}
		if spec, err = groupShellSpec(g); err != nil {
			return nil, err
		}
	} else {
		a, err := db.GetAgent(p.Agent)
		if err != nil || a == nil || !a.Active() {
			return nil, errors.New("no active target agent")
		}
		if !fedGroupHasMember(g.ID, a.CurrentConvID) {
			return nil, errors.New("target is not a member of the granted group")
		}
		if spec, _, err = agentShellSpec(a.CurrentConvID, p.Shell, g.Name); err != nil {
			return nil, err
		}
		agentID, conv = p.Agent, a.CurrentConvID
	}
	if create {
		if err := ensureShellSession(spec); err != nil {
			return nil, err
		}
	} else if !session.IsTmuxSessionAlive(spec.name) {
		return nil, errors.New("shell is no longer running")
	}
	pin, err := fedPaneIdentity(spec.name)
	if err != nil {
		return nil, err
	}
	// No cwd: a shell shares no files (sessions.files.read covers agent
	// panes), and its current directory moves while the pin must not.
	pin.agent, pin.conv, pin.session, pin.tmux, pin.group = agentID, conv, "shell:"+p.Shell, spec.name, g.ID
	return pin, nil
}

// fedGroupHasMember reports whether conv is a member of group gid.
func fedGroupHasMember(gid int64, conv string) bool {
	members, err := db.ListAgentGroupMembers(gid)
	if err != nil {
		return false
	}
	for _, m := range members {
		if m.ConvID == conv {
			return true
		}
	}
	return false
}

// validFedOpenTarget checks an open's target fields: an agent's pane, a
// group's shell (no agent), or an agent's directory shell.
func validFedOpenTarget(p proto.SessionOpenPayload) bool {
	if p.Shell == "" {
		return proto.ValidAgentRef(p.Agent)
	}
	if !proto.ValidShell(p.Shell) || p.Group == "" {
		return false
	}
	if p.Shell == proto.ShellGroup {
		return p.Agent == ""
	}
	return proto.ValidAgentRef(p.Agent)
}

// resolveFedShellTarget builds the open for a peer's shell from discovery:
// the peer's catalog must grant this node sessions.shell on group.
func resolveFedShellTarget(peerSel, group, shell, agentRef string, readOnly bool) (*db.FederationPeer, proto.SessionOpenPayload, error) {
	p := proto.SessionOpenPayload{Shell: shell, Group: strings.TrimSpace(group), Agent: agentRef, ReadOnly: readOnly, Cols: 80, Rows: 24}
	if !validFedOpenTarget(p) {
		return nil, proto.SessionOpenPayload{}, errors.New("a shell names its group, and an agent for a directory shell")
	}
	peer, err := resolveFederationPeer(peerSel)
	if err != nil {
		return nil, proto.SessionOpenPayload{}, err
	}
	if !fedCatalogGroupHasCap(peer.InstanceID, p.Group, proto.CapSessionsShell) {
		return nil, proto.SessionOpenPayload{}, fmt.Errorf("%s does not share shells in group %s with you (needs %s)", peerDisplay(peer), p.Group, PermSessionsShell)
	}
	p.Stream = proto.NewEnvelopeID()
	return peer, p, nil
}

// fedViewerTargetStatus re-checks a viewer's target against the peer's
// current catalog: "" while it is still shared, else why not.
func fedViewerTargetStatus(cat *proto.CatalogPayload, p proto.SessionOpenPayload) string {
	if cat == nil {
		return "revoked"
	}
	for _, g := range cat.Groups {
		if g.Name != p.Group {
			continue
		}
		if p.Shell != "" {
			if g.HasCap(proto.CapSessionsShell) {
				return ""
			}
			continue
		}
		if !g.HasCap(fedTerminalCap(p.ReadOnly)) {
			continue
		}
		for _, s := range g.Sessions {
			if s.Agent == p.Agent && s.Session == p.Session {
				if s.Incarnation != p.Incarnation {
					return "reincarnated"
				}
				return ""
			}
		}
	}
	return "revoked"
}
