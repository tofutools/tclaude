package hostmetrics

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestMemoryReaders(t *testing.T) {
	m, err := parseLinuxMemory("MemTotal: 1000 kB\nMemAvailable: 250 kB\nMemFree: 1 kB\nCached: 2 kB\n")
	require.NoError(t, err)
	require.Equal(t, uint64(1000*1024), m.TotalBytes)
	require.Equal(t, uint64(250*1024), m.AvailableBytes)
	require.False(t, m.AvailableEstimated)
	m, err = parseLinuxMemory("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 50 kB\nCached: 200 kB\nSReclaimable: 75 kB\nShmem: 25 kB\n")
	require.NoError(t, err)
	require.Equal(t, uint64(400*1024), m.AvailableBytes)
	require.True(t, m.AvailableEstimated)
	for _, raw := range []string{"MemTotal: 0 kB", "MemTotal: 100 kB", "MemTotal: -1 kB", "MemTotal: 18446744073709551615 kB", "MemTotal: 100 MB", "MemTotal: 100 kB\nMemAvailable: NaN kB"} {
		_, err = parseLinuxMemory(raw)
		require.Error(t, err, raw)
	}
	for _, size := range []string{"4096", "16384"} {
		raw := "Mach Virtual Memory Statistics: (page size of " + size + " bytes)\nPages free: 10.\nPages inactive: 20.\nPages speculative: 5.\nPages purgeable: 20.\nPages occupied by compressor: 100.\n"
		m, err = parseMacMemory(raw, 1<<30)
		require.NoError(t, err)
		require.True(t, m.AvailableEstimated)
		want := uint64(35 * 4096)
		if size == "16384" {
			want *= 4
		}
		require.Equal(t, want, m.AvailableBytes)
	}
	for _, raw := range []string{"bad header", "Mach Virtual Memory Statistics: (page size of 0 bytes)\nPages free: 1.", "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: 1.", "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: -1.\nPages inactive: 2."} {
		_, err = parseMacMemory(raw, 1<<30)
		require.Error(t, err, raw)
	}
}
func TestLoadAndWarnings(t *testing.T) {
	for _, raw := range []string{"1.25 2.50 3.75 1/200 1234", "{ 1.25 2.50 3.75 }\n"} {
		load, err := parseLoad(raw)
		require.NoError(t, err)
		require.Equal(t, [3]float64{1.25, 2.5, 3.75}, load)
	}
	for _, raw := range []string{"1 2", "NaN 1 2", "-1 2 3", "Inf 2 3"} {
		_, err := parseLoad(raw)
		require.Error(t, err)
	}
	load := [3]float64{4, 2, 1}
	s := Snapshot{CPU: CPU{LogicalCores: 2, LoadAverage: &load}, RAM: &Memory{TotalBytes: 100, AvailableBytes: 5}, Disks: []Disk{{Path: Path{Kind: "data", Path: "/data"}, TotalBytes: 100, AvailableBytes: 5}, {Path: Path{Path: "/missing"}, Error: "unavailable"}}}
	warnings := Warnings(s, Thresholds{LoadPerCore: 1.5, RAMAvailablePercent: 10, DiskAvailablePercent: 10})
	require.Len(t, warnings, 3)
	require.Equal(t, "high_load", warnings[0].Code)
	require.Equal(t, "low_ram", warnings[1].Code)
	require.Equal(t, "low_disk", warnings[2].Code)
	require.Empty(t, Warnings(s, Thresholds{}))
	require.Empty(t, Warnings(Snapshot{}, Thresholds{1.5, 10, 10}))
	_, err := multiply(math.MaxUint64, 2)
	require.Error(t, err)
}
func TestNativeSnapshotAndMissingWorkDirectory(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native metrics target Linux/macOS")
	}
	root := testutil.CanonicalTempDir(t)
	future := filepath.Join(root, "future", "work")
	paths := []Path{{Kind: "work", Path: future}, {Kind: "data", Path: root}, {Kind: "work", Path: future}}
	s := Read(paths)
	require.False(t, s.ObservedAt.IsZero())
	require.Positive(t, s.CPU.LogicalCores)
	require.NotNil(t, s.RAM, s.Errors)
	require.NotNil(t, s.CPU.LoadAverage, s.Errors)
	require.Len(t, s.Disks, 2)
	require.Equal(t, future, paths[0].Path, "reader does not reorder caller's slice")
	for _, d := range s.Disks {
		require.Empty(t, d.Error)
		require.Equal(t, root, d.MeasuredPath)
		require.Positive(t, d.TotalBytes)
		require.LessOrEqual(t, d.AvailableBytes, d.TotalBytes)
	}
	_, err := os.Stat(future)
	require.True(t, os.IsNotExist(err), "sampler must not create directories")
	encoded, err := json.Marshal(s)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(encoded), "NaN"))
}
