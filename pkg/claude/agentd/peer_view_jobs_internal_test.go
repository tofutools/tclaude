package agentd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A job answers only the peer that started it.
func TestPeerViewJobAnswersOnlyItsPeer(t *testing.T) {
	job := &peerViewJob{peer: "inst_alice0000", done: make(chan struct{}), out: &peerViewResponse{header: make(http.Header)}, finished: time.Now()}
	close(job.done)
	peerViewJobs.Lock()
	peerViewJobs.byID["pvj_test"] = job
	peerViewJobs.Unlock()
	t.Cleanup(func() { peerViewJobs.Lock(); delete(peerViewJobs.byID, "pvj_test"); peerViewJobs.Unlock() })
	read := func(peer string) int {
		req := httptest.NewRequest("GET", "/api/peer-view-jobs/pvj_test", nil)
		req.SetPathValue("id", "pvj_test")
		rec := httptest.NewRecorder()
		servePeerViewJob(rec, req, &peerView{peer: &db.FederationPeer{InstanceID: peer}}, peerViewRule{})
		return rec.Code
	}
	require.Equal(t, 200, read("inst_alice0000"))
	require.Equal(t, 404, read("inst_bob000000"))
}
