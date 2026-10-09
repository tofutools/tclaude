package agentd

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/nodeinfo"
	"github.com/tofutools/tclaude/pkg/testutil"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHarnessAvailabilityCacheRefresh(t *testing.T) {
	availabilityCache.Lock()
	old := availabilityCache.value
	availabilityCache.value = nodeinfo.Availability{}
	availabilityCache.Unlock()
	t.Cleanup(func() { availabilityCache.Lock(); availabilityCache.value = old; availabilityCache.Unlock() })
	dir := testutil.CanonicalTempDir(t)
	t.Setenv("PATH", dir)
	binary := filepath.Join(dir, "codex")
	write := func(version string) {
		require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'codex "+version+"\\n'\n"), 0700))
	}
	version := func(a nodeinfo.Availability) string {
		for _, h := range a.Harnesses {
			if h.Name == "codex" {
				return h.Version
			}
		}
		return ""
	}
	write("one")
	first := cachedHarnessAvailability(false)
	require.Equal(t, "codex one", version(first))
	write("two")
	require.Equal(t, first.ObservedAt, cachedHarnessAvailability(false).ObservedAt)
	require.Equal(t, "codex one", version(cachedHarnessAvailability(true)))
	availabilityCache.Lock()
	availabilityCache.value.ObservedAt = time.Now().Add(-11 * time.Second)
	availabilityCache.Unlock()
	require.Equal(t, "codex two", version(cachedHarnessAvailability(true)))
}
