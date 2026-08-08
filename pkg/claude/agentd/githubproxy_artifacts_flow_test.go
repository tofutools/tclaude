package agentd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// ghArtifactManifestJSON renders the projection the daemon asks gh for: the
// run's real artifact count, plus the page it got back.
func ghArtifactManifestJSON(entries ...string) string {
	return ghArtifactPage(len(entries), entries...)
}

// ghArtifactPage renders a manifest whose `total` may exceed the entries — the
// shape a run with more artifacts than one page holds produces.
func ghArtifactPage(total int, entries ...string) string {
	return fmt.Sprintf(`{"total":%d,"artifacts":[%s]}`, total, strings.Join(entries, ","))
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

	// "expired" and "no such name" are DIFFERENT answers, and conflating them
	// produces a self-contradicting message — "no live artifact named
	// "coverage"; it has: coverage" — that an agent will retry on.
	t.Run("an expired artifact is named as expired, not as missing", func(t *testing.T) {
		f, rec := downloadingWorld(t,
			ghArtifactManifestJSON(ghArtifact("coverage", 4096, true)), nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890, "name": "coverage"})
		require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "expired")
		assert.Contains(t, res.Body.String(), "retrying will not",
			"an agent told only 'not found' will retry a retention lapse forever")
		assert.NotContains(t, res.Body.String(), "it has: coverage",
			"offering the expired artifact as an alternative to itself is nonsense")
		assert.Len(t, ghArgvs(t, rec), 1)
	})

	// Without a --name every artifact matches, so an all-expired run reaches
	// the expired branch with no name to put in the message. It must read as a
	// statement about the run, not about an artifact nobody asked for.
	t.Run("a run whose every artifact expired says that, without naming one", func(t *testing.T) {
		f, rec := downloadingWorld(t, ghArtifactManifestJSON(
			ghArtifact("coverage", 4096, true),
			ghArtifact("test-results", 4096, true),
		), nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890})
		require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "expired")
		assert.Contains(t, res.Body.String(), "all 2 of this run's artifacts")
		assert.NotContains(t, res.Body.String(), `the artifact "" `,
			"an empty name in the message means the named branch was reached without a name")
		assert.Len(t, ghArgvs(t, rec), 1)
	})

	// retention-days is per upload step, so "every artifact on page 1 expired"
	// is not "every artifact in the run expired". Claiming the run — and that
	// retrying cannot help — would stop an agent that could still name a live
	// artifact from the pages this read never saw.
	t.Run("an all-expired first page does not speak for the whole run", func(t *testing.T) {
		f, rec := downloadingWorld(t, ghArtifactPage(400,
			ghArtifact("shard-00", 4096, true),
			ghArtifact("shard-01", 4096, true),
		), nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890})
		require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
		body := res.Body.String()
		assert.Contains(t, body, "400", "the run's real size has to appear")
		assert.Contains(t, body, "may still be live")
		assert.NotContains(t, body, "all 2 of this run's artifacts",
			"2 is what was inspected, not what the run holds")
		assert.NotContains(t, body, "retrying will not bring them back",
			"that verdict is only true when the whole run was seen")
		assert.Len(t, ghArgvs(t, rec), 1)
	})

	t.Run("a run that uploaded nothing says so", func(t *testing.T) {
		f, rec := downloadingWorld(t, ghArtifactManifestJSON(), nil)

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

// TestGHProxy_DownloadRefusesAnAllOfThemDownloadItCannotSize closes the gap
// between what the preflight MEASURES and what gh then FETCHES.
//
// The manifest is one page. `gh run download` without --name fetches every
// artifact in the RUN, which on a busy run is not the same set — so sizing the
// download against the page would let several times the cap through while
// reporting a number under it. Naming an artifact removes the gap, because then
// gh fetches exactly what was measured.
func TestGHProxy_DownloadRefusesAnAllOfThemDownloadItCannotSize(t *testing.T) {
	page := make([]string, 0, 100)
	for i := range 100 {
		page = append(page, ghArtifact(fmt.Sprintf("shard-%02d", i), 4<<20, false))
	}

	t.Run("every artifact, on a run with more than a page of them", func(t *testing.T) {
		f, rec := downloadingWorld(t, ghArtifactPage(400, page...), nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890})
		require.Equal(t, http.StatusRequestEntityTooLarge, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "400 artifacts")
		assert.Len(t, ghArgvs(t, rec), 1, "nothing may be fetched")
	})

	t.Run("one named artifact is measured exactly and allowed", func(t *testing.T) {
		f, rec := downloadingWorld(t, ghArtifactPage(400, page...),
			map[string]string{"shard.txt": "ok\n"})

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890, "name": "shard-07"})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		assert.Len(t, ghArgvs(t, rec), 2)
	})
}

// TestGHProxy_ManifestIsReadWithABulkBound is the regression for a download
// that is impossible on exactly the runs it is most wanted for.
//
// The default output bound is 16 KiB, sized for a diagnosis, and it keeps the
// TAIL. A full page of this projection passes 16 KiB once artifact names reach
// ordinary matrix lengths, and a tail of a JSON document begins mid-object — so
// under the default bound a busy run's manifest would fail to parse and no
// --name could rescue it.
func TestGHProxy_ManifestIsReadWithABulkBound(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{Stdout: ghArtifactManifestJSON(ghArtifact("coverage", 4096, false))}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/artifacts",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	call := ghCall(t, rec)
	assert.Greater(t, call.MaxOutputBytes, 16*1024,
		"a page of artifacts does not fit the diagnosis-sized default, and the tail of a "+
			"truncated JSON document does not parse")
}

