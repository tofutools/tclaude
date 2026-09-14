package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// NetworkSyncLaunch is a private host-side mailbox. The supervisor alone
// acknowledges kernel installation; a profile save only queues a request.
// It deliberately does not replace the actor's filesystem launch snapshot.
type NetworkSyncLaunch struct {
	ID           string                  `json:"id"`
	SessionID    string                  `json:"session_id"`
	Snapshot     sandboxpolicy.Snapshot  `json:"-"`
	Dependencies []int64                 `json:"profile_ids"`
	Requested    *sandboxpolicy.Snapshot `json:"-"`
	Revision     int64                   `json:"revision"`
	Acknowledged int64                   `json:"acknowledged"`
	Status       string                  `json:"status"`
	Detail       string                  `json:"detail"`
	Heartbeat    int64                   `json:"heartbeat"`
	Automatic    bool                    `json:"automatic"`
}

func ResolveNetworkSyncSnapshot(previous sandboxpolicy.Snapshot) (sandboxpolicy.Snapshot, error) {
	if previous.ProfilesOmitted {
		return previous, nil
	}
	var explicit int64
	for _, p := range previous.Applied {
		if p.Scope == sandboxpolicy.ScopeExplicit {
			explicit = p.ID
		}
	}
	current, err := ResolveEffectiveSandboxSnapshotByID(previous.ResolutionGroupID, explicit)
	current.NetworkAutoSync = previous.NetworkAutoSync
	return current, err
}

func networkSyncDependencies(snapshot sandboxpolicy.Snapshot) ([]int64, error) {
	profiles, err := ListSandboxProfiles()
	if err != nil {
		return nil, err
	}
	byID := map[int64]*SandboxProfile{}
	byName := map[string]*SandboxProfile{}
	for _, p := range profiles {
		byID[p.ID] = p
		byName[p.Name] = p
	}
	seen := map[int64]bool{}
	var visit func(*SandboxProfile)
	visit = func(p *SandboxProfile) {
		if p == nil || seen[p.ID] {
			return
		}
		seen[p.ID] = true
		for _, name := range p.Includes {
			visit(byName[name])
		}
	}
	for _, p := range snapshot.Applied {
		visit(byID[p.ID])
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

func RegisterNetworkSyncLaunch(id, sessionID string, snapshot sandboxpolicy.Snapshot) error {
	deps, err := networkSyncDependencies(snapshot)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	depJSON, _ := json.Marshal(deps)
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO network_sync_launches(id,session_id,snapshot,launch_snapshot,dependencies,heartbeat) VALUES(?,?,?,?,?,0)`, id, sessionID, string(raw), string(raw), string(depJSON))
	return err
}

func NetworkSyncLaunches() ([]NetworkSyncLaunch, error) { return networkSyncLaunches("") }

func networkSyncLaunches(id string) ([]NetworkSyncLaunch, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,session_id,snapshot,dependencies,requested,revision,acknowledged,status,detail,heartbeat FROM network_sync_launches WHERE heartbeat > ? AND (?='' OR id=?) ORDER BY session_id`, time.Now().Add(-30*time.Second).Unix(), id, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []NetworkSyncLaunch{}
	for rows.Next() {
		var p NetworkSyncLaunch
		var snapshot, deps, request string
		if err := rows.Scan(&p.ID, &p.SessionID, &snapshot, &deps, &request, &p.Revision, &p.Acknowledged, &p.Status, &p.Detail, &p.Heartbeat); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(snapshot), &p.Snapshot); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(deps), &p.Dependencies); err != nil {
			return nil, err
		}
		if request != "" {
			if err := json.Unmarshal([]byte(request), &p.Requested); err != nil {
				return nil, err
			}
		}
		p.Automatic = p.Snapshot.NetworkAutoSync
		out = append(out, p)
	}
	return out, rows.Err()
}

func ReadNetworkSyncLaunch(id string) (*NetworkSyncLaunch, error) {
	rows, err := networkSyncLaunches(id)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.ID == id {
			return &row, nil
		}
	}
	return nil, fmt.Errorf("network sync launch is no longer registered")
}

