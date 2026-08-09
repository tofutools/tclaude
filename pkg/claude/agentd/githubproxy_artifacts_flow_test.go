package agentd_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
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
//
// The daemon now unpacks the archive itself rather than handing the job to a
// child process, which adds a third: an archive is untrusted input, and a zip
// entry naming `../../.ssh/authorized_keys` is a real thing a fork's pull
// request can upload.

// zipOf builds an archive from a path → content map. Used for artifacts and for
// the run-log archive alike.
func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	// Sorted, so a fixture with several entries produces the same bytes every
	// run and a failure is reproducible.
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte(files[name]))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// ghArtifactsWireJSON renders the endpoint's own shape: the run's real artifact
// count, plus the page it got back.
func ghArtifactsWireJSON(entries ...string) string {
	return ghArtifactsWirePage(len(entries), entries...)
}

// ghArtifactsWirePage renders a manifest whose total_count may exceed the
// entries — the shape a run with more artifacts than one page holds produces.
func ghArtifactsWirePage(total int, entries ...string) string {
	return fmt.Sprintf(`{"total_count":%d,"artifacts":[%s]}`, total, strings.Join(entries, ","))
}

var ghArtifactID int64

func ghArtifact(name string, size int64, expired bool) string {
	ghArtifactID++
	b, _ := json.Marshal(map[string]any{
		"id": ghArtifactID, "name": name, "size_in_bytes": size, "expired": expired,
		"created_at": "2026-08-01T00:00:00Z", "expires_at": "2026-09-01T00:00:00Z",
		// The bulk each raw entry carries, and the reason this read is
		// projected rather than passed through.
		"workflow_run": map[string]any{"id": 18234567890, "head_branch": "feat/thing"},
	})
	return string(b)
}

// downloadingWorld scripts the manifest read and answers every artifact-zip
// transfer with an archive built from `files`.
//
// The files matter: everything the caller learns about a download comes from
// the daemon walking the destination afterwards, so a stub that transferred
// nothing would let a broken listing pass.
func downloadingWorld(t *testing.T, manifest string, files map[string]string) (
	*testharness.Flow, *gitProxyRecorder, *ghRecorder,
) {
	t.Helper()
	f, git, gh := ghWorld(t, []string{"github.com/tofutools"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitHubRead, "test"))

	gh.route = func(req agentd.GitHubRequestForTest) (ghCanned, bool) {
		if strings.HasSuffix(req.Path, "/artifacts") {
			return ghCanned{Status: 200, Body: manifest}, true
		}
		return ghCanned{}, false
	}
	if files != nil {
		gh.zipAny = zipOf(t, files)
	}
	return f, git, gh
}

