package sandboxpolicy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeychainOptOutSurvivesCompositionAndSnapshots(t *testing.T) {
	base := &Profile{Name: "base", DarwinDisableKeychainWrite: true}
	flat, err := Flatten(Profile{Name: "local", Includes: []string{"base"}}, registryLookup(map[string]*Profile{"base": base}))
	require.NoError(t, err)
	require.True(t, flat.DarwinDisableKeychainWrite)
	effective, err := Resolve(Scopes{Global: &flat, Explicit: &Profile{Name: "explicit"}})
	require.NoError(t, err)
	require.True(t, effective.DarwinDisableKeychainWrite)
	restricted := Snapshot{Version: SnapshotVersion, Effective: effective}
	encoded, err := json.Marshal(restricted)
	require.NoError(t, err)
	var decoded Snapshot
	err = json.Unmarshal(encoded, &decoded)
	require.NoError(t, err)
	assert.True(t, decoded.Effective.DarwinDisableKeychainWrite)
	allowed := restricted
	allowed.Effective.DarwinDisableKeychainWrite = false
	require.ErrorContains(t, RequireContained(restricted, allowed), "Keychain")
	require.NoError(t, RequireContained(allowed, restricted))
}
