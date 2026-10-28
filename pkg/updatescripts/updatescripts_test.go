package updatescripts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseStatusJSONAndPlainText(t *testing.T) {
	st, err := ParseStatus([]byte(`{"items":[{"name":"jq","current":"1.7","latest":"1.8","update_available":true},{"name":"git","update_available":false},{"name":""}]}`), 0)
	require.NoError(t, err)
	require.True(t, *st.UpdateAvailable, "a pending item makes the target pending")
	require.Len(t, st.Items, 2, "nameless items are dropped")

	st, err = ParseStatus([]byte(`{"summary":"fine","update_available":false}`), 0)
	require.NoError(t, err)
	require.Equal(t, "fine", st.Summary)
	require.False(t, *st.UpdateAvailable)

	st, err = ParseStatus([]byte(`{"summary":"no flags"}`), 0)
	require.NoError(t, err)
	require.Nil(t, st.UpdateAvailable, "no flag and exit 0 is unknown, not up to date")

	st, err = ParseStatus([]byte("3 packages can be upgraded\nopenssl\ncurl\n"), ExitUpdatesPending)
	require.NoError(t, err)
	require.Equal(t, "3 packages can be upgraded", st.Summary)
	require.Equal(t, "openssl\ncurl", st.Details)
	require.True(t, *st.UpdateAvailable)

	st, err = ParseStatus([]byte("all current"), 0)
	require.NoError(t, err)
	require.False(t, *st.UpdateAvailable)

	_, err = ParseStatus([]byte("boom"), 2)
	require.ErrorContains(t, err, "exited 2")
	_, err = ParseStatus([]byte(`{"summary":`), 0)
	require.ErrorContains(t, err, "invalid JSON")

	st, err = ParseStatus([]byte(strings.Repeat("é", 500)), 0)
	require.NoError(t, err)
	require.LessOrEqual(t, len(st.Summary), fieldLimit+len("…"))
}

func TestServiceRunsOneUpdateAtATimeAndRechecks(t *testing.T) {
	release := make(chan struct{})
	checks := make(chan string, 4)
	run := func(ctx context.Context, command string, env []string) ([]byte, []byte, int, error) {
		if command == "update" {
			<-release
			return []byte("done"), nil, 0, nil
		}
		checks <- command
		return []byte("ok"), nil, 0, nil
	}
	s := New(Hooks{Scripts: func() ([]Script, error) {
		return []Script{{Name: "a", Status: "status", Update: "update"}, {Name: "b", Update: "update"}}, nil
	}}, run)
	t.Cleanup(s.Close)
	j, err := s.Start("a", "operator")
	require.NoError(t, err)
	_, err = s.Start("b", "operator")
	require.ErrorIs(t, err, ErrBusy)
	_, err = s.Start("missing", "operator")
	require.Error(t, err)
	close(release)
	require.Eventually(t, func() bool { got, _ := s.Job(j.ID); return got.State == "succeeded" }, 2*time.Second, time.Millisecond)
	select {
	case <-checks:
	case <-time.After(2 * time.Second):
		t.Fatal("a finished update re-runs its status script")
	}
}
