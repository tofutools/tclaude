package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Link modes. The discriminator stored in agent_group_links.mode.
// Kept as plain strings (rather than an int enum) so a future mode
// can be added without a schema migration.
const (
	LinkModeMembersToMembers = "members->members"
	LinkModeOwnersToMembers  = "owners->members"
)

// ValidLinkMode reports whether mode is one of the v1-supported modes.
// New modes get added here when the feature lands.
func ValidLinkMode(mode string) bool {
	switch mode {
	case LinkModeMembersToMembers, LinkModeOwnersToMembers:
		return true
	}
	return false
}

// AgentGroupLink is a row in agent_group_links — a directed comm edge
// between two groups.
type AgentGroupLink struct {
	ID          int64
	FromGroupID int64
	ToGroupID   int64
	Mode        string
	CreatedAt   time.Time
	ByConv      string
}

// InsertAgentGroupLink records a new edge from→to under mode. Returns
// the new row id, or ErrLinkExists when the edge already exists for
// that (from, to, mode) triple.
func InsertAgentGroupLink(fromID, toID int64, mode, byConv string) (int64, error) {
	if fromID == 0 || toID == 0 {
		return 0, fmt.Errorf("from and to group ids required")
	}
	if fromID == toID {
		return 0, fmt.Errorf("self-links are not allowed (use shared-group instead)")
	}
	if !ValidLinkMode(mode) {
		return 0, fmt.Errorf("invalid link mode %q", mode)
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(
		`INSERT INTO agent_group_links (from_group_id, to_group_id, mode, created_at, by_conv, by_agent)
		 VALUES (?, ?, ?, ?, ?, `+agentForConvExpr+`)`,
		fromID, toID, mode, time.Now().Format(time.RFC3339Nano), byConv, byConv)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return 0, ErrLinkExists
		}
		return 0, err
	}
	return res.LastInsertId()
}

// ErrLinkExists is returned by InsertAgentGroupLink when the (from,
// to, mode) triple already exists. Lets callers turn the case into a
// 409 without re-checking via a SELECT.
var ErrLinkExists = errors.New("link already exists")

