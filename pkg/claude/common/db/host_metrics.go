package db

// HostSessionRef avoids loading historical session payloads for the sampler.
type HostSessionRef struct {
	TmuxSession string
	ConvID      string
}

// HostSessionRefs lists non-exited sessions for the live tmux intersection.
func HostSessionRefs() ([]HostSessionRef, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT tmux_session,conv_id FROM sessions WHERE status != 'exited'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []HostSessionRef{}
	for rows.Next() {
		var r HostSessionRef
		if err := rows.Scan(&r.TmuxSession, &r.ConvID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
