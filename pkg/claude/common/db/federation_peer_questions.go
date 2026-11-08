package db

import (
	"database/sql"
	"errors"
	"time"
)

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

// ConsumeFederationPeerQuestion takes the live question matching (peer,
// task, agent), if any: the match is single-use. It returns the record's
// expiry, for RecordFederationPeerQuestion to put it back when the mail it
// let in is refused after all (a full inbox the sender retries).
func ConsumeFederationPeerQuestion(peer, task, agent string, now time.Time) (time.Time, bool, error) {
	d, err := Open()
	if err != nil {
		return time.Time{}, false, err
	}
	var expires dbTimestamp
	err = d.QueryRow(`DELETE FROM federation_peer_questions WHERE peer = ? AND task = ? AND agent = ? AND expires_at > ?
		RETURNING expires_at`, peer, task, agent, dbTime(now)).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return expires.Time(), true, nil
}
