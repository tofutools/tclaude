package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxAgentLineageDepth bounds permission-time recursive lineage walks. A
// corrupt cycle cannot spin, and a path deeper than this fails closed.
const MaxAgentLineageDepth = 64

// RecordAgentLineage records the immutable birth relationship child→parent.
// Repeating the same fact is idempotent; attempting to re-parent an existing
// child is an error rather than silently changing authorization ancestry.
func RecordAgentLineage(childAgentID, parentAgentID string, spawnedAt time.Time) error {
	childAgentID = strings.TrimSpace(childAgentID)
	parentAgentID = strings.TrimSpace(parentAgentID)
	if childAgentID == "" || parentAgentID == "" {
		return errors.New("RecordAgentLineage: child and parent agent ids are required")
	}
	if childAgentID == parentAgentID {
		return errors.New("RecordAgentLineage: child and parent must differ")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`INSERT INTO agent_lineage
		(child_agent_id, parent_agent_id, spawned_at)
		VALUES (?, ?, ?)
		ON CONFLICT(child_agent_id) DO NOTHING`,
		childAgentID, parentAgentID, dbTime(spawnedAt.UTC()))
	if err != nil {
		return err
	}
	inserted, err := res.RowsAffected()
	if err != nil || inserted != 0 {
		return err
	}
	var existing string
	if err := d.QueryRow(`SELECT parent_agent_id FROM agent_lineage
		WHERE child_agent_id = ?`, childAgentID).Scan(&existing); err != nil {
		return err
	}
	if existing != parentAgentID {
		return fmt.Errorf("RecordAgentLineage: child %s already belongs to parent %s", childAgentID, existing)
	}
	return nil
}

// IsDirectAgentChild reports whether target was spawned directly by parent.
func IsDirectAgentChild(parentAgentID, targetAgentID string) (bool, error) {
	parentAgentID = strings.TrimSpace(parentAgentID)
	targetAgentID = strings.TrimSpace(targetAgentID)
	if parentAgentID == "" || targetAgentID == "" || parentAgentID == targetAgentID {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	var one int
	err = d.QueryRow(`SELECT 1 FROM agent_lineage
		WHERE parent_agent_id = ? AND child_agent_id = ?`,
		parentAgentID, targetAgentID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// IsAgentDescendant reports whether target is a transitive child of parent.
// The recursive CTE carries a visited path as well as a hard depth bound, so a
// corrupt cycle terminates without ever making the caller its own descendant.
func IsAgentDescendant(parentAgentID, targetAgentID string) (bool, error) {
	parentAgentID = strings.TrimSpace(parentAgentID)
	targetAgentID = strings.TrimSpace(targetAgentID)
	if parentAgentID == "" || targetAgentID == "" || parentAgentID == targetAgentID {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	var matched int
	err = d.QueryRow(`WITH RECURSIVE descendants(agent_id, depth, path) AS (
			SELECT child_agent_id, 1, ',' || ? || ',' || child_agent_id || ','
			FROM agent_lineage
			WHERE parent_agent_id = ? AND child_agent_id <> ?
			UNION ALL
			SELECT l.child_agent_id, d.depth + 1, d.path || l.child_agent_id || ','
			FROM agent_lineage l
			JOIN descendants d ON l.parent_agent_id = d.agent_id
			WHERE d.depth < ?
			  AND l.child_agent_id <> ?
			  AND instr(d.path, ',' || l.child_agent_id || ',') = 0
		)
		SELECT EXISTS(SELECT 1 FROM descendants WHERE agent_id = ?)`,
		parentAgentID, parentAgentID, parentAgentID,
		MaxAgentLineageDepth, parentAgentID, targetAgentID).Scan(&matched)
	return matched != 0, err
}
