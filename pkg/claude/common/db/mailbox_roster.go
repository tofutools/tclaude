package db

// Read helpers for the dashboard Messages-tab roster (GET /api/mailboxes).
// Each answers a whole-installation question in one statement, so the
// roster's cost does not grow with one query per agent or per group.

// ListPrevGenConvIDs returns the conv-ids that are superseded predecessor
// generations of a still-existing actor — the same set the replaced listing
// (ListReplacedGenerationsPage) pages through.
func ListPrevGenConvIDs() ([]string, error) {
	return queryStrings(`SELECT s.old_conv_id FROM agent_conv_succession s
		JOIN agents a ON a.agent_id = s.agent_id
		WHERE s.old_conv_id <> a.current_conv_id`)
}

// MailboxRosterMatches returns the convs whose display title (the
// mailboxTitleConvsSQL precedence) or owning agent_id contains q, matched
// case-insensitively with LIKE. The roster's filter box also matches
// conv-ids and group names, which the caller checks itself.
func MailboxRosterMatches(q string) ([]string, error) {
	like := "%" + likeEscape(q) + "%"
	return queryStrings(mailboxTitleConvsSQL+`
		UNION ALL
		SELECT ac.conv_id FROM agent_conversations ac
		WHERE ac.agent_id LIKE ? ESCAPE '\'`, like, like, like)
}

// GroupMessageParticipantsAll is GroupMessageParticipants for every group
// at once: group id -> the distinct conv-ids that sent or received a
// message routed through that group (blank ids dropped). One pass over
// the covering idx_agent_messages_parties index instead of a query per
// group.
func GroupMessageParticipantsAll() (map[int64][]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`
		SELECT group_id, from_conv FROM agent_messages WHERE group_id != 0 AND from_conv != ''
		UNION
		SELECT group_id, to_conv FROM agent_messages WHERE group_id != 0 AND to_conv != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64][]string{}
	for rows.Next() {
		var g int64
		var conv string
		if err := rows.Scan(&g, &conv); err != nil {
			return nil, err
		}
		out[g] = append(out[g], conv)
	}
	return out, rows.Err()
}

func queryStrings(q string, args ...any) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
