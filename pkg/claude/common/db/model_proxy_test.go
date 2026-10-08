package db

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelProxyLaunchHashRestartRotationAndRevocation(t *testing.T) {
	setupTestDB(t)
	bearer := strings.Repeat("a", 64)
	hash := sha256.Sum256([]byte(bearer))
	require.NoError(t, SaveSession(&SessionRow{ID: "launch", Status: "idle", ExitLaunchGeneration: "generation1"}))
	require.NoError(t, BindModelProxyLaunch("launch", "main@peer", hex.EncodeToString(hash[:])))
	ResetForTest() // durable state survives reopening the daemon DB.
	launch, err := VerifyModelProxyLaunch("launch", bearer)
	require.NoError(t, err)
	require.Equal(t, "generation1", launch.Generation)
	_, err = VerifyModelProxyLaunch("launch", strings.Repeat("b", 64))
	require.ErrorIs(t, err, ErrModelProxyRefused)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE sessions SET exit_callback_generation='generation2' WHERE id='launch'`)
	require.NoError(t, err)
	_, err = VerifyModelProxyLaunch("launch", bearer)
	require.ErrorIs(t, err, ErrModelProxyRefused)
	require.NoError(t, BindModelProxyLaunch("launch", "main@peer", hex.EncodeToString(hash[:])))
	require.NoError(t, RevokeModelProxyLaunch("launch", "generation2"))
	_, err = VerifyModelProxyLaunch("launch", bearer)
	require.ErrorIs(t, err, ErrModelProxyRefused)
	require.ErrorIs(t, BindModelProxyLaunch("launch", "main@peer", hex.EncodeToString(hash[:])), ErrModelProxyRefused)
}
func TestModelProxyBudgetsConcurrentIncompleteAndRollover(t *testing.T) {
	setupTestDB(t)
	budget := ModelProxyBudget{Requests: 100, Tokens: 100, PeerRequests: 100, PeerTokens: 100, SessionRequests: 100, SessionTokens: 100}
	var wg sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if ReserveModelProxyRequest(ModelProxyUsage{ID: fmt.Sprint(i), Day: "2026-10-08", Proxy: "main", Peer: "peer", Session: "s", ChargedTokens: 20}, budget) == nil {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	require.EqualValues(t, 5, accepted.Load())
	usage, err := ListModelProxyUsage("2026-10-08")
	require.NoError(t, err)
	first := usage[0]
	first.InputTokens = 1
	require.NoError(t, FinishModelProxyRequest(first)) // incomplete never refunds
	require.ErrorIs(t, ReserveModelProxyRequest(ModelProxyUsage{ID: "refused", Day: first.Day, Proxy: "main", Peer: "peer", Session: "s", ChargedTokens: 1}, budget), ErrModelProxyBudget)
	first.Complete = true
	require.NoError(t, FinishModelProxyRequest(first))
	first.InputTokens = 0
	require.NoError(t, FinishModelProxyRequest(first)) // terminal immutable
	usage, err = ListModelProxyUsage(first.Day)
	require.NoError(t, err)
	require.EqualValues(t, 1, usage[0].ChargedTokens)
	require.NoError(t, ReserveModelProxyRequest(ModelProxyUsage{ID: "tomorrow", Day: "2026-10-09", Proxy: "main", Peer: "peer", Session: "s", ChargedTokens: 100}, budget))
}
