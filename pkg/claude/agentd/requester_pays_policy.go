package agentd

import (
	"errors"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func validRequesterPaysPolicy(s string) bool {
	return s == "" || s == "off" || s == "allowed" || s == "required"
}
func requesterPaysPolicy(peer string, group int64, teleport bool) (string, error) {
	mode := "off"
	assignment, err := db.GetFederationNodeProfileAssignment(peer)
	if err != nil {
		return "", err
	}
	if assignment != nil {
		if s := assignment.Profile.Definition.RequesterPays; s != "" {
			mode = s
		}
		if teleport && assignment.Profile.Definition.TeleportLanding != nil && assignment.Profile.Definition.TeleportLanding.RequesterPays != "" {
			mode = assignment.Profile.Definition.TeleportLanding.RequesterPays
		}
	}
	if !teleport {
		grants, err := db.ListEffectiveFederationPeerGrants(peer)
		if err != nil {
			return "", err
		}
		rank := func(g db.FederationPeerGrant) int {
			n := 0
			if g.Scope != "" {
				n = 2
			}
			if g.PoolID == "" {
				n++
			}
			return n
		}
		var best *db.FederationPeerGrant
		for _, g := range grants {
			if g.Slug == PermGroupsMembersSpawn && (g.Scope == "" || g.Scope == db.FederationGroupScope(group)) {
				if best == nil || rank(g) > rank(*best) {
					copy := g
					best = &copy
				}
			}
		}
		if best != nil {
			for _, g := range grants {
				if g.Slug == PermGroupsMembersSpawn && (g.Scope == "" || g.Scope == db.FederationGroupScope(group)) && rank(g) == rank(*best) && g.SpawnPolicy != best.SpawnPolicy {
					return "", errors.New("conflicting receiving spawn policies; set an explicit peer policy")
				}
			}
			if best.SpawnPolicy.RequesterPays != "" {
				mode = best.SpawnPolicy.RequesterPays
			}
		}
	}
	if !validRequesterPaysPolicy(mode) {
		return "", errors.New("invalid receiver requester_pays policy")
	}
	return mode, nil
}
func checkRequesterPays(peer string, group int64, mode, lease string, teleport bool) error {
	policy, err := requesterPaysPolicy(peer, group, teleport)
	if err != nil {
		return err
	}
	if policy == "required" && lease == "" {
		return errors.New("receiver requires requester-pays; use --credentials proxy:<name>@self with a requester gateway")
	}
	if policy == "off" && lease != "" {
		return errors.New("receiver requester-pays policy is off")
	}
	if lease != "" {
		ref, ok := strings.CutPrefix(mode, "proxy:")
		if !ok {
			return errors.New("requester lease requires a gateway credential mode")
		}
		name, gateway, ok := strings.Cut(ref, "@")
		if !ok || !validModelProxyName(name) || gateway != peer {
			return errors.New("requester gateway must be the authenticated requesting instance")
		}
	}
	return nil
}
