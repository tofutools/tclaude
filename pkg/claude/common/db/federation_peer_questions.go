package db

import "time"

// FederationPeerQuestionTTL bounds how long the answer to a question asked
// on a linked node's board is let in (tk_976fe76c).
const FederationPeerQuestionTTL = 30 * 24 * time.Hour

// RecordFederationPeerQuestion remembers that agent asked peer's operator
// the question task: its answer mail is let in once until expires. Expired
// rows are pruned on the way.
func RecordFederationPeerQuestion(peer, task, agent string, now, expires time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM federation_peer_questions WHERE expires_at <= ?`, dbTime(now)); err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO federation_peer_questions (peer, task, agent, created_at, expires_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (peer, task, agent) DO UPDATE SET expires_at = excluded.expires_at`,
		peer, task, agent, dbTime(now), dbTime(expires))
	return err
}

// ConsumeFederationPeerQuestion reports whether a live question matches
// (peer, task, agent), and removes it: the match is single-use.
func ConsumeFederationPeerQuestion(peer, task, agent string, now time.Time) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM federation_peer_questions WHERE peer = ? AND task = ? AND agent = ? AND expires_at > ?`,
		peer, task, agent, dbTime(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