func NetworkSyncHeartbeat(id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE network_sync_launches SET heartbeat=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

func FinishNetworkSyncLaunch(id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE network_sync_launches SET status='stopped',heartbeat=0 WHERE id=?`, id)
	return err
}

func AcknowledgeNetworkSync(id string, revision int64, status, detail string, snapshot *sandboxpolicy.Snapshot) error {
	d, err := Open()
	if err != nil {
		return err
	}
	if snapshot == nil {
		_, err = d.Exec(`UPDATE network_sync_launches SET acknowledged=?,status=?,detail=? WHERE id=? AND revision=?`, revision, status, detail, id, revision)
		return err
	}
	deps, err := networkSyncDependencies(*snapshot)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	depJSON, _ := json.Marshal(deps)
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE network_sync_launches SET acknowledged=?,status=CASE WHEN revision=? THEN ? ELSE 'pending' END,detail=?,snapshot=?,dependencies=? WHERE id=?`, revision, revision, status, detail, string(raw), string(depJSON), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	// Publish only the network axis to existing authority readers. The original
	// launch snapshot remains in the mailbox for audit; mounts never change.
	var sessionID, agentID, previous string
	err = tx.QueryRow(`SELECT id,agent_id,effective_sandbox_config FROM sessions WHERE network_sync_id=? LIMIT 1`, id).Scan(&sessionID, &agentID, &previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && previous != "" {
		old, err := unmarshalEffectiveSandboxSnapshot(previous)
		if err != nil {
			return err
		}
		if old != nil {
			old.Effective.Network = snapshot.Effective.Network
			old.Effective.NetworkAccess = snapshot.Effective.NetworkAccess
			old.Effective.Provenance.Network = snapshot.Effective.Provenance.Network
			updated, err := marshalEffectiveSandboxSnapshot(old)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(`UPDATE sessions SET effective_sandbox_config=? WHERE id=? AND network_sync_id=?`, updated, sessionID, id); err != nil {
				return err
			}
			if agentID != "" {
				if _, err = tx.Exec(`UPDATE agents SET effective_sandbox_config=? WHERE agent_id=? AND current_conv_id=(SELECT conv_id FROM sessions WHERE id=? AND network_sync_id=?)`, updated, agentID, sessionID, id); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

// QueueProfileNetworkSync also consults the last applied dependencies: removing
// an include must update consumers that have just ceased to depend on it.
func QueueProfileNetworkSync(profileID int64, includeManual bool) ([]NetworkSyncLaunch, error) {
	rows, err := NetworkSyncLaunches()
	if err != nil {
		return nil, err
	}
	queued := []NetworkSyncLaunch{}
	for _, row := range rows {
		if !includeManual && !row.Automatic {
			continue
		}
		current, err := ResolveNetworkSyncSnapshot(row.Snapshot)
		if err != nil {
			if !slices.Contains(row.Dependencies, profileID) {
				continue
			}
			row.Status = "failed"
			row.Detail = err.Error()
			if ackErr := AcknowledgeNetworkSync(row.ID, row.Revision, row.Status, row.Detail, nil); ackErr != nil {
				return queued, ackErr
			}
			queued = append(queued, row)
			continue
		}
		deps, err := networkSyncDependencies(current)
		if err != nil {
			return queued, err
		}
		if !slices.Contains(row.Dependencies, profileID) && !slices.Contains(deps, profileID) {
			continue
		}
		raw, err := json.Marshal(current)
		if err != nil {
			return queued, err
		}
		d, err := Open()
		if err != nil {
			return queued, err
		}
		_, err = d.Exec(`UPDATE network_sync_launches SET requested=?,revision=revision+1,status='pending',detail='' WHERE id=?`, string(raw), row.ID)
		if err != nil {
			return queued, err
		}
		row.Revision++
		row.Status = "pending"
		queued = append(queued, row)
	}
	return queued, nil
}

// BeginNetworkSyncLaunch binds updates to this concrete running generation.
// A successor replaces the token, so an old supervisor cannot publish over it.
func BeginNetworkSyncLaunch(id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`UPDATE sessions SET network_sync_id=? WHERE id=(SELECT id FROM sessions WHERE id=(SELECT session_id FROM network_sync_launches WHERE id=?) OR agent_id=(SELECT session_id FROM network_sync_launches WHERE id=?) ORDER BY updated_at DESC LIMIT 1)`, id, id, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE network_sync_launches SET heartbeat=? WHERE id=?`, time.Now().Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}
