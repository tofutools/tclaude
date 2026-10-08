package agentd

import (
	"errors"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type federationJobContextKey struct{}
type federationJobLaunch struct {
	LivePath     string
	Peer         string
	RepoID       string
	RepoRevision int64
	Permissions  map[string]db.PermissionOverride
	ID           string
	WorkerID     string
	GroupID      int64
	Cwd          string
	Harness      string
	Defaults     *db.FederationWorkerDefaults
}

// The tmux helper cannot exec until this callback completes and the parent
// creates its private permit. All API calls from the worker can therefore be
// attributed to a registered actor with its frozen profile defaults.
func (j *federationJobLaunch) Enroll(target string) error {
	fedNodeGroupsMu.Lock()
	defer fedNodeGroupsMu.Unlock()
	repo, e := db.GetFederationRepo(j.RepoID)
	p, pe := db.GetFederationPeer(j.Peer)
	if e != nil || pe != nil || p == nil || repo.Revision != j.RepoRevision || !jobRepoAllows(repo, j.GroupID) || fedPeerGroupGrant(j.Peer, j.GroupID, PermJobsRun) == nil {
		return errors.New("job authority changed before exec")
	}
	conv := "job-" + j.ID
	if _, _, e := db.EnsureAgentForConvWithID(conv, j.WorkerID, "remote-job"); e != nil {
		return e
	}
	if e := db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: j.GroupID, ConvID: conv, Role: "worker", Descr: "remote one-shot job"}); e != nil {
		return e
	}
	if e := db.ApplyAgentPermissionOverrides(conv, j.Permissions, "remote-job-launch", false, false); e != nil {
		return e
	}
	if j.Defaults != nil && len(j.Defaults.Permissions) > 0 {
		if e := db.ApplyAgentPermissionOverrides(conv, j.Defaults.Permissions, "node-profile:"+j.Defaults.ProfileID, false, false); e != nil {
			return e
		}
	}
	if e := db.SaveSession(&db.SessionRow{ID: conv, TmuxSession: target, ConvID: conv, Cwd: j.Cwd, Harness: j.Harness, Status: "working", CreatedAt: time.Now(), UpdatedAt: time.Now()}); e != nil {
		return fmt.Errorf("record job session: %w", e)
	}
	return nil
}
