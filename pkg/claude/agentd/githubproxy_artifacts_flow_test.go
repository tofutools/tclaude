package agentd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// githubproxy_artifacts_flow_test.go covers `run artifacts` and `run download`.
//
// Download is unlike every other verb in this proxy: its effect is a
// FILESYSTEM WRITE performed by an unsandboxed daemon, not a response. So the
// assertions here concentrate on the two things that would make that
// dangerous — a destination the caller can influence, and a download the
// operator's disk cannot afford — plus the preflight that makes both decidable
// before a byte is fetched.

// ghArtifactManifestJSON renders the projection the daemon asks gh for.
func ghArtifactManifestJSON(entries ...string) string {
	return "[" + strings.Join(entries, ",") + "]"
}

func ghArtifact(name string, size int64, expired bool) string {
	b, _ := json.Marshal(map[string]any{
		"id": 1, "name": name, "size_in_bytes": size, "expired": expired,
		"created_at": "2026-08-01T00:00:00Z", "expires_at": "2026-09-01T00:00:00Z",
	})
	return string(b)
}

// ghArgvs returns the argv of every gh invocation the recorder saw, in order.
func ghArgvs(t *testing.T, rec *gitProxyRecorder) []agentd.ProxyCommand {
	t.Helper()
	cmds, _ := ghCalls(t, rec)
	return cmds
}

// downloadingWorld is a world whose gh stub behaves like the real one for this
// verb: the manifest call answers with `manifest`, and the download call
// actually CREATES the files it claims to, under the --dir it was given.
//
// The files matter. `gh run download` prints nothing on success, so everything
// the caller learns comes from the daemon walking the destination afterwards —
// a stub that writes nothing would let a broken listing pass.
func downloadingWorld(t *testing.T, manifest string, files map[string]string) (
	*testharness.Flow, *gitProxyRecorder,
) {
	t.Helper()
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	inner := rec.exec
	t.Cleanup(agentd.SetProxyExecForTest(func(ctx context.Context, cmd agentd.ProxyCommand) (agentd.ProxyResult, error) {
		res, err := inner(ctx, cmd)
		if cmd.Tool != "gh" || len(cmd.Args) < 2 {
			return res, err
		}
		switch {
		case cmd.Args[0] == "api":
			return agentd.ProxyResult{Stdout: manifest}, nil
		case cmd.Args[0] == "run" && cmd.Args[1] == "download":
			dir := cmd.Args[slices.Index(cmd.Args, "--dir")+1]
			for rel, content := range files {
				p := filepath.Join(dir, filepath.FromSlash(rel))
				require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
				require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
			}
			return agentd.ProxyResult{}, nil
		}
		return res, err
	}))
	return f, rec
}

