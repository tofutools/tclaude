package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

type proofHostFixture struct {
	verifications int
	fail          bool
}

func (*proofHostFixture) ResolveProofDirectories(_ context.Context, paths []string) ([]string, error) {
	out := slices.Clone(paths)
	slices.Sort(out)
	return slices.Compact(out), nil
}
func (h *proofHostFixture) VerifyProofMarkers(context.Context, []string, string) error {
	h.verifications++
	if h.fail {
		return errors.New("missing caller marker")
	}
	return nil
}
func (*proofHostFixture) ReassertProofDirectories(context.Context, []string) error   { return nil }
func (*proofHostFixture) RemoveProofMarkers(context.Context, []string, string) error { return nil }

func TestDirectoryProofChallengeBindsCallerIntentPathsAndExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	caller := model.ExecutionPrincipal("execution", "agent", 1)
	intent := struct{ RequestID, Model string }{"launch", "first"}
	for _, mismatch := range []string{"caller", "generation", "intent", "paths", "expired"} {
		t.Run(mismatch, func(t *testing.T) {
			var registry directoryProofChallenges
			host := &proofHostFixture{}
			_, err := registry.verify(context.Background(), host, caller, intent, "", []string{"/work"}, now)
			var challenge *DirectoryProofRequired
			require.ErrorAs(t, err, &challenge)
			changedCaller, changedIntent, paths, at := caller, intent, []string{"/work"}, now
			switch mismatch {
			case "caller":
				changedCaller.AgentID = "other"
			case "generation":
				changedCaller.Generation++
			case "intent":
				changedIntent.Model = "second"
			case "paths":
				paths = []string{"/other"}
			case "expired":
				at = now.Add(directoryProofTTL)
			}
			_, err = registry.verify(context.Background(), host, changedCaller, changedIntent, challenge.Token, paths, at)
			var replacement *DirectoryProofRequired
			require.ErrorAs(t, err, &replacement)
			require.NotEqual(t, challenge.Token, replacement.Token)
			require.Zero(t, host.verifications, "mismatched proof must not inspect or authorize caller markers")
		})
	}
}

func TestDirectoryProofConsumesSuccessAndFailureAndBoundsOutstandingChallenges(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	caller := model.ExecutionPrincipal("execution", "agent", 1)
	for _, missing := range []bool{false, true} {
		var registry directoryProofChallenges
		host := &proofHostFixture{fail: missing}
		_, err := registry.verify(ctx, host, caller, "exact intent", "", []string{"/b", "/a", "/a"}, now)
		var challenge *DirectoryProofRequired
		require.ErrorAs(t, err, &challenge)
		require.Equal(t, []string{"/a", "/b"}, challenge.Directories)
		dirs, err := registry.verify(ctx, host, caller, "exact intent", challenge.Token, []string{"/a", "/b"}, now)
		if missing {
			require.ErrorIs(t, err, ErrUnauthorized)
		} else {
			require.NoError(t, err)
			require.Equal(t, challenge.Directories, dirs)
		}
		_, err = registry.verify(ctx, host, caller, "exact intent", challenge.Token, []string{"/a", "/b"}, now)
		require.ErrorAs(t, err, &challenge)
		require.Equal(t, 1, host.verifications)
		for range directoryProofLimit * 2 {
			_, err = registry.verify(ctx, host, caller, "exact intent", "", []string{"/a"}, now)
			require.ErrorAs(t, err, &challenge)
		}
		require.Len(t, registry.active, directoryProofLimit)
		_, err = registry.verify(ctx, host, caller, "exact intent", "", []string{"/a"}, now.Add(directoryProofTTL))
		require.ErrorAs(t, err, &challenge)
		require.Len(t, registry.active, 1)
	}
}
