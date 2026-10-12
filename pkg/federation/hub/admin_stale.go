package hub

import "time"

const staleAdmissionBatch = 250

type staleAdmission struct {
	Instance string    `json:"instance"`
	Name     string    `json:"name"`
	LastSeen time.Time `json:"last_seen"`
}

// cleanupStaleAdmissions confirms an explicit bounded set, never a moving
// selection. Admins and the requesting node require the existing individual
// revoke action; the bulk action cannot accidentally remove either.
func (h *Hub) cleanupStaleAdmissions(self string, p adminParams, now time.Time) (any, error) {
	if p.Days == 0 {
		p.Days = 30
	}
	if p.Days < 1 || p.Days > 36500 {
		return nil, adminErr(400, "days", "days must be between 1 and 36500")
	}
	cutoff := now.Add(-time.Duration(p.Days) * 24 * time.Hour)
	if p.Apply {
		if len(p.Instances) == 0 || len(p.Instances) > staleAdmissionBatch || p.Cutoff.IsZero() || p.Cutoff.After(cutoff) {
			return nil, adminErr(400, "confirmation_required", "apply requires the preview cutoff and 1–250 explicitly confirmed instance IDs")
		}
		cutoff = p.Cutoff
	}
	admins, err := h.store.Admins()
	if err != nil {
		return nil, err
	}
	protected := map[string]bool{self: true, h.hubID: true}
	for _, a := range admins {
		protected[a.Instance] = true
	}
	h.mu.Lock()
	for id := range h.conns {
		protected[id] = true
	}
	h.mu.Unlock()
	if p.Apply {
		removed, skipped := []string{}, []string{}
		visited := map[string]bool{}
		for _, id := range p.Instances {
			if visited[id] {
				continue
			}
			visited[id] = true
			if protected[id] {
				skipped = append(skipped, id)
				continue
			}
			changed, e := h.store.revoke(id, cutoff)
			if e != nil {
				return nil, e
			}
			if changed {
				removed = append(removed, id)
			} else {
				skipped = append(skipped, id)
			}
		}
		return map[string]any{"revoked": removed, "skipped": skipped}, nil
	}
	rows, err := h.store.List()
	if err != nil {
		return nil, err
	}
	candidates := []staleAdmission{}
	more := false
	for _, row := range rows {
		at := row.LastSeen
		if at.IsZero() {
			at = row.AdmittedAt
		}
		if protected[row.ID] || row.Revoked || at.IsZero() || !at.Before(cutoff) {
			continue
		}
		if len(candidates) == staleAdmissionBatch {
			more = true
			break
		}
		candidates = append(candidates, staleAdmission{row.ID, row.Name, at})
	}
	return map[string]any{"days": p.Days, "cutoff": cutoff, "instances": candidates, "more": more}, nil
}