func ghOutcomeStdout(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		Stdout string `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.Stdout
}

// TestGHProxy_DownloadDestinationIsComputedNotNamed is the invariant that keeps
// "the proxy lends credentials, not filesystem reach" true of the one verb that
// writes files.
//
// agentd runs unsandboxed. A caller who could name the destination could have
// it unzip an archive over the operator's ~/.ssh, or anywhere else the daemon
// can reach — an escalation no permission slug in this proxy is meant to
// confer. So there is no destination parameter, and anything that looks like
// one in the request is ignored rather than sanitized.
func TestGHProxy_DownloadDestinationIsComputedNotNamed(t *testing.T) {
	f, rec := downloadingWorld(t,
		ghArtifactManifestJSON(ghArtifact("coverage", 2048, false)),
		map[string]string{"coverage.out": "mode: set\n"})

	res := gitProxyPost(t, f, "/v1/github/run/download", map[string]any{
		"run_id": 18234567890,
		"dir":    "/etc",
		"path":   "../../../root/.ssh",
		"output": "/home/victim/.ssh/authorized_keys",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := ghArgvs(t, rec)
	require.Len(t, calls, 2, "the manifest preflight, then the download")
	dl := calls[1]
	i := slices.Index(dl.Args, "--dir")
	require.GreaterOrEqual(t, i, 0, "gh must always be told where to write")
	want := filepath.Join(rec.repoRoot, ".tclaude-artifacts", "run-18234567890")
	assert.Equal(t, want, dl.Args[i+1],
		"the destination comes from the agent's own work tree, never from the request")
	assert.NotContains(t, strings.Join(dl.Args, " "), "/etc")
	assert.NotContains(t, strings.Join(dl.Args, " "), "authorized_keys")

	// gh still RUNS in the neutral directory. Writing into the agent's
	// repository is not a reason to let .git/config back into scope.
	assert.NotEqual(t, rec.repoRoot, dl.Dir)
	assert.Equal(t, os.TempDir(), dl.Dir)
}

// TestGHProxy_DownloadReportsWhereItLandedAndWhat — `gh run download` prints
// nothing at all on success, which for a verb whose entire effect is on disk
// tells the caller neither where to look nor whether anything arrived.
func TestGHProxy_DownloadReportsWhereItLandedAndWhat(t *testing.T) {
	f, rec := downloadingWorld(t,
		ghArtifactManifestJSON(ghArtifact("test-results", 4096, false)),
		map[string]string{
			"junit/report.xml": strings.Repeat("x", 2048),
			"stdout.log":       "boom\n",
		})

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	stdout := ghOutcomeStdout(t, res.Body.Bytes())
	dest := filepath.Join(rec.repoRoot, ".tclaude-artifacts", "run-18234567890")
	assert.Contains(t, stdout, dest, "the caller has to be told where the files are")
	assert.Contains(t, stdout, "junit/report.xml")
	assert.Contains(t, stdout, "stdout.log")
	assert.Contains(t, stdout, "2 files")

	// And the files are genuinely there, at the path that was reported.
	_, err := os.Stat(filepath.Join(dest, "junit", "report.xml"))
	assert.NoError(t, err)
}

// TestGHProxy_DownloadRefusesMoreThanTheDiskBudget — gh has no size limit of
// its own to ask for, so without the manifest preflight the only bound on what
// lands in the operator's disk is whatever CI happened to upload. The refusal
// must also come BEFORE the download, or it has cost the very thing it exists
// to protect.
func TestGHProxy_DownloadRefusesMoreThanTheDiskBudget(t *testing.T) {
	f, rec := downloadingWorld(t, ghArtifactManifestJSON(
		ghArtifact("build-tree", 700<<20, false),
	), nil)

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusRequestEntityTooLarge, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "700.0 MiB")

	assert.Len(t, ghArgvs(t, rec), 1, "the manifest read only — nothing was fetched")
	assert.NoDirExists(t, filepath.Join(rec.repoRoot, ".tclaude-artifacts", "run-18234567890"),
		"a refused download must not leave a destination behind either")
}

// TestGHProxy_DownloadNarrowsTheBudgetToTheNamedArtifact — naming one artifact
// is how a caller gets past a run whose artifacts do not fit together, so the
// budget has to follow the name rather than the run.
func TestGHProxy_DownloadNarrowsTheBudgetToTheNamedArtifact(t *testing.T) {
	f, rec := downloadingWorld(t, ghArtifactManifestJSON(
		ghArtifact("build-tree", 700<<20, false),
		ghArtifact("coverage", 4096, false),
	), map[string]string{"coverage.out": "mode: set\n"})

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "coverage"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	calls := ghArgvs(t, rec)
	require.Len(t, calls, 2)
	i := slices.Index(calls[1].Args, "--name")
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, "coverage", calls[1].Args[i+1])
}

// TestGHProxy_DownloadExplainsAMissingArtifact — gh reports "no artifact
// matches" for a typo, an expired artifact and a run that uploaded nothing
// alike. The preflight has the manifest in hand, so it can say which.
func TestGHProxy_DownloadExplainsAMissingArtifact(t *testing.T) {
	t.Run("a name that is not there lists the ones that are", func(t *testing.T) {
		f, rec := downloadingWorld(t, ghArtifactManifestJSON(
			ghArtifact("coverage", 4096, false),
			ghArtifact("test-results", 4096, false),
		), nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890, "name": "covrage"})
		require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "coverage")
		assert.Contains(t, res.Body.String(), "test-results")
		assert.Len(t, ghArgvs(t, rec), 1)
	})

	t.Run("an expired artifact is not a live one", func(t *testing.T) {
		f, rec := downloadingWorld(t,
			ghArtifactManifestJSON(ghArtifact("coverage", 4096, true)), nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890, "name": "coverage"})
		require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
		assert.Len(t, ghArgvs(t, rec), 1)
	})

	t.Run("a run that uploaded nothing says so", func(t *testing.T) {
		f, rec := downloadingWorld(t, "[]", nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890})
		require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "no live artifacts")
		assert.Len(t, ghArgvs(t, rec), 1)
	})
}

// TestGHProxy_DownloadRefusesAnArtifactNameThatIsAnArgument — the name reaches
// argv, and a path separator in it is an attempt to steer where gh writes.
func TestGHProxy_DownloadRefusesAnArtifactNameThatIsAnArgument(t *testing.T) {
	for _, name := range []string{"--dir=/etc", "-n", "../../etc/passwd", "a\nb"} {
		t.Run(name, func(t *testing.T) {
			f, rec := downloadingWorld(t,
				ghArtifactManifestJSON(ghArtifact("coverage", 4096, false)), nil)

			res := gitProxyPost(t, f, "/v1/github/run/download",
				map[string]any{"run_id": 18234567890, "name": name})
			assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			assert.Empty(t, ghArgvs(t, rec), "a refused name must not even reach the preflight")
		})
	}
}

// TestGHProxy_DownloadStartsFromAnEmptyDirectory — left in place, an earlier
// download's files come back in this one's listing, and a caller reading that
// listing has no way to tell which run a file came from.
func TestGHProxy_DownloadStartsFromAnEmptyDirectory(t *testing.T) {
	f, rec := downloadingWorld(t,
		ghArtifactManifestJSON(ghArtifact("coverage", 4096, false)),
		map[string]string{"coverage.out": "mode: set\n"})

	dest := filepath.Join(rec.repoRoot, ".tclaude-artifacts", "run-18234567890")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	stale := filepath.Join(dest, "from-an-older-run.txt")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o644))

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	stdout := ghOutcomeStdout(t, res.Body.Bytes())
	assert.NotContains(t, stdout, "from-an-older-run.txt")
	assert.NoFileExists(t, stale)
	assert.Contains(t, stdout, "coverage.out")
}

// TestGHProxy_DownloadDirectoryIgnoresItself — an agent that downloads an
// artifact mid-branch should not then commit it by reflex, and the operator
// should not have to edit .gitignore to prevent that. A directory whose
// contents are all ignored does not appear as untracked at all.
func TestGHProxy_DownloadDirectoryIgnoresItself(t *testing.T) {
	f, rec := downloadingWorld(t,
		ghArtifactManifestJSON(ghArtifact("coverage", 4096, false)),
		map[string]string{"coverage.out": "mode: set\n"})

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	ignore, err := os.ReadFile(filepath.Join(rec.repoRoot, ".tclaude-artifacts", ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, "*\n", string(ignore))
}

// TestGHProxy_DownloadRefusesToFollowASymlinkOutOfTheWorkTree — the agent can
// write its own work tree, so it can replace the artifacts directory with a
// link to somewhere it cannot write. The daemon can, which is the whole point
// of the check: every step of the destination is taken through an os.Root that
// refuses a traversal leaving the tree.
func TestGHProxy_DownloadRefusesToFollowASymlinkOutOfTheWorkTree(t *testing.T) {
	f, rec := downloadingWorld(t,
		ghArtifactManifestJSON(ghArtifact("coverage", 4096, false)), nil)

	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside,
		filepath.Join(rec.repoRoot, ".tclaude-artifacts")))

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())

	assert.Len(t, ghArgvs(t, rec), 1, "only the preflight ran; nothing was downloaded")
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing may be written through the link")
}

// TestGHProxy_ArtifactsListIsARead — listing what a run produced spends the
// operator's credential and returns nothing that could be written with it, so
// it sits behind github.read beside the other reads.
func TestGHProxy_ArtifactsListIsARead(t *testing.T) {
	t.Run("ungranted", func(t *testing.T) {
		f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
		res := gitProxyPost(t, f, "/v1/github/run/artifacts",
			map[string]any{"run_id": 18234567890})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.False(t, rec.sawAnySubprocess())
	})

	t.Run("granted", func(t *testing.T) {
		f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
		rec.gh = agentd.ProxyResult{
			Stdout: ghArtifactManifestJSON(ghArtifact("coverage", 4096, false)),
		}
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

		res := gitProxyPost(t, f, "/v1/github/run/artifacts",
			map[string]any{"run_id": 18234567890})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

		call := ghCall(t, rec)
		assert.Equal(t, "api", call.Args[0])
		assert.Equal(t, "repos/tofutools/tclaude/actions/runs/18234567890/artifacts?per_page=100",
			call.Args[1], "the slug is derived; gh's {owner}/{repo} placeholders would come "+
				"from the agent-writable .git/config instead")
		assert.NotContains(t, strings.Join(call.Args, " "), "{owner}")
		// -f/-F would flip gh to POST — writing with the operator's credential
		// from a read verb.
		assert.NotContains(t, call.Args, "-f")
		assert.NotContains(t, call.Args, "-F")

		var out struct {
			JSON json.RawMessage `json:"json"`
		}
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
		assert.Contains(t, string(out.JSON), "coverage",
			"a JSON read must ride in the JSON field, not be flattened into text")
	})
}

// TestGHProxy_DownloadRequiresTheReadSlug — it writes to the agent's own disk,
// but what it spends is a GitHub read.
func TestGHProxy_DownloadRequiresTheReadSlug(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.False(t, rec.sawAnySubprocess(), "a denied caller runs nothing at all")
	assert.NoDirExists(t, filepath.Join(rec.repoRoot, ".tclaude-artifacts"),
		"and creates no directory either")
}
