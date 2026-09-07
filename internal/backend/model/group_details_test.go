package model

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestGroupDetailsBoundEachFieldAndRejectUnsafeLinks(t *testing.T) {
	require.NoError(t, ValidateGroupDetails(GroupDetails{Description: strings.Repeat("a", 32768), Mission: "Mission", LinkURL: "https://example.org/task", LinkLabel: "Task"}))
	for _, d := range []GroupDetails{{Description: strings.Repeat("a", 32769)}, {Mission: strings.Repeat("b", 32769)}, {LinkLabel: strings.Repeat("c", 257), LinkURL: "https://example.org"}, {Description: string([]byte{255})}, {Mission: "a\x00b"}, {LinkURL: "//example.org"}, {LinkURL: "https://user:password@example.org"}, {LinkURL: "data:text/html,test"}, {LinkLabel: "orphan"}} {
		require.Error(t, ValidateGroupDetails(d))
	}
}
