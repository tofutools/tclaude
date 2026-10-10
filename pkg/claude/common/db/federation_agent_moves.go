package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// FederationAgentMove retains provenance even after the bundle payload expires.
// State is separate from the JSON so transitions and abandon are compare-and-swap.
type FederationMoveLink struct {
	Instance string `json:"instance"`
	Agent    string `json:"agent"`
	Offer    string `json:"offer"`
}

type FederationMoveTransfer struct {
	BytesDone  int64  `json:"bytes_done"`
	BytesTotal int64  `json:"bytes_total"`
	State      string `json:"state"`
}

type FederationAgentMove struct {
	Transfer             *FederationMoveTransfer `json:"transfer,omitempty"`
	Disposition          string                  `json:"disposition,omitempty"`
	Cwd                  string                  `json:"cwd,omitempty"`
	Teleport             bool                    `json:"teleport,omitempty"`
	ShutdownPID          int                     `json:"shutdown_pid,omitempty"`
	ShutdownProcessStart string                  `json:"shutdown_process_start,omitempty"`
	ConfirmedAt          time.Time               `json:"confirmed_at,omitempty"`
	MovedFrom            *FederationMoveLink     `json:"moved_from,omitempty"`
	MovedTo              *FederationMoveLink     `json:"moved_to,omitempty"`
	Direction            string                  `json:"direction"`
	Peer                 string                  `json:"peer"`
	ID                   string                  `json:"id"`
	State                string                  `json:"state"`
	SourceAgent          string                  `json:"source_agent"`
	SourceConv           string                  `json:"source_conv"`
	TargetAgent          string                  `json:"target_agent,omitempty"`
	TargetConv           string                  `json:"target_conv,omitempty"`
	SHA256               string                  `json:"sha256"`
	Initiator            string                  `json:"initiator,omitempty"`
	CodexAppServer       bool                    `json:"codex_app_server,omitempty"`
	Human                bool                    `json:"human"`
	Group                string                  `json:"group"`
	SourceGroups         []int64                 `json:"-"`
	ExpiresAt            time.Time               `json:"expires_at"`
	LastError            string                  `json:"last_error,omitempty"`
}

func InsertFederationAgentMove(m FederationAgentMove) error {
	d, err := Open()
	if err != nil {
		return err
	}
	// SourceGroups are private persisted authorization metadata.
	raw, err := json.Marshal(struct {
		FederationAgentMove
		Groups []int64 `json:"source_groups"`
	}{m, m.SourceGroups})
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO federation_agent_moves(direction,peer,id,source_agent,state,payload) VALUES(?,?,?,?,?,?)`, m.Direction, m.Peer, m.ID, m.SourceAgent, m.State, string(raw))
	return err
}
func scanFederationAgentMove(row rowScanner) (*FederationAgentMove, error) {
	var raw, state string
	err := row.Scan(&raw, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v struct {
		FederationAgentMove
		Groups []int64 `json:"source_groups"`
	}
	if err = json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, err
	}
	v.State = state
	v.SourceGroups = v.Groups
	return &v.FederationAgentMove, nil
}
func GetFederationAgentMove(direction, peer, id string) (*FederationAgentMove, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanFederationAgentMove(d.QueryRow(`SELECT payload,state FROM federation_agent_moves WHERE direction=? AND peer=? AND id=?`, direction, peer, id))
}
func ListFederationAgentMoves() ([]FederationAgentMove, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT payload,state FROM federation_agent_moves ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationAgentMove{}
	for rows.Next() {
		m, err := scanFederationAgentMove(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}
func TransitionFederationAgentMove(m FederationAgentMove, from string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	raw, err := json.Marshal(struct {
		FederationAgentMove
		Groups []int64 `json:"source_groups"`
	}{m, m.SourceGroups})
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE federation_agent_moves SET state=?,payload=? WHERE direction=? AND peer=? AND id=? AND state=?`, m.State, string(raw), m.Direction, m.Peer, m.ID, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
func DeleteFederationAgentMove(direction, peer, id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM federation_agent_moves WHERE direction=? AND peer=? AND id=?`, direction, peer, id)
	return err
}

// Progress updates only their JSON projection, avoiding a stale read/write
// overwriting a concurrent running confirmation or disposition transition.
func UpdateFederationMoveTransfer(direction, peer, id string, done, total int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	state := "transferring"
	if done == total {
		state = "downloaded"
	}
	raw, err := json.Marshal(FederationMoveTransfer{done, total, state})
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE federation_agent_moves SET payload=json_set(payload,'$.transfer',json(?)) WHERE direction=? AND peer=? AND id=? AND state='awaiting_confirmation' AND coalesce(json_extract(payload,'$.transfer.bytes_done'),0)<=?`, string(raw), direction, peer, id, done)
	return err
}
