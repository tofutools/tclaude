package db

import (
	"fmt"
	"time"
)

// AgentGroupOwner is a row in agent_group_owners. Owners can message
// a group's members and multicast to the group without being members
// themselves. Distinct from membership so the "X is an owner but
// not a peer" case is representable.
type AgentGroupOwner struct {
	GroupID int64
	// AgentID is the owner's stable actor key — ownership is keyed on it
	// (JOH-26), so it is the canonical, rotation-immune identity to display.
	AgentID   string
	ConvID    string
	GrantedAt time.Time
	GrantedBy string
}

// AddAgentGroupOwner records convID as an owner of groupID. Idempotent
// (INSERT OR IGNORE). grantedBy is "" for human-issued grants, a
// conv-id when an agent with permissions.grant did it.
func AddAgentGroupOwner(groupID int64, convID, grantedBy string) error {
	if convID == "" {
		return fmt.Errorf("conv_id required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	// Owning a group makes the conv an agent. EnsureAgentForConv mints / links
	// the actor; ownership is then keyed on agent_id (JOH-26).
	if _, _, err := EnsureAgentForConv(convID, "group"); err != nil {
		return err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return fmt.Errorf("AddAgentGroupOwner: no actor for conv %s", convID)
	}
	res, err := d.Exec(
		`INSERT OR IGNORE INTO agent_group_owners (group_id, agent_id, granted_at, granted_by)
		 SELECT ?, ?, ?, ? FROM agents WHERE agent_id = ? AND retired_at IS NULL`,
		groupID, agentID, dbTime(time.Now()), grantedBy, agentID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var active int
		if err := d.QueryRow(`SELECT COUNT(*) FROM agents WHERE agent_id = ? AND retired_at IS NULL`, agentID).Scan(&active); err != nil {
			return err
		}
		if active == 0 {
			return fmt.Errorf("AddAgentGroupOwner: agent %s is retired", agentID)
		}
	}
	return nil
}

// RemoveAgentGroupOwner clears an ownership row. Returns the number
// of rows removed (0 when convID wasn't an owner).
func RemoveAgentGroupOwner(groupID int64, convID string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	res, err := d.Exec(
		`DELETE FROM agent_group_owners WHERE group_id = ? AND agent_id = ?`,
		groupID, agentID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// IsAgentGroupOwner returns true when convID's actor owns groupID. Resolves
// the conv to its stable agent_id (JOH-26), so ANY generation of the actor —
// not just its current conv — answers correctly.
func IsAgentGroupOwner(groupID int64, convID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return false, err
	}
	if agentID == "" {
		return false, nil
	}
	var n int
	err = d.QueryRow(
		`SELECT COUNT(*) FROM agent_group_owners WHERE group_id = ? AND agent_id = ?`,
		groupID, agentID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// OwnerHasGroupContaining reports whether ownerConv owns any group containing
// targetConv. Both conversations are matched through their stable agent IDs,
// and the existence check stays a single bounded query rather than an N+1
// owned-groups/membership scan.
func OwnerHasGroupContaining(ownerConv, targetConv string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var exists int
	err = d.QueryRow(`SELECT EXISTS (
		SELECT 1
		FROM agent_group_owners o
		JOIN agent_group_members m ON m.group_id = o.group_id
		WHERE o.agent_id = (
			SELECT agent_id FROM agent_conversations WHERE conv_id = ?
		)
		AND m.agent_id = (
			SELECT agent_id FROM agent_conversations WHERE conv_id = ?
		)
		LIMIT 1
	)`, ownerConv, targetConv).Scan(&exists)
	return exists != 0, err
}

// ListAgentGroupOwners returns every owner row for the given group,
// most-recently-granted first.
func ListAgentGroupOwners(groupID int64) ([]*AgentGroupOwner, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return queryRows(d, func(rows rowScanner) (*AgentGroupOwner, error) {
		var o AgentGroupOwner
		var grantedAt dbTimestamp
		if err := rows.Scan(&o.GroupID, &o.AgentID, &o.ConvID, &grantedAt, &o.GrantedBy); err != nil {
			return nil, err
		}
		o.GrantedAt = grantedAt.Time()
		return &o, nil
	},
		`SELECT o.group_id, o.agent_id, ag.current_conv_id, o.granted_at, o.granted_by
		 FROM agent_group_owners o JOIN agents ag ON ag.agent_id = o.agent_id
		 WHERE o.group_id = ?
		 ORDER BY o.granted_at DESC`,
		groupID)
}

// OwnedGroupScopes is one owned group's identity plus the only field the
// owner-bypass evaluation needs from it. It exists so a caller that must
// consult EVERY owned group's owner-scope map pays one query rather than
// one per group — the dashboard roster runs that walk per agent, per poll.
type OwnedGroupScopes struct {
	ID              int64
	Name            string
	OwnerScopesJSON string
}

// ListOwnedGroupScopes returns the owner-scope narrowing carried by every
// group convID owns, in one query. A group whose row cannot be read is not
// silently absent: the query is a join, so a missing group simply has no
// ownership row either.
func ListOwnedGroupScopes(convID string) ([]OwnedGroupScopes, error) {
	if convID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return queryRows(d, func(rows rowScanner) (OwnedGroupScopes, error) {
		var g OwnedGroupScopes
		if err := rows.Scan(&g.ID, &g.Name, &g.OwnerScopesJSON); err != nil {
			return OwnedGroupScopes{}, err
		}
		return g, nil
	}, `SELECT g.id, g.name, g.owner_scopes_json
		FROM agent_group_owners o
		JOIN agent_groups g ON g.id = o.group_id
		WHERE o.agent_id = (SELECT agent_id FROM agent_conversations WHERE conv_id = ?)
		  AND g.archived_at IS NULL`, convID)
}

// ListGroupsOwnedBy returns every group_id convID owns. Used by the
// auth check that asks "is the sender an owner of any group the
// target is a member of?".
func ListGroupsOwnedBy(convID string) ([]int64, error) {
	if convID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, nil
	}
	return queryRows(d, func(rows rowScanner) (int64, error) {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		return id, nil
	}, `SELECT group_id FROM agent_group_owners WHERE agent_id = ?`, agentID)
}

// CanSenderReachTarget returns the routing group when a message from
// senderID to targetID is authorised, plus a label describing the
// reason (so callers can echo it back to the user). Returns
// (nil, "", nil) when no path exists.
//
// Rules, in order of preference:
//   - shared membership: pick the first group both belong to.
//   - sender-as-owner: pick the first group the sender owns that
//     also contains the target.
//   - via-link: sender is in (or owns) some group A which has an
//     outbound link to a group B that contains the target. The link
//     mode determines whether membership alone suffices or ownership
//     is required (see roleSatisfiesLinkMode).
//
// The order is stable so audit logs ("which path authorised this?")
// keep meaning across versions: tightening the model adds new reasons
// at the end, never reorders the existing ones.
func CanSenderReachTarget(senderID, targetID string) (*AgentGroup, string, error) {
	shared, err := SharedGroupsForConvs(senderID, targetID)
	if err != nil {
		return nil, "", err
	}
	if len(shared) > 0 {
		return shared[0], "shared-group", nil
	}
	ownerGroups, err := ListGroupsOwnedBy(senderID)
	if err != nil {
		return nil, "", err
	}
	for _, gID := range ownerGroups {
		members, err := ListAgentGroupMembers(gID)
		if err != nil {
			continue
		}
		for _, m := range members {
			if m.ConvID == targetID {
				g, err := GetAgentGroupByID(gID)
				if err != nil || g == nil {
					continue
				}
				return g, "owner-of-group", nil
			}
		}
	}
	// via-link: walk outbound link reach and check membership of the
	// target group on the far side.
	reach, err := LinkReachableTargetsFor(senderID)
	if err != nil {
		return nil, "", err
	}
	// Pick the link with the lowest id for determinism — matches the
	// "first by name" convention used by SharedGroupsForConvs.
	var bestVia *AgentGroup
	var bestLinkID int64
	bestReason := ""
	for _, r := range reach {
		members, err := ListAgentGroupMembers(r.Target.ID)
		if err != nil {
			continue
		}
		hit := false
		for _, m := range members {
			if m.ConvID == targetID {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if bestVia == nil || r.Link.ID < bestLinkID {
			bestVia = r.Via
			bestLinkID = r.Link.ID
			bestReason = fmt.Sprintf("via-link:%d", r.Link.ID)
		}
	}
	if bestVia != nil {
		return bestVia, bestReason, nil
	}
	return nil, "", nil
}

// FindAgentMembersBySelector returns every row whose conv_id matches
// the selector. Used as a fallback by the agent CLI's target resolver:
// a conv that was just added to a group via spawn lives in
// agent_group_members before its .jsonl is scanned into conv_index, so
// the conv_index-only resolver can't find it by title yet — but its
// conv_id is already resolvable here.
//
// Match rules:
//   - exact conv_id
//   - prefix on conv_id (8+ chars typed, like the rest of tclaude)
//
// Returns rows in the order seen; the caller is responsible for
// deduping by conv_id and reporting ambiguity.
func FindAgentMembersBySelector(selector string) ([]*AgentGroupMember, error) {
	if selector == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	// Membership is agent-keyed; match the selector against ANY of the actor's
	// conversation generations (agent_conversations), then return its rows with
	// the actor's current conv for the ConvID field. The IN-subquery (rather
	// than a JOIN on agent_conversations) avoids fan-out when a prefix matches
	// several generations of the same actor.
	q := `SELECT m.group_id, ag.current_conv_id, m.role, m.descr, m.joined_at
		FROM agent_group_members m
		JOIN agents ag ON ag.agent_id = m.agent_id
		WHERE m.agent_id IN (
			SELECT agent_id FROM agent_conversations WHERE conv_id = ? OR conv_id LIKE ?
		)
		ORDER BY m.joined_at DESC`
	return queryRows(d, func(rows rowScanner) (*AgentGroupMember, error) {
		var m AgentGroupMember
		var joined dbTimestamp
		if err := rows.Scan(&m.GroupID, &m.ConvID, &m.Role, &m.Descr, &joined); err != nil {
			return nil, err
		}
		m.JoinedAt = joined.Time()
		return &m, nil
	}, q, selector, selector+"%")
}
