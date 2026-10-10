package agentd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
)

type arrivalContextKey struct{}
type arrivalContext struct {
	Operation, Peer, Reason, Note string
	Source                        agentbundle.Definition
	PermissionSummary             string
	LocalClone                    bool
	Restored                      bool
}

func arrivalFromRequest(r *http.Request) *arrivalContext {
	a, _ := r.Context().Value(arrivalContextKey{}).(*arrivalContext)
	return a
}
func arrivalFromOffer(o *db.FederationBundleOffer, d agentbundle.Definition) *arrivalContext {
	a := &arrivalContext{Operation: "clone / shared agent import", Peer: o.Peer, Source: d}
	if o.Descriptor.Move != nil {
		a.Operation = "move"
		if o.Descriptor.Move.DirectIfAllowed {
			a.Operation = "direct move"
		}
	}
	if d.Origin != nil {
		copyOrigin := *d.Origin
		a.Source.Origin = &copyOrigin
	}
	if a.Source.Origin == nil {
		a.Source.Origin = &agentbundle.Origin{}
	}
	if o.Descriptor.Move != nil {
		a.Source.Origin.Agent = o.Descriptor.Move.SourceAgent
	}
	if t := o.Descriptor.Teleport; t != nil {
		a.Operation = "teleport"
		if t.Clone {
			a.Operation = "teleport clone"
		}
		if t.Home {
			a.Operation = "teleport home"
		}
		a.Note = t.Note
		a.Source.Origin.Agent = t.SourceAgent
	}
	return a
}
func arrivalNode() string {
	if rt := currentFederation(); rt != nil {
		return rt.id.ID()
	}
	return "this node"
}
func arrivalTrigger(caller, source string, human bool) string {
	if human {
		return "operator"
	}
	if caller == source {
		return "agent itself"
	}
	if id, _ := db.AgentIDForConv(caller); id != "" {
		return "agent " + id
	}
	return "another agent"
}
func arrivalOrigin(conv, cwd, model string) *agentbundle.Origin {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	o := &agentbundle.Origin{Instance: arrivalNode(), Model: model, Trigger: "unknown"}
	o.Agent, _ = db.AgentIDForConv(conv)
	if commit, ok := arrivalGit(ctx, cwd, "rev-parse", "HEAD"); ok {
		o.Commit = commit
	}
	if status, ok := arrivalGit(ctx, cwd, "status", "--porcelain", "--untracked-files=normal"); ok {
		dirty := status != ""
		o.Dirty = &dirty
	}
	return o
}

// Quote and bound data fields so paths, notes and branch names remain facts,
// not additional lines or terminal controls in an injected message.
func arrivalQuote(s string) string {
	if s == "" {
		s = "unknown"
	}
	r := []rune(s)
	if len(r) > 240 {
		s = string(r[:240]) + "…"
	}
	return strconv.Quote(s)
}

