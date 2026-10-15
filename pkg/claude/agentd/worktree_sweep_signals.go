package agentd

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const sweepStaleDays = 30

// Unknown Git signals stay nil rather than being reported as safe. All probes
// are local: no fetch, network access, or worktree mutations. A shared deadline
// bounds the extra Git work per repository, including large worktree sets.
type sweepSignals struct {
	BaseRef             string `json:"base_ref,omitempty"`
	Merged              *bool  `json:"merged"`
	Unpushed            *int   `json:"unpushed_commits"`
	UpstreamGone        bool   `json:"upstream_gone"`
	LastCommit          int64  `json:"last_commit,omitempty"`
	LastActivity        int64  `json:"last_activity,omitempty"`
	Stale               bool   `json:"stale"`
	SignalError         string `json:"signal_error,omitempty"`
	PotentiallyDangling bool   `json:"potentially_dangling"`
	DanglingReason      string `json:"dangling_reason,omitempty"`
}

type sweepRef struct {
	commit, upstream string
	timestamp        int64
}

func addSweepSignals(roots []string, rows []sweepWorktree) {
	for _, root := range roots {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		scanSweepSignals(ctx, root, rows)
		cancel()
	}
}

func scanSweepSignals(ctx context.Context, root string, rows []sweepWorktree) {
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		out, err := cmd.Output()
		return strings.TrimRight(string(out), "\r\n"), err
	}
	refs := map[string]sweepRef{}
	raw, refsErr := git("for-each-ref", "--format=%(refname)%09%(objectname)%09%(committerdate:unix)%09%(upstream)", "refs/heads/", "refs/remotes/")
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			continue
		}
		timestamp, _ := strconv.ParseInt(fields[2], 10, 64)
		refs[fields[0]] = sweepRef{fields[1], fields[3], timestamp}
	}
	base, _ := git("symbolic-ref", "refs/remotes/origin/HEAD")
	if _, ok := refs[base]; !ok {
		base = ""
		for _, candidate := range []string{"refs/heads/main", "refs/heads/master"} {
			if _, ok := refs[candidate]; ok {
				base = candidate
				break
			}
		}
	}
	now := time.Now()
	for i := range rows {
		row := &rows[i]
		if row.RepoRoot != root {
			continue
		}
		row.BaseRef = base
		ref, known := refs["refs/heads/"+row.Branch]
		// Detached checkouts still have a registered commit, without a branch upstream.
		if !known {
			ref.commit = row.commit
			if ref.commit != "" {
				date, err := git("show", "-s", "--format=%ct", ref.commit, "--")
				if err == nil {
					ref.timestamp, _ = strconv.ParseInt(date, 10, 64)
				}
			}
		}
		row.LastCommit = ref.timestamp
		row.LastActivity = row.LastCommit
		if info, err := os.Stat(row.Path); err == nil && info.ModTime().Unix() > row.LastActivity {
			row.LastActivity = info.ModTime().Unix()
		}
		row.Stale = row.LastActivity > 0 && now.Sub(time.Unix(row.LastActivity, 0)) >= sweepStaleDays*24*time.Hour
		if refsErr != nil || base == "" || ref.commit == "" {
			row.SignalError = "Git signals unavailable (missing base or commit)"
		} else {
			counts, err := git("rev-list", "--left-right", "--count", refs[base].commit+"..."+ref.commit, "--")
			fields := strings.Fields(counts)
			if err != nil || len(fields) != 2 {
				row.SignalError = "Git comparison unavailable or scan timed out"
			} else if ahead, err := strconv.Atoi(fields[1]); err == nil {
				merged := ahead == 0
				row.Merged = &merged
				// Without a surviving upstream, unpublished means commits absent from
				// base. This is deliberately conservative after remote branch deletion.
				row.Unpushed = &ahead
				if ref.upstream != "" {
					upstream, exists := refs[ref.upstream]
					row.UpstreamGone = !exists
					if exists {
						count, err := git("rev-list", "--count", upstream.commit+".."+ref.commit, "--")
						n, parseErr := strconv.Atoi(count)
						if err != nil || parseErr != nil {
							row.Unpushed = nil
							row.SignalError = "Upstream comparison unavailable or scan timed out"
						} else {
							row.Unpushed = &n
						}
					}
				}
			}
		}
		if row.Unpushed != nil && *row.Unpushed > 0 {
			row.Checked = false
			row.Reason = "commits absent from upstream/base — review before deleting"
		}
		if !row.IsMain && row.Category != "live" {
			if row.Merged != nil && *row.Merged {
				row.DanglingReason = "merged into " + base
			}
			if row.Stale {
				if row.DanglingReason != "" {
					row.DanglingReason += "; "
				}
				row.DanglingReason += "no recent activity (30 days)"
			}
			row.PotentiallyDangling = row.DanglingReason != ""
		}
	}
}