// TestGHProxy_DownloadRefusesATruncatedManifest — a manifest read that lost
// entries cannot size a download, and a truncated JSON document does not always
// fail to parse. Refuse rather than measure a fraction of the bytes.
func TestGHProxy_DownloadRefusesATruncatedManifest(t *testing.T) {
	f, rec := gitProxyWorld(t, []string{"github.com/tofutools"})
	rec.gh = agentd.ProxyResult{
		Stdout:    ghArtifactManifestJSON(ghArtifact("coverage", 4096, false)),
		Truncated: true,
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusBadGateway, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 1, ghCallCount(rec), "nothing may be fetched on an unsizeable manifest")
}

// TestGHProxy_DownloadSharesOneBudgetAcrossBothCalls — the CLI waits on a
// single number, so the daemon's worst case has to be that number however the
// work divides between the manifest read and the download.
func TestGHProxy_DownloadSharesOneBudgetAcrossBothCalls(t *testing.T) {
	f, rec := downloadingWorld(t,
		ghArtifactManifestJSON(ghArtifact("coverage", 4096, false)),
		map[string]string{"coverage.out": "mode: set\n"})

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	_, budgets := ghCalls(t, rec)
	require.Len(t, budgets, 2)
	// The manifest read gets the ordinary per-call bound, and the download the
	// long one — but both sit under the shared 300s ceiling, so the two
	// together cannot outlast what the CLI is waiting on.
	assert.LessOrEqual(t, budgets[0], 300*time.Second)
	assert.Greater(t, budgets[1], 210*time.Second,
		"the download itself must get the long bound, not the ordinary one")
	assert.LessOrEqual(t, budgets[1], 300*time.Second)
}

// TestGHProxy_ListingIsBoundedInBytesNotJustEntries — the paths inside an
// artifact are chosen by whoever configured the CI job, which on a public
// repository is anyone who can open a pull request. A count bound alone does
// not bound the listing, and this response can be persisted for the
// idempotency TTL.
func TestGHProxy_ListingIsBoundedInBytesNotJustEntries(t *testing.T) {
	files := map[string]string{}
	deep := strings.Repeat("a-long-directory-component/", 30)
	for i := range 150 {
		files[fmt.Sprintf("%s/file-%03d.txt", deep, i)] = "x"
	}
	f, _ := downloadingWorld(t,
		ghArtifactManifestJSON(ghArtifact("logs", 4096, false)), files)

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	stdout := ghOutcomeStdout(t, res.Body.Bytes())
	assert.Less(t, len(stdout), 96*1024, "the listing must be bounded in bytes")
	// The footer itself, not a bare "and" that any file path would satisfy: a
	// bounded listing that does not SAY it is bounded reads as the whole tree.
	assert.Regexp(t, `… and \d+ more`, stdout, "and it must say that it stopped short")
	assert.Contains(t, stdout, "150 files", "while still reporting the true total")
}

// TestGHProxy_DownloadRefusesADestinationThatIsNotADirectory — the agent owns
// its work tree, so `.tclaude-artifacts` may be anything it likes by the time
// the daemon looks.
func TestGHProxy_DownloadRefusesADestinationThatIsNotADirectory(t *testing.T) {
	t.Run("a regular file where the directory belongs", func(t *testing.T) {
		f, rec := downloadingWorld(t,
			ghArtifactManifestJSON(ghArtifact("coverage", 4096, false)), nil)
		require.NoError(t, os.WriteFile(
			filepath.Join(rec.repoRoot, ".tclaude-artifacts"), []byte("not a dir"), 0o644))

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890})
		require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
		assert.Len(t, ghArgvs(t, rec), 1)
	})

	// The per-run directory is the one the daemon RemoveAlls before each
	// download, so a symlink there aims a deletion rather than a write. It does
	// not have to be refused — unlinking a symlink is not following it — but it
	// must not reach through.
	t.Run("the per-run directory symlinked out of the tree", func(t *testing.T) {
		f, rec := downloadingWorld(t,
			ghArtifactManifestJSON(ghArtifact("coverage", 4096, false)),
			map[string]string{"coverage.out": "mode: set\n"})
		base := filepath.Join(rec.repoRoot, ".tclaude-artifacts")
		require.NoError(t, os.MkdirAll(base, 0o755))
		outside := t.TempDir()
		bystander := filepath.Join(outside, "someone-elses-file")
		require.NoError(t, os.WriteFile(bystander, []byte("keep me"), 0o644))
		require.NoError(t, os.Symlink(outside, filepath.Join(base, "run-18234567890")))

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

		// The link was replaced by a real directory inside the work tree...
		dest := filepath.Join(base, "run-18234567890")
		fi, err := os.Lstat(dest)
		require.NoError(t, err)
		assert.True(t, fi.IsDir())
		assert.Zero(t, fi.Mode()&os.ModeSymlink, "the symlink must not survive as the destination")
		assert.FileExists(t, filepath.Join(dest, "coverage.out"))

		// ...and the directory it pointed at was neither emptied nor written to.
		assert.FileExists(t, bystander, "the clear-out must not reach through the link")
		entries, err := os.ReadDir(outside)
		require.NoError(t, err)
		assert.Len(t, entries, 1, "and nothing was downloaded through it either")
	})
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
		// The projection is what keeps this read small: each raw entry embeds a
		// complete copy of the workflow-run object, and a page of those does
		// not fit any bound this proxy would accept.
		i := slices.Index(call.Args, "--jq")
		require.GreaterOrEqual(t, i, 0, "the manifest must be projected, not passed through raw")
		assert.Contains(t, call.Args[i+1], "total_count",
			"total_count is how a partial page is detected at all")
		assert.Contains(t, call.Args[i+1], "size_in_bytes")
		assert.NotContains(t, call.Args[i+1], "workflow_run")
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