var arrivalCommitRE = regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`)

// These are local read-only Git probes, sharing one short deadline. Disable
// fsmonitor hooks and discard output beyond the bounded diagnostic prefix.
func arrivalGit(ctx context.Context, cwd string, args ...string) (string, bool) {
	out, ok, _ := arrivalGitProbe(ctx, cwd, args...)
	return out, ok
}

// Exit 1 from the quiet verification commands means absence. Other failures,
// including a consumed deadline, leave the fact unknown.
func arrivalGitProbe(ctx context.Context, cwd string, args ...string) (string, bool, bool) {
	if cwd == "" {
		return "", false, false
	}
	argv := append([]string{"-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-C", cwd}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	out := &arrivalBuffer{}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		missing := ctx.Err() == nil && errors.As(err, &exit) && exit.ExitCode() == 1
		return "", false, missing
	}
	return strings.TrimSpace(out.String()), true, false
}

type arrivalBuffer struct{ bytes.Buffer }

func (b *arrivalBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if left := 8192 - b.Len(); left > 0 {
		_, _ = b.Buffer.Write(p[:min(left, len(p))])
	}
	return n, nil
}
func buildArrivalBriefing(a arrivalContext, agentID, cwd, group, h, model string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sourceAgent := "unknown"
	sourceNode, trigger, sourceModel, sourceCommit, sourceDirty := "unknown", "unknown", "unknown", "", "unknown"
	if o := a.Source.Origin; o != nil {
		sourceNode, trigger, sourceModel, sourceCommit = o.Instance, o.Trigger, o.Model, o.Commit
		sourceAgent = o.Agent
		if o.Dirty != nil {
			if *o.Dirty {
				sourceDirty = "uncommitted changes reported"
			} else {
				sourceDirty = "clean reported"
			}
		}
	}
	if a.Peer != "" {
		sourceNode = a.Peer
	}
	exists := "unknown"
	if a.Source.Paths.Cwd != "" {
		if fi, err := os.Stat(a.Source.Paths.Cwd); err == nil && fi.IsDir() {
			exists = "yes"
		} else if os.IsNotExist(err) {
			exists = "no"
		}
	}
	reason := a.Reason
	if reason == "" {
		reason = "receiver-selected path"
	}
	permissions := "source permissions not copied; receiver policy applies; fresh inbox"
	if a.PermissionSummary != "" {
		permissions = a.PermissionSummary + "; fresh inbox"
	}
	if a.LocalClone {
		permissions = "local permissions inherited; independent inbox"
	}
	if a.Restored {
		permissions = "restored home identity and inbox; original local permissions retained"
	}
	if group == "" {
		group = "ungrouped"
	}
	sourceLabel := "From"
	if a.Peer == "" && !a.LocalClone {
		sourceLabel = "From (source-reported)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Arrival briefing — %s\n%s %s/%s → %s; trigger (source-reported): %s. Identity: %s; inbox: tclaude agent inbox; group: %s. %s.\n", a.Operation, sourceLabel, arrivalQuote(sourceNode), arrivalQuote(sourceAgent), arrivalQuote(arrivalNode()), arrivalQuote(trigger), arrivalQuote(agentID), arrivalQuote(group), permissions)
	fmt.Fprintf(&b, "Cwd: %s (%s); source cwd %s exists here: %s. Harness/model: %s/%s (source-reported: %s/%s).\n", arrivalQuote(cwd), arrivalQuote(reason), arrivalQuote(a.Source.Paths.Cwd), exists, arrivalQuote(h), arrivalQuote(model), arrivalQuote(a.Source.Harness), arrivalQuote(sourceModel))
	root, repo := arrivalGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if !repo {
		b.WriteString("Git: no repository verified at landing.\n")
	} else {
		branch, ok, detached := arrivalGitProbe(ctx, cwd, "symbolic-ref", "--quiet", "--short", "HEAD")
		if !ok {
			branch = "unknown"
			if detached {
				branch = "detached HEAD"
			}
		}
		dirty := "unknown"
		if status, ok := arrivalGit(ctx, cwd, "status", "--porcelain", "--untracked-files=normal"); ok {
			dirty = "clean"
			if status != "" {
				dirty = "dirty"
			}
		}
		branchHere := "unknown"
		if a.Source.Paths.Branch != "" {
			if _, ok, missing := arrivalGitProbe(ctx, cwd, "show-ref", "--quiet", "--verify", "--", "refs/heads/"+a.Source.Paths.Branch); ok {
				branchHere = "yes"
			} else if missing {
				branchHere = "no"
			}
		}
		commitHere := "unknown"
		if arrivalCommitRE.MatchString(sourceCommit) {
			if _, ok, missing := arrivalGitProbe(ctx, cwd, "rev-parse", "--quiet", "--verify", sourceCommit+"^{commit}"); ok {
				commitHere = "yes"
			} else if missing {
				commitHere = "no"
			}
		}
		fmt.Fprintf(&b, "Git: %s; remote %s; branch %s; %s. Source branch %s present: %s; source commit %s present: %s.\n", arrivalQuote(root), arrivalQuote(jobrepo.OriginHint(ctx, cwd)), arrivalQuote(branch), dirty, arrivalQuote(a.Source.Paths.Branch), branchHere, arrivalQuote(sourceCommit), commitHere)
	}
	if a.LocalClone {
		fmt.Fprintf(&b, "Source worktree %s; %s. Files stay on this node; no files, credentials, sidecars or mail were copied into a new directory.\n", arrivalQuote(a.Source.Paths.Worktree), sourceDirty)
	} else {
		fmt.Fprintf(&b, "Source-reported worktree %s; %s. Not transferred: worktree contents, uncommitted changes, credentials, sidecars or mail.\n", arrivalQuote(a.Source.Paths.Worktree), sourceDirty)
	}
	if a.Note != "" {
		fmt.Fprintf(&b, "Note (source-reported): %s.\n", arrivalQuote(a.Note))
	}
	b.WriteString("Re-check remembered paths before using them. If the needed branch is missing, fetch it from a trusted remote.\nAsk the operator if the task needs files that are absent here; do not assume they arrived with the conversation.")
	return b.String()
}
