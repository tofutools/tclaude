package copilotfixture

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNormalizeLayoutPath pins every normalization rule the layout golden
// depends on, without needing the Copilot binary.
//
// This exists because the committed golden cannot pin two of them, and the
// reason is NOT that the run skips them — NewSandboxDirs hands every scenario a
// fresh cache, so the payload really is extracted and the lock really is taken.
// The golden simply cannot see either one:
//
//   - `inuse.<pid>.lock` sits at pkg/<platform>/<version>/inuse.<pid>.lock,
//     one level below where collapsePackagePayload stops the walk.
//   - the `.extracting-…` staging directory is RENAMED into place before the
//     run ends, so it no longer exists when the post-run walk happens.
//
// Both rules are therefore load-bearing but unexercised by the golden, and a
// regex that stopped matching would not produce a clear failure — it would
// produce a golden diff full of real pids and timestamps that reads like CLI
// drift.
func TestNormalizeLayoutPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "session uuid",
			in:   "session-state/3f9a1c7e-5b2d-4e81-9c3a-7d61f0a8b452/session.db",
			want: "session-state/<uuid>/session.db",
		},
		{
			name: "host platform tuple",
			in:   "pkg/linux-x64/1.0.77/app.js",
			want: "pkg/<platform>/1.0.77/app.js",
		},
		{
			name: "platform tuple as the final segment",
			in:   "pkg/darwin-arm64",
			want: "pkg/<platform>",
		},
		{
			name: "config write temp file",
			in:   "config.json.tmp.e013d536-a7e8-405d-915f-55d42ef096ce",
			want: "config.json.tmp.<uuid>",
		},
		{
			name: "per-launch lock",
			in:   "pkg/linux-x64/1.0.77/inuse.185.lock",
			want: "pkg/<platform>/1.0.77/inuse.<pid>.lock",
		},
		{
			name: "extraction staging directory",
			in:   "pkg/linux-x64/.extracting-1.0.77-185-1785783962755/app.js",
			want: "pkg/<platform>/.extracting-<version>-<pid>-<timestamp>/app.js",
		},
		{
			name: "stable path is left alone",
			in:   "session-store.db-wal",
			want: "session-store.db-wal",
		},
		{
			name: "a directory that merely looks platform-shaped is not rewritten",
			in:   "Microsoft/DeveloperTools/deviceid",
			want: "Microsoft/DeveloperTools/deviceid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, normalizeLayoutPath(tc.in))
		})
	}
}

// TestCollapsePackagePayload pins the depth at which the unpacked payload stops
// being walked: the grant is the cache root, and the payload is thousands of
// files that would make the golden meaningless.
func TestCollapsePackagePayload(t *testing.T) {
	got, stop := collapsePackagePayload("pkg/<platform>/1.0.77/prebuilds/linux-x64/runtime.node")
	assert.True(t, stop)
	assert.Equal(t, "pkg/<platform>/1.0.77", got)

	// Above the collapse depth the walk continues, so the intermediate
	// directories still appear in the golden.
	got, stop = collapsePackagePayload("pkg/<platform>")
	assert.False(t, stop)
	assert.Equal(t, "pkg/<platform>", got)

	// A non-payload tree is never collapsed.
	got, stop = collapsePackagePayload("session-state/<uuid>/checkpoints/index.md")
	assert.False(t, stop)
	assert.Equal(t, "session-state/<uuid>/checkpoints/index.md", got)
}
