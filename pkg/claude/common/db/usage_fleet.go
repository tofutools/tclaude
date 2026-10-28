package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

// Fleet limit targets (see migrate_v279_usage_fleet_limits.go).
const (
	UsageFleetTargetAll        = "all"
	UsageFleetTargetPoolPrefix = "pool:"
	UsageFleetTargetPeerPrefix = "peer:"
)

// UsageFleetLimit is one rule over spend summed across nodes: every trusted
// peer sharing its usage, one local node pool (a sub-fleet) or one peer, plus
// this node's own when IncludeSelf.
type UsageFleetLimit struct {
	ID          int64           `json:"id"`
	Target      string          `json:"target"`
	IncludeSelf bool            `json:"include_self"`
	Rule        usagerules.Rule `json:"rule"`
	Enabled     bool            `json:"enabled"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// ValidUsageFleetTarget checks a target's shape; whether the pool or peer
// exists is the caller's to check.
func ValidUsageFleetTarget(target string) error {
	if target == UsageFleetTargetAll {
		return nil
	}
	for _, prefix := range []string{UsageFleetTargetPoolPrefix, UsageFleetTargetPeerPrefix} {
		if id, ok := strings.CutPrefix(target, prefix); ok && strings.TrimSpace(id) == id && id != "" && len(id) <= 256 {
			return nil
		}
	}
	return fmt.Errorf("fleet limit target %q must be \"all\", \"pool:<id>\" or \"peer:<instance>\"", target)
}

const usageFleetLimitSelect = `SELECT id, target, include_self, rule, enabled, created_at, updated_at FROM usage_fleet_limits`

func scanUsageFleetLimit(s rowScanner) (*UsageFleetLimit, error) {
	var l UsageFleetLimit
	var rule string
	var created, updated dbTimestamp
	if err := s.Scan(&l.ID, &l.Target, &l.IncludeSelf, &rule, &l.Enabled, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(rule), &l.Rule); err != nil {
		return nil, fmt.Errorf("usage fleet limit %d: decode rule: %w", l.ID, err)
	}
	l.CreatedAt, l.UpdatedAt = created.time, updated.time
	return &l, nil
}

// ListUsageFleetLimits returns every fleet limit, oldest first.
func ListUsageFleetLimits() ([]*UsageFleetLimit, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(usageFleetLimitSelect + ` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*UsageFleetLimit
	for rows.Next() {
		l, err := scanUsageFleetLimit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetUsageFleetLimit returns one fleet limit, or nil.
func GetUsageFleetLimit(id int64) (*UsageFleetLimit, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	l, err := scanUsageFleetLimit(d.QueryRow(usageFleetLimitSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return l, err
}

// CreateUsageFleetLimit stores l and sets its ID.
func CreateUsageFleetLimit(l *UsageFleetLimit) error {
	if err := ValidUsageFleetTarget(l.Target); err != nil {
		return err
	}
	rule, err := json.Marshal(l.Rule)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	now := time.Now()
	res, err := d.Exec(`INSERT INTO usage_fleet_limits (target, include_self, rule, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		l.Target, l.IncludeSelf, string(rule), l.Enabled, dbTime(now), dbTime(now))
	if err != nil {
		return err
	}
	l.ID, err = res.LastInsertId()
	l.CreatedAt, l.UpdatedAt = now, now
	return err
}

// UpdateUsageFleetLimit replaces everything but the ID of l.ID. It reports
// whether the limit existed.
func UpdateUsageFleetLimit(l *UsageFleetLimit) (bool, error) {
	if err := ValidUsageFleetTarget(l.Target); err != nil {
		return false, err
	}
	rule, err := json.Marshal(l.Rule)
	if err != nil {
		return false, err
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	now := time.Now()
	res, err := d.Exec(`UPDATE usage_fleet_limits SET target = ?, include_self = ?, rule = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		l.Target, l.IncludeSelf, string(rule), l.Enabled, dbTime(now), l.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	l.UpdatedAt = now
	return n > 0, err
}

// DeleteUsageFleetLimit removes a fleet limit.
func DeleteUsageFleetLimit(id int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM usage_fleet_limits WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
