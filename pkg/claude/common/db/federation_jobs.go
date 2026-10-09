package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type FederationRepo struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Revision   int64              `json:"revision"`
	Definition jobrepo.Definition `json:"definition"`
	Enabled    bool               `json:"enabled"`
}

func SaveFederationRepo(r *FederationRepo) error {
	d, e := Open()
	if e != nil {
		return e
	}
	b, e := json.Marshal(r.Definition)
	if e != nil {
		return e
	}
	if r.ID == "" {
		r.ID = proto.NewEnvelopeID()
		r.Revision = 1
		_, e = d.Exec(`INSERT INTO federation_repos(id,name,revision,definition,enabled) VALUES(?,?,?,?,?)`, r.ID, r.Name, r.Revision, string(b), r.Enabled)
		return e
	}
	result, e := d.Exec(`UPDATE federation_repos SET name=?,revision=revision+1,definition=?,enabled=? WHERE id=? AND revision=?`, r.Name, string(b), r.Enabled, r.ID, r.Revision)
	if e != nil {
		return e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return errors.New("repository changed; reload before saving")
	}
	r.Revision++
	return nil
}
func scanFederationRepo(row interface{ Scan(...any) error }) (*FederationRepo, error) {
	var r FederationRepo
	var raw string
	e := row.Scan(&r.ID, &r.Name, &r.Revision, &raw, &r.Enabled)
	if e != nil {
		return nil, e
	}
	e = json.Unmarshal([]byte(raw), &r.Definition)
	return &r, e
}
func GetFederationRepo(ref string) (*FederationRepo, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	return scanFederationRepo(d.QueryRow(`SELECT id,name,revision,definition,enabled FROM federation_repos WHERE id=? OR name=?`, ref, ref))
}
func ListFederationRepos() ([]FederationRepo, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	rows, e := d.Query(`SELECT id,name,revision,definition,enabled FROM federation_repos ORDER BY name`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []FederationRepo{}
	for rows.Next() {
		r, e := scanFederationRepo(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// Disable preserves immutable identity so deleting and recreating an alias
// cannot authorize an already admitted job against a different repository.
func DisableFederationRepo(ref string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	_, e = d.Exec(`UPDATE federation_repos SET enabled=0,revision=revision+1 WHERE id=? OR name=?`, ref, ref)
	return e
}

type FederationJob struct {
	CallerAgent  string          `json:"caller_agent,omitempty"`
	ID           string          `json:"id"`
	Direction    string          `json:"direction"`
	Peer         string          `json:"peer"`
	Fingerprint  string          `json:"fingerprint"`
	State        string          `json:"state"`
	Request      json.RawMessage `json:"request"`
	RepoID       string          `json:"repo_id"`
	RepoRevision int64           `json:"repo_revision"`
	WorkerID     string          `json:"worker_id"`
	Result       json.RawMessage `json:"result"`
	CreatedAt    time.Time       `json:"created_at"`
	ExpiresAt    time.Time       `json:"expires_at"`
}

const federationJobColumns = `id,direction,peer,fingerprint,state,request,repo_id,repo_revision,worker_id,caller_agent,result,created_at,expires_at`

func scanFederationJob(row interface{ Scan(...any) error }) (*FederationJob, error) {
	var j FederationJob
	var raw, result string
	var created, expires dbTimestamp
	e := row.Scan(&j.ID, &j.Direction, &j.Peer, &j.Fingerprint, &j.State, &raw, &j.RepoID, &j.RepoRevision, &j.WorkerID, &j.CallerAgent, &result, &created, &expires)
	if e != nil {
		return nil, e
	}
	j.Request = json.RawMessage(raw)
	j.Result = json.RawMessage(result)
	j.CreatedAt = created.Time()
	j.ExpiresAt = expires.Time()
	return &j, nil
}
func GetFederationJob(id string) (*FederationJob, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	return scanFederationJob(d.QueryRow(`SELECT `+federationJobColumns+` FROM federation_jobs WHERE id=?`, id))
}
func InsertFederationJob(j *FederationJob) error {
	d, e := Open()
	if e != nil {
		return e
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = time.Now()
	}
	if len(j.Result) == 0 {
		j.Result = json.RawMessage(`{}`)
	}
	_, e = d.Exec(`INSERT INTO federation_jobs (`+federationJobColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, j.ID, j.Direction, j.Peer, j.Fingerprint, j.State, string(j.Request), j.RepoID, j.RepoRevision, j.WorkerID, j.CallerAgent, string(j.Result), dbTime(j.CreatedAt), dbTime(j.ExpiresAt))
	return e
}
func ListFederationJobs(reservations bool) ([]FederationJob, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	q := `SELECT ` + federationJobColumns + ` FROM federation_jobs`
	if reservations {
		q += ` WHERE direction='in' AND state IN ('pending','preparing','running','stopping','unknown')`
	}
	q += ` ORDER BY created_at,id`
	rows, e := d.Query(q)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []FederationJob{}
	for rows.Next() {
		j, e := scanFederationJob(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Compare-and-swap transitions prevent concurrent retries, approvals or cancels
// from starting a job twice. Terminal receipts remain durable replay answers.
func TransitionFederationJob(id, from, to string, result json.RawMessage) error {
	d, e := Open()
	if e != nil {
		return e
	}
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	r, e := d.Exec(`UPDATE federation_jobs SET state=?,result=? WHERE id=? AND state=?`, to, string(result), id, from)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}
