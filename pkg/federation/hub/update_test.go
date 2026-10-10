package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/selfupdate"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestUnsupervisedHubRecoversInterruptedCheckWithoutConfirmingApply(t *testing.T) {
	for _, action := range []string{"check", "apply", "rollback"} {
		t.Run(action, func(t *testing.T) {
			dir := testutil.CanonicalTempDir(t)
			st, err := OpenStore(filepath.Join(dir, "hub.sqlite"))
			require.NoError(t, err)
			defer st.Close()
			updates := filepath.Join(dir, "hub-update")
			require.NoError(t, os.MkdirAll(filepath.Join(updates, "jobs"), 0700))
			job := selfupdate.Job{ID: strings.Repeat("1", 32), Action: action, State: "running", Phase: "checking_release", CurrentVersion: "v1.0.0", StartedAt: time.Now().UTC()}
			raw, err := json.Marshal(selfupdate.Status{Job: &job})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(updates, "status.json"), raw, 0600))
			raw, err = json.Marshal(job)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(updates, "jobs", job.ID+".json"), raw, 0600))
			h, err := New(st, Config{})
			require.NoError(t, err)
			defer h.Close()
			pending := h.updates.Pending()
			require.NotNil(t, pending)
			if action == "check" {
				require.Equal(t, "failed", pending.State)
				require.Contains(t, pending.Error, "new check")
			} else {
				require.Equal(t, "running", pending.State, "plain serving process cannot confirm update health")
			}
		})
	}
}