// isUniqueConstraintErr checks for SQLite's UNIQUE constraint failure.
// modernc/sqlite returns the message inside the wrapped error.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// UpdateAgentGroupLinkMode changes the mode of an existing link. The
// only mutable field on a link — changing from/to is conceptually
// "delete + recreate" and goes through the regular endpoints. Returns
// ErrLinkExists when the new (from, to, mode) triple collides with
// another row, mirroring InsertAgentGroupLink. Returns the number of
// rows updated (0 when id didn't exist).
func UpdateAgentGroupLinkMode(id int64, mode string) (int64, error) {
	if !ValidLinkMode(mode) {
		return 0, fmt.Errorf("invalid link mode %q", mode)
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE agent_group_links SET mode = ? WHERE id = ?`, mode, id)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return 0, ErrLinkExists
		}
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteAgentGroupLink removes the link with the given id. Returns the
// number of rows affected (0 when the id didn't exist).
func DeleteAgentGroupLink(id int64) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM agent_group_links WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GetAgentGroupLinkByID returns the link with the given primary key,
// or nil if not found.
func GetAgentGroupLinkByID(id int64) (*AgentGroupLink, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(
		`SELECT id, from_group_id, to_group_id, mode, created_at, by_conv
		 FROM agent_group_links WHERE id = ?`, id)
	l, err := scanAgentGroupLink(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return l, err
}

// LinkDirection selects which side of the link to filter on for
// ListAgentGroupLinks.
type LinkDirection string

const (
	LinkOut  LinkDirection = "out"  // edges where groupID is the source
	LinkIn   LinkDirection = "in"   // edges where groupID is the target
	LinkBoth LinkDirection = "both" // either side
)

// ListAgentGroupLinks returns the link rows touching the given group
// in the requested direction.
func ListAgentGroupLinks(groupID int64, dir LinkDirection) ([]*AgentGroupLink, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var (
		rows *sql.Rows
	)
	switch dir {
	case LinkOut:
		rows, err = d.Query(
			`SELECT id, from_group_id, to_group_id, mode, created_at, by_conv
			 FROM agent_group_links WHERE from_group_id = ?
			 ORDER BY id`, groupID)
	case LinkIn:
		rows, err = d.Query(
			`SELECT id, from_group_id, to_group_id, mode, created_at, by_conv
			 FROM agent_group_links WHERE to_group_id = ?
			 ORDER BY id`, groupID)
	case LinkBoth, "":
		rows, err = d.Query(
			`SELECT id, from_group_id, to_group_id, mode, created_at, by_conv
			 FROM agent_group_links
			 WHERE from_group_id = ? OR to_group_id = ?
			 ORDER BY id`, groupID, groupID)
	default:
		return nil, fmt.Errorf("invalid direction %q", dir)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AgentGroupLink
	for rows.Next() {
		l, err := scanAgentGroupLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListAllAgentGroupLinks returns every link in the table, ordered by
// id. Used by the human-facing `groups links` overview verb.
func ListAllAgentGroupLinks() ([]*AgentGroupLink, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(
		`SELECT id, from_group_id, to_group_id, mode, created_at, by_conv
		 FROM agent_group_links
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentGroupLink
	for rows.Next() {
		l, err := scanAgentGroupLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LinkReach is one (sender's group, link, target group) triple that
// expresses "messages sent from senderID through `Via` are allowed to
// land on members of `Target` thanks to `Link`." Used by both the
// auth check and the `why-can-i-message` debug verb.
type LinkReach struct {
	Via    *AgentGroup     // the sender's group (membership or ownership side)
	Target *AgentGroup     // the destination group at the other end of the link
	Link   *AgentGroupLink // the edge that authorises the bridge
	// Why senderID counts as being in Via:
	//   "member" — senderID is a member of Via
	//   "owner"  — senderID is an owner of Via (not a member)
	// owners satisfy any mode (super-member); members only satisfy
	// modes whose source side is "members->...".
	SenderRole string
}

// LinkReachableTargetsFor enumerates every group that senderID can
// reach via at least one outbound link, paired with the routing group
// and link that authorise it. Multiple rows for the same target are
// possible (different bridges); the auth check picks the lowest
// link.ID for determinism.
//
// Implementation: load sender's member groups + owner groups, then
// for each, walk the outbound links and filter by mode against the
// sender's role.
func LinkReachableTargetsFor(senderID string) ([]LinkReach, error) {
	if senderID == "" {
		return nil, nil
	}
	memberGroups, err := ListGroupsForConv(senderID)
	if err != nil {
		return nil, err
	}
	ownerGroupIDs, err := ListGroupsOwnedBy(senderID)
	if err != nil {
		return nil, err
	}

	// Dedupe: a sender that is both member AND owner of the same
	// group counts as "owner" — owner is the strictly stronger role
	// for outbound link purposes (satisfies every mode "member" does
	// plus owners->members). The original implementation inverted
	// this and stranded dual-role senders behind owners-only links.
	ownerSet := make(map[int64]bool, len(ownerGroupIDs))
	for _, gid := range ownerGroupIDs {
		ownerSet[gid] = true
	}

	type pair struct {
		group *AgentGroup
		role  string // "member" | "owner"
	}
	var sources []pair
	// Owners first so dual-role groups end up tagged role="owner".
	for _, gid := range ownerGroupIDs {
		g, err := GetAgentGroupByID(gid)
		if err != nil || g == nil {
			continue
		}
		sources = append(sources, pair{group: g, role: "owner"})
	}
	for _, g := range memberGroups {
		if ownerSet[g.ID] {
			continue
		}
		sources = append(sources, pair{group: g, role: "member"})
	}

	var out []LinkReach
	for _, src := range sources {
		links, err := ListAgentGroupLinks(src.group.ID, LinkOut)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			if !roleSatisfiesLinkMode(src.role, l.Mode) {
				continue
			}
			to, err := GetAgentGroupByID(l.ToGroupID)
			if err != nil || to == nil {
				continue
			}
			out = append(out, LinkReach{
				Via:        src.group,
				Target:     to,
				Link:       l,
				SenderRole: src.role,
			})
		}
	}
	return out, nil
}

// roleSatisfiesLinkMode answers "is this sender authorised to use a
// link of the given mode given their role in the source group?"
//
//	members->members : member ✓, owner ✓ (owner is a super-member)
//	owners->members  : member ✗, owner ✓
func roleSatisfiesLinkMode(role, mode string) bool {
	switch mode {
	case LinkModeMembersToMembers:
		return role == "member" || role == "owner"
	case LinkModeOwnersToMembers:
		return role == "owner"
	}
	return false
}

// scanAgentGroupLink reads a single row from a query that selects
// (id, from_group_id, to_group_id, mode, created_at, by_conv) in
// that order.
func scanAgentGroupLink(scanner interface {
	Scan(dest ...any) error
}) (*AgentGroupLink, error) {
	var l AgentGroupLink
	var createdAt string
	if err := scanner.Scan(&l.ID, &l.FromGroupID, &l.ToGroupID, &l.Mode, &createdAt, &l.ByConv); err != nil {
		return nil, err
	}
	l.CreatedAt = parseTimeOrZero(createdAt)
	return &l, nil
}