func ghOutcomeStdout(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		Stdout string `json:"stdout"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.Stdout
}

func artifactDest(repoRoot string, runID int64) string {
	return filepath.Join(repoRoot, ".tclaude-artifacts", fmt.Sprintf("run-%d", runID))
}

// TestGHProxy_DownloadDestinationIsComputedNotNamed is the invariant that keeps
// "the proxy lends credentials, not filesystem reach" true of the one verb that
// writes files.
//
// agentd runs unsandboxed. A caller who could name the destination could have
// it unpack an archive over the operator's ~/.ssh, or anywhere else the daemon
// can reach — an escalation no permission slug in this proxy is meant to
// confer. So there is no destination parameter, and anything that looks like
// one in the request is ignored rather than sanitized.
func TestGHProxy_DownloadDestinationIsComputedNotNamed(t *testing.T) {
	f, git, gh := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("coverage", 2048, false)),
		map[string]string{"coverage.out": "mode: set\n"})

	res := gitProxyPost(t, f, "/v1/github/run/download", map[string]any{
		"run_id": 18234567890,
		"dir":    "/etc",
		"path":   "../../../root/.ssh",
		"output": "/home/victim/.ssh/authorized_keys",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	// The files landed in the computed destination and nowhere else. There is
	// no argv to inspect any more — the destination is not something the daemon
	// tells anyone, it is where it writes — so this asserts the disk.
	dest := artifactDest(git.repoRoot, 18234567890)
	assert.FileExists(t, filepath.Join(dest, "coverage", "coverage.out"))
	assert.Contains(t, ghOutcomeStdout(t, res.Body.Bytes()), dest)
	for _, req := range gh.requests() {
		assert.NotContains(t, req.Path, "etc")
		assert.NotContains(t, req.Path, "authorized_keys")
	}
}

// TestGHProxy_DownloadRefusesAnArchiveThatEscapesTheDestination — a zip entry
// name is attacker-controlled input on any public repository, and `..` in one
// is the oldest trick there is. The daemon unpacks as the operator, so the
// destination has to be a hard boundary rather than a convention.
func TestGHProxy_DownloadRefusesAnArchiveThatEscapesTheDestination(t *testing.T) {
	f, git, _ := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("innocent", 2048, false)),
		map[string]string{
			"../../../../../../tmp/tclaude-zipslip-escapee": "pwned\n",
			"legit.txt": "fine\n",
		})

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	assert.NoFileExists(t, "/tmp/tclaude-zipslip-escapee",
		"a traversing entry must never be written outside the destination")
	// The traversal is neutralized rather than fatal: the entry lands inside
	// the destination, which is the same answer an ordinary unzip tool gives.
	// Without a --name each artifact lands in its own subdirectory, so the
	// entries are under the artifact's name as well as inside the destination.
	dest := filepath.Join(artifactDest(git.repoRoot, 18234567890), "innocent")
	assert.FileExists(t, filepath.Join(dest, "legit.txt"))
	assert.FileExists(t, filepath.Join(dest, "tmp", "tclaude-zipslip-escapee"))
}

// TestGHProxy_DownloadReportsWhereItLandedAndWhat — a verb whose entire effect
// is on disk has to say where to look and whether anything arrived.
func TestGHProxy_DownloadReportsWhereItLandedAndWhat(t *testing.T) {
	f, git, _ := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("test-results", 4096, false)),
		map[string]string{
			"junit/report.xml": strings.Repeat("x", 2048),
			"stdout.log":       "boom\n",
		})

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "test-results"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	stdout := ghOutcomeStdout(t, res.Body.Bytes())
	dest := artifactDest(git.repoRoot, 18234567890)
	assert.Contains(t, stdout, dest, "the caller has to be told where the files are")
	assert.Contains(t, stdout, "junit/report.xml")
	assert.Contains(t, stdout, "stdout.log")
	assert.Contains(t, stdout, "2 files")

	// And the files are genuinely there, at the path that was reported.
	assert.FileExists(t, filepath.Join(dest, "junit", "report.xml"))
}

// TestGHProxy_DownloadWithoutANameSeparatesTheArtifacts — every artifact in the
// run is fetched, and two of them holding a file of the same name must not
// overwrite one another.
func TestGHProxy_DownloadWithoutANameSeparatesTheArtifacts(t *testing.T) {
	f, git, _ := downloadingWorld(t, ghArtifactsWireJSON(
		ghArtifact("coverage", 2048, false),
		ghArtifact("test-results", 2048, false),
	), map[string]string{"report.txt": "same name in both\n"})

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	dest := artifactDest(git.repoRoot, 18234567890)
	assert.FileExists(t, filepath.Join(dest, "coverage", "report.txt"))
	assert.FileExists(t, filepath.Join(dest, "test-results", "report.txt"))
}

// TestGHProxy_DownloadRefusesMoreThanTheDiskBudget — GitHub offers no size
// limit to ask for, so without the manifest preflight the only bound on what
// lands on the operator's disk is whatever CI happened to upload. The refusal
// must also come BEFORE the transfer, or it has cost the very thing it exists
// to protect.
func TestGHProxy_DownloadRefusesMoreThanTheDiskBudget(t *testing.T) {
	f, git, gh := downloadingWorld(t, ghArtifactsWireJSON(
		ghArtifact("build-tree", 700<<20, false),
	), nil)

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusRequestEntityTooLarge, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "700.0 MiB")

	assert.Equal(t, 1, gh.count(), "the manifest read only")
	assert.Empty(t, gh.streamed(), "nothing was fetched")
	assert.NoDirExists(t, artifactDest(git.repoRoot, 18234567890),
		"a refused download must not leave a destination behind either")
}

// TestGHProxy_DownloadNarrowsTheBudgetToTheNamedArtifact — naming one artifact
// is how a caller gets past a run whose artifacts do not fit together, so the
// budget has to follow the name rather than the run.
func TestGHProxy_DownloadNarrowsTheBudgetToTheNamedArtifact(t *testing.T) {
	f, _, gh := downloadingWorld(t, ghArtifactsWireJSON(
		ghArtifact("build-tree", 700<<20, false),
		ghArtifact("coverage", 4096, false),
	), map[string]string{"coverage.out": "mode: set\n"})

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "coverage"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	streamed := gh.streamed()
	require.Len(t, streamed, 1, "only the named artifact is fetched")
	assert.Contains(t, streamed[0].Path, "/actions/artifacts/")
}

// TestGHProxy_DownloadExplainsAMissingArtifact — a typo, an expired artifact
// and a run that uploaded nothing all look alike from the outside. The
// preflight has the manifest in hand, so it can say which.
func TestGHProxy_DownloadExplainsAMissingArtifact(t *testing.T) {
	t.Run("a name that is not there lists the ones that are", func(t *testing.T) {
		f, _, gh := downloadingWorld(t, ghArtifactsWireJSON(
			ghArtifact("coverage", 4096, false),
			ghArtifact("test-results", 4096, false),
		), nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890, "name": "covrage"})
		require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "coverage")
		assert.Contains(t, res.Body.String(), "test-results")
		assert.Empty(t, gh.streamed())
	})

	// "expired" and "no such name" are DIFFERENT answers, and conflating them
	// produces a self-contradicting message — "no live artifact named
	// "coverage"; it has: coverage" — that an agent will retry on.
	t.Run("an expired artifact is named as expired, not as missing", func(t *testing.T) {
		f, _, gh := downloadingWorld(t,
			ghArtifactsWireJSON(ghArtifact("coverage", 4096, true)), nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890, "name": "coverage"})
		require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "expired")
		assert.Contains(t, res.Body.String(), "retrying will not",
			"an agent told only 'not found' will retry a retention lapse forever")
		assert.NotContains(t, res.Body.String(), "it has: coverage",
			"offering the expired artifact as an alternative to itself is nonsense")
		assert.Empty(t, gh.streamed())
	})

	// Without a --name every artifact matches, so an all-expired run reaches
	// the expired branch with no name to put in the message. It must read as a
	// statement about the run, not about an artifact nobody asked for.
	t.Run("a run whose every artifact expired says that, without naming one", func(t *testing.T) {
		f, _, gh := downloadingWorld(t, ghArtifactsWireJSON(
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
		assert.Empty(t, gh.streamed())
	})

	// retention-days is per upload step, so "every artifact on page 1 expired"
	// is not "every artifact in the run expired". Claiming the run — and that
	// retrying cannot help — would stop an agent that could still name a live
	// artifact from the pages this read never saw.
	t.Run("an all-expired first page does not speak for the whole run", func(t *testing.T) {
		f, _, gh := downloadingWorld(t, ghArtifactsWirePage(400,
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
		assert.Empty(t, gh.streamed())
	})

	t.Run("a run that uploaded nothing says so", func(t *testing.T) {
		f, _, gh := downloadingWorld(t, ghArtifactsWireJSON(), nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890})
		require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "no live artifacts")
		assert.Empty(t, gh.streamed())
	})
}

// TestGHProxy_DownloadRefusesAnArtifactNameThatIsNotOne — GitHub itself refuses
// these characters in an artifact name, so nothing legal is lost by refusing
// them here, and a path separator in one is an attempt to steer where the
// daemon writes.
func TestGHProxy_DownloadRefusesAnArtifactNameThatIsNotOne(t *testing.T) {
	for _, name := range []string{"--dir=/etc", "-n", "../../etc/passwd", "a\nb"} {
		t.Run(name, func(t *testing.T) {
			f, _, gh := downloadingWorld(t,
				ghArtifactsWireJSON(ghArtifact("coverage", 4096, false)), nil)

			res := gitProxyPost(t, f, "/v1/github/run/download",
				map[string]any{"run_id": 18234567890, "name": name})
			assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, 0, gh.count(), "a refused name must not even reach the preflight")
		})
	}
}

// TestGHProxy_DownloadStartsFromAnEmptyDirectory — left in place, an earlier
// download's files come back in this one's listing, and a caller reading that
// listing has no way to tell which run a file came from.
func TestGHProxy_DownloadStartsFromAnEmptyDirectory(t *testing.T) {
	f, git, _ := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("coverage", 4096, false)),
		map[string]string{"coverage.out": "mode: set\n"})

	dest := artifactDest(git.repoRoot, 18234567890)
	require.NoError(t, os.MkdirAll(dest, 0o755))
	stale := filepath.Join(dest, "from-an-older-run.txt")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o644))

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "coverage"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	stdout := ghOutcomeStdout(t, res.Body.Bytes())
	assert.NotContains(t, stdout, "from-an-older-run.txt")
	assert.NoFileExists(t, stale)
	assert.Contains(t, stdout, "coverage.out")
}

// artifactRunDirs lists the run directories currently on disk.
func artifactRunDirs(t *testing.T, repoRoot string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot, ".tclaude-artifacts"))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "run-") {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

// TestGHProxy_DownloadsCannotAccumulateOnDisk is the disk-exhaustion bound.
//
// Every other limit here is PER DOWNLOAD, and per-download limits say nothing
// about a caller who simply asks again. Two shapes of "ask again" matter, and
// they are bounded differently:
//
//   - the SAME run, repeatedly — bounded because each download clears its own
//     directory first, so the footprint is one run's worth however many times
//     it is asked for;
//   - DIFFERENT runs — each gets its own directory, so without pruning a
//     caller with an endless supply of run ids fills a disk one perfectly
//     legal download at a time. That is what maxGHArtifactRuns bounds.
func TestGHProxy_DownloadsCannotAccumulateOnDisk(t *testing.T) {
	t.Run("the same run again replaces rather than accumulates", func(t *testing.T) {
		f, git, _ := downloadingWorld(t,
			ghArtifactsWireJSON(ghArtifact("logs", 4096, false)),
			map[string]string{"a.txt": "aaaa"})

		for range 5 {
			res := gitProxyPost(t, f, "/v1/github/run/download",
				map[string]any{"run_id": 18234567890, "name": "logs"})
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		}
		assert.Equal(t, []string{"run-18234567890"}, artifactRunDirs(t, git.repoRoot))

		// One copy of the file, not five appended into the same tree.
		entries, err := os.ReadDir(artifactDest(git.repoRoot, 18234567890))
		require.NoError(t, err)
		assert.Len(t, entries, 1)
	})

	t.Run("many different runs are pruned to a bounded set", func(t *testing.T) {
		f, git, _ := downloadingWorld(t,
			ghArtifactsWireJSON(ghArtifact("logs", 4096, false)),
			map[string]string{"a.txt": "aaaa"})

		for i := range 12 {
			res := gitProxyPost(t, f, "/v1/github/run/download",
				map[string]any{"run_id": 18234567890 + i, "name": "logs"})
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		}
		dirs := artifactRunDirs(t, git.repoRoot)
		assert.LessOrEqual(t, len(dirs), 3,
			"twelve downloads must not leave twelve directories: %v", dirs)
		assert.Contains(t, dirs, "run-18234567901", "the most recent one has to survive")
	})

	// The prune bound is only a bound if it cannot be switched off, and the
	// agent owns the directory the bound is applied to. Taking the read bit off
	// it is the obvious attempt: if the daemon reacted by pruning nothing and
	// downloading anyway, one chmod would buy back unbounded accumulation.
	//
	// It refuses instead — earlier than pruning, as it happens, because os.Root
	// traverses by opening each component and that needs the read bit too. What
	// matters is the outcome, so that is what this asserts rather than which
	// gate produced it.
	t.Run("an unreadable artifacts directory refuses the download", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores the read bit, so the unreadable case cannot be staged")
		}
		f, git, gh := downloadingWorld(t,
			ghArtifactsWireJSON(ghArtifact("logs", 4096, false)),
			map[string]string{"a.txt": "aaaa"})

		base := filepath.Join(git.repoRoot, ".tclaude-artifacts")
		require.NoError(t, os.MkdirAll(base, 0o755))
		require.NoError(t, os.Chmod(base, 0o300)) // write and traverse, not read
		t.Cleanup(func() { _ = os.Chmod(base, 0o755) })

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890, "name": "logs"})
		require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
		assert.Empty(t, gh.streamed(),
			"nothing may be downloaded into a directory whose contents the cap cannot see")
	})

	// Pruning deletes, so it must only ever delete what this proxy created.
	t.Run("a directory the proxy did not create is left alone", func(t *testing.T) {
		f, git, _ := downloadingWorld(t,
			ghArtifactsWireJSON(ghArtifact("logs", 4096, false)),
			map[string]string{"a.txt": "aaaa"})

		base := filepath.Join(git.repoRoot, ".tclaude-artifacts")
		require.NoError(t, os.MkdirAll(filepath.Join(base, "my-own-notes"), 0o755))
		keep := filepath.Join(base, "my-own-notes", "keep.txt")
		require.NoError(t, os.WriteFile(keep, []byte("mine"), 0o644))

		for i := range 6 {
			res := gitProxyPost(t, f, "/v1/github/run/download",
				map[string]any{"run_id": 18234567890 + i, "name": "logs"})
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		}
		assert.FileExists(t, keep, "only `run-<digits>` directories are the proxy's to prune")
	})
}

// TestGHProxy_DownloadRefusesAnArtifactThatUnpacksTooLarge — the 512 MiB gate
// reads GitHub's COMPRESSED size, so it cannot see a zip bomb coming. On a
// public repository a fork's pull request can upload one, and `run download`
// would fetch it.
//
// The daemon unpacks the archive itself, so the cap is enforced AS BYTES ARE
// WRITTEN. That is the difference from the subprocess this replaced, which
// could only measure the wreckage afterwards.
func TestGHProxy_DownloadRefusesAnArtifactThatUnpacksTooLarge(t *testing.T) {
	// The cap is lowered rather than met: proving the refusal does not require
	// materializing two real gigabytes on the runner, and a test that writes
	// them fails for unrelated reasons on a tmpfs TMPDIR or a small disk.
	t.Cleanup(agentd.SetMaxArtifactUnpackedBytesForTest(4 << 20))

	// A tiny "compressed" size, unpacking to more than the on-disk cap.
	big := strings.Repeat("x", 1<<20)
	files := map[string]string{}
	for i := range 8 {
		files[fmt.Sprintf("bomb/%04d.bin", i)] = big
	}
	f, git, _ := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("innocent-looking", 64<<10, false)), files)

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "innocent-looking"})
	require.Equal(t, http.StatusRequestEntityTooLarge, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "unpacked")

	assert.NoDirExists(t, artifactDest(git.repoRoot, 18234567890),
		"an artifact refused for its unpacked size must not be left on the disk it would fill")
}

// TestGHProxy_FailedDownloadDoesNotLeaveAPartialTree — a transfer killed
// part-way has still written whatever it got, and nothing else would remove it
// until that same run is downloaded again.
func TestGHProxy_FailedDownloadDoesNotLeaveAPartialTree(t *testing.T) {
	f, git, gh := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("logs", 4096, false)), nil)
	gh.streamErr = fmt.Errorf("connection reset by peer")

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "logs"})
	require.Equal(t, http.StatusBadGateway, res.Code, "body=%s", res.Body.String())
	assert.Contains(t, res.Body.String(), "connection reset")

	assert.NoDirExists(t, artifactDest(git.repoRoot, 18234567890),
		"a failed download's partial tree must not be left behind")
}

// TestGHProxy_DownloadReportsGitHubsRefusal — an artifact GitHub will not hand
// over (revoked token, artifact deleted between the manifest and the transfer)
// is an answer, not a daemon failure.
func TestGHProxy_DownloadReportsGitHubsRefusal(t *testing.T) {
	f, git, _ := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("logs", 4096, false)), nil)
	// No zip registered for the artifact path, so the stub answers 404.

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "logs"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	var out struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
	assert.NotEqual(t, 0, out.ExitCode)
	assert.NoDirExists(t, artifactDest(git.repoRoot, 18234567890))
}

// TestGHProxy_DownloadDirectoryIgnoresItself — an agent that downloads an
// artifact mid-branch should not then commit it by reflex, and the operator
// should not have to edit .gitignore to prevent that. A directory whose
// contents are all ignored does not appear as untracked at all.
func TestGHProxy_DownloadDirectoryIgnoresItself(t *testing.T) {
	f, git, _ := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("coverage", 4096, false)),
		map[string]string{"coverage.out": "mode: set\n"})

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "coverage"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	ignore, err := os.ReadFile(filepath.Join(git.repoRoot, ".tclaude-artifacts", ".gitignore"))
	require.NoError(t, err)
	assert.Equal(t, "*\n", string(ignore))
}

// TestGHProxy_DownloadRefusesToFollowASymlinkOutOfTheWorkTree — the agent can
// write its own work tree, so it can replace the artifacts directory with a
// link to somewhere it cannot write. The daemon can, which is the whole point
// of the check: every step of the destination is taken through an os.Root that
// refuses a traversal leaving the tree.
func TestGHProxy_DownloadRefusesToFollowASymlinkOutOfTheWorkTree(t *testing.T) {
	f, git, gh := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("coverage", 4096, false)), nil)

	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside,
		filepath.Join(git.repoRoot, ".tclaude-artifacts")))

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "coverage"})
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())

	assert.Empty(t, gh.streamed(), "only the preflight ran; nothing was downloaded")
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing may be written through the link")
}

// TestGHProxy_DownloadRefusesAnAllOfThemDownloadItCannotSize closes the gap
// between what the preflight MEASURES and what is then FETCHED.
//
// The manifest is one page. A download without --name fetches every artifact in
// the RUN, which on a busy run is not the same set — so sizing it against the
// page would let several times the cap through while reporting a number under
// it. Naming an artifact removes the gap, because then exactly what was
// measured is what is fetched.
func TestGHProxy_DownloadRefusesAnAllOfThemDownloadItCannotSize(t *testing.T) {
	page := make([]string, 0, 100)
	for i := range 100 {
		page = append(page, ghArtifact(fmt.Sprintf("shard-%02d", i), 4<<20, false))
	}

	t.Run("every artifact, on a run with more than a page of them", func(t *testing.T) {
		f, _, gh := downloadingWorld(t, ghArtifactsWirePage(400, page...), nil)

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890})
		require.Equal(t, http.StatusRequestEntityTooLarge, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "400 artifacts")
		assert.Empty(t, gh.streamed(), "nothing may be fetched")
	})

	t.Run("one named artifact is measured exactly and allowed", func(t *testing.T) {
		f, _, gh := downloadingWorld(t, ghArtifactsWirePage(400, page...),
			map[string]string{"shard.txt": "ok\n"})

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890, "name": "shard-07"})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		assert.Len(t, gh.streamed(), 1)
	})
}

// TestGHProxy_DownloadRefusesAManifestItCannotUnderstand — without a manifest
// there is no size decision to make, and proceeding as though there had been
// one is how the disk bound stops being a bound.
func TestGHProxy_DownloadRefusesAManifestItCannotUnderstand(t *testing.T) {
	f, _, gh := downloadingWorld(t, `{"total_count": "not a number"}`, nil)

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	require.Equal(t, http.StatusBadGateway, res.Code, "body=%s", res.Body.String())
	assert.Empty(t, gh.streamed(), "nothing may be fetched on an unsizeable manifest")
}

// TestGHProxy_DownloadSharesOneBudgetAcrossEveryCall — the CLI waits on a
// single number, so the daemon's worst case has to be that number however the
// work divides between the manifest read and the transfers.
func TestGHProxy_DownloadSharesOneBudgetAcrossEveryCall(t *testing.T) {
	f, _, gh := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("coverage", 4096, false)),
		map[string]string{"coverage.out": "mode: set\n"})

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "coverage"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

	gh.mu.Lock()
	defer gh.mu.Unlock()
	require.NotEmpty(t, gh.budgets)
	for i, budget := range gh.budgets {
		assert.LessOrEqual(t, budget, 300*time.Second,
			"call %d must run inside the verb's total budget", i)
		assert.Greater(t, budget, time.Second, "call %d has no usable budget at all", i)
	}
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
		files[fmt.Sprintf("%sfile-%03d.txt", deep, i)] = "x"
	}
	f, _, _ := downloadingWorld(t,
		ghArtifactsWireJSON(ghArtifact("logs", 4096, false)), files)

	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890, "name": "logs"})
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
		f, git, gh := downloadingWorld(t,
			ghArtifactsWireJSON(ghArtifact("coverage", 4096, false)), nil)
		require.NoError(t, os.WriteFile(
			filepath.Join(git.repoRoot, ".tclaude-artifacts"), []byte("not a dir"), 0o644))

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890, "name": "coverage"})
		require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
		assert.Empty(t, gh.streamed())
	})

	// The per-run directory is the one the daemon RemoveAlls before each
	// download, so a symlink there aims a deletion rather than a write. It does
	// not have to be refused — unlinking a symlink is not following it — but it
	// must not reach through.
	t.Run("the per-run directory symlinked out of the tree", func(t *testing.T) {
		f, git, _ := downloadingWorld(t,
			ghArtifactsWireJSON(ghArtifact("coverage", 4096, false)),
			map[string]string{"coverage.out": "mode: set\n"})
		base := filepath.Join(git.repoRoot, ".tclaude-artifacts")
		require.NoError(t, os.MkdirAll(base, 0o755))
		outside := t.TempDir()
		bystander := filepath.Join(outside, "someone-elses-file")
		require.NoError(t, os.WriteFile(bystander, []byte("keep me"), 0o644))
		require.NoError(t, os.Symlink(outside, filepath.Join(base, "run-18234567890")))

		res := gitProxyPost(t, f, "/v1/github/run/download",
			map[string]any{"run_id": 18234567890, "name": "coverage"})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

		// The link was replaced by a real directory inside the work tree...
		dest := artifactDest(git.repoRoot, 18234567890)
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
		f, git, gh := ghWorld(t, []string{"github.com/tofutools"})
		res := gitProxyPost(t, f, "/v1/github/run/artifacts",
			map[string]any{"run_id": 18234567890})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 0, gh.count())
		assert.False(t, git.sawAnySubprocess())
	})

	t.Run("granted", func(t *testing.T) {
		f, _, gh := downloadingWorld(t,
			ghArtifactsWireJSON(ghArtifact("coverage", 4096, false)), nil)

		res := gitProxyPost(t, f, "/v1/github/run/artifacts",
			map[string]any{"run_id": 18234567890})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

		call := gh.only(t)
		assert.Equal(t, "repos/tofutools/tclaude/actions/runs/18234567890/artifacts", call.Path)
		assert.Equal(t, "100", call.Query.Get("per_page"))
		assert.Equal(t, http.MethodGet, orGet(call.Method),
			"a read verb must not write with the operator's credential")

		var out struct {
			JSON json.RawMessage `json:"json"`
		}
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &out))
		assert.Contains(t, string(out.JSON), "coverage",
			"a JSON read must ride in the JSON field, not be flattened into text")
		assert.Contains(t, string(out.JSON), `"total":1`,
			"the run's real count is how a partial page is detected at all")
		assert.Contains(t, string(out.JSON), "size_in_bytes")
		// The projection is what keeps this read small: each raw entry embeds a
		// complete copy of the workflow-run object, and a page of those does
		// not fit any bound this proxy would accept.
		assert.NotContains(t, string(out.JSON), "workflow_run")
	})
}

// TestGHProxy_DownloadRequiresTheReadSlug — it writes to the agent's own disk,
// but what it spends is a GitHub read.
func TestGHProxy_DownloadRequiresTheReadSlug(t *testing.T) {
	f, git, gh := ghWorld(t, []string{"github.com/tofutools"})
	res := gitProxyPost(t, f, "/v1/github/run/download",
		map[string]any{"run_id": 18234567890})
	assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 0, gh.count(), "a denied caller spends nothing at all")
	assert.False(t, git.sawAnySubprocess())
	assert.NoDirExists(t, filepath.Join(git.repoRoot, ".tclaude-artifacts"),
		"and creates no directory either")
}
