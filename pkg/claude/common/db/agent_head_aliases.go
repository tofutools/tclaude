package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// HeadAlias is a row in agent_head_aliases. The handle is a stable
// global name; AnchorConvID is the conv we initially pointed at. The
// "current head" is computed by walking the succession chain forward
// from AnchorConvID via ResolveLatestConv at lookup time — no need
// to update this row on every reincarnate.
type HeadAlias struct {
	Handle       string
	AnchorConvID string
	CreatedAt    time.Time
	ByConv       string
}

// SetHeadAlias upserts (handle → anchorConvID). Handle is lower-cased
// so case folding doesn't surprise lookups. byConv is empty when set
// by a human (dashboard / CLI without claude ancestor); a conv-id
// when an agent issued the call. INSERT OR REPLACE — re-pointing an
// existing handle is idempotent.
func SetHeadAlias(handle, anchorConvID, byConv string) error {
	handle = strings.ToLower(strings.TrimSpace(handle))
	if handle == "" {
		return errors.New("SetHeadAlias: handle is required")
	}
	if anchorConvID == "" {
		return errors.New("SetHeadAlias: anchor_conv_id is required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT OR REPLACE INTO agent_head_aliases
		(handle, anchor_conv_id, created_at, by_conv)
		VALUES (?, ?, ?, ?)`,
		handle, anchorConvID, time.Now().Format(time.RFC3339Nano), byConv)
	return err
}

// RemoveHeadAlias drops a handle. Returns the number of rows removed
// (0 when the handle wasn't set).
func RemoveHeadAlias(handle string) (int64, error) {
	handle = strings.ToLower(strings.TrimSpace(handle))
	if handle == "" {
		return 0, errors.New("RemoveHeadAlias: handle is required")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM agent_head_aliases WHERE handle = ?`, handle)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GetHeadAlias returns the row for handle, or nil if unset. Use
// ResolveHeadAlias when you want the current head (chain-walked);
// this raw form exists for the listing / audit view.
func GetHeadAlias(handle string) (*HeadAlias, error) {
	handle = strings.ToLower(strings.TrimSpace(handle))
	if handle == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var h HeadAlias
	var createdAt string
	err = d.QueryRow(`SELECT handle, anchor_conv_id, created_at, by_conv
		FROM agent_head_aliases WHERE handle = ?`, handle).
		Scan(&h.Handle, &h.AnchorConvID, &createdAt, &h.ByConv)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h.CreatedAt = parseTimeOrZero(createdAt)
	return &h, nil
}

// ResolveHeadAlias resolves a handle to the current head conv-id by
// looking up the anchor and walking the succession chain forward.
// Returns "" when the handle is unset.
func ResolveHeadAlias(handle string) (string, error) {
	h, err := GetHeadAlias(handle)
	if err != nil || h == nil {
		return "", err
	}
	return ResolveLatestConv(h.AnchorConvID), nil
}

// ListHeadAliases returns every row (sorted by handle ascending).
// Used by `tclaude agent alias ls` and the dashboard's audit view.
func ListHeadAliases() ([]*HeadAlias, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT handle, anchor_conv_id, created_at, by_conv
		FROM agent_head_aliases ORDER BY handle ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*HeadAlias
	for rows.Next() {
		var h HeadAlias
		var createdAt string
		if err := rows.Scan(&h.Handle, &h.AnchorConvID, &createdAt, &h.ByConv); err != nil {
			return nil, err
		}
		h.CreatedAt = parseTimeOrZero(createdAt)
		out = append(out, &h)
	}
	return out, rows.Err()
}

// validateHeadAliasHandle rejects handles that would collide with the
// `group:<name>` multicast prefix, the `.` / `-` self-selectors, or
// look like UUIDs (which would shadow conv-id selectors). Returns nil
// when the handle is safe to use.
func validateHeadAliasHandle(handle string) error {
	h := strings.ToLower(strings.TrimSpace(handle))
	if h == "" {
		return errors.New("handle is empty")
	}
	if h == "." || h == "-" {
		return fmt.Errorf("handle %q is reserved (self-selector)", handle)
	}
	if strings.HasPrefix(h, "group:") {
		return fmt.Errorf("handle %q is reserved (multicast prefix)", handle)
	}
	if strings.ContainsAny(h, " \t\n\r/\\") {
		return fmt.Errorf("handle %q must not contain whitespace or path separators", handle)
	}
	// UUID-like: 8-4-4-4-12 hex with dashes. Exclude so handles never
	// shadow direct conv-id resolution.
	if len(h) == 36 && h[8] == '-' && h[13] == '-' && h[18] == '-' && h[23] == '-' {
		return fmt.Errorf("handle %q looks like a conv-id; pick a name", handle)
	}
	return nil
}

// ValidateHeadAliasHandle exposes the package-internal handle
// validation for the CLI and HTTP handler layers, both of which need
// to reject the same set of unsafe values before persisting.
func ValidateHeadAliasHandle(handle string) error {
	return validateHeadAliasHandle(handle)
}
