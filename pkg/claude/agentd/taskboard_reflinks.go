package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A tracker ref's link is derived as the board is read whenever it has none
// stored: an awb id on this node's awb server, a Linear id in its team's
// workspace, owner/repo#n on GitHub. A stored link always wins. A ref that
// still has none says why (link_note), so a chip is never silently plain.
//
// Derived links are not written back: the next read derives them again, so
// a later change of awb server or a first Linear lookup links every old ref
// at once. A ref written from now on (add, edit, import) stores its link
// when it is known then, as it always has.

// ---- awb ----

// awbLinkBase is the awb web base refs are linked on, and why there is none:
// the configured awb proxy (agent.awb_proxy.url) first, then the server the
// operator's own awb CLI is configured for (awbCLIBase).
func awbLinkBase() (base, note string) {
	cfg, _ := config.Load()
	proxyNote := ""
	if raw := cfg.ResolvedAWBProxy().URL; raw != "" {
		b, fault := validateAWBBaseURL(raw)
		if fault == nil {
			return b, ""
		}
		proxyNote = fault.Msg
	}
	b, cliNote := awbCLIBase()
	if b != "" {
		return b, ""
	}
	if proxyNote != "" {
		return "", proxyNote
	}
	note = "no awb server URL is configured: set agent.awb_proxy.url in ~/.tclaude/data/config.json, or point the awb CLI at an awb server"
	if cliNote != "" {
		note += " (" + cliNote + ")"
	}
	return "", note
}

// awbCLIStatus runs `awb status --json` the way the operator would, so awb
// reads its own configuration (AWB_CONFIG_FILE, its user and local config
// files). Tests swap it with SetAWBCLIStatusForTest, under the cache's lock,
// so they never run the operator's awb. Its error is what a board shows,
// also to linked nodes, so awb's own output stays in this node's log.
func awbCLIStatus(ctx context.Context) ([]byte, error) {
	path, err := exec.LookPath("awb")
	if err != nil {
		return nil, errAWBCLIMissing
	}
	out, err := exec.CommandContext(ctx, path, "status", "--json", "--no-color").Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			slog.Info("taskboard: awb status failed", "error", err,
				"stderr", guideText(strings.TrimSpace(string(exit.Stderr)), 300), "module", "agentd")
		}
		return nil, errors.New("awb status failed (see the daemon log)")
	}
	return out, nil
}

var errAWBCLIMissing = errors.New("no awb CLI on the daemon's PATH")

const (
	// awbCLIBaseTTL is how long a found server is kept before it is read
	// again (in the background: a board read never waits on a refresh).
	awbCLIBaseTTL = 10 * time.Minute
	// awbCLIBaseRetry is how long "none" is kept before awb is asked again.
	awbCLIBaseRetry = time.Minute
	// awbCLIProbeTimeout bounds one `awb status`; it asks its server too.
	awbCLIProbeTimeout = 4 * time.Second
	// awbCLIFirstWait is how long the first reads wait for the first answer;
	// past it they go on without a server until it lands.
	awbCLIFirstWait = 1500 * time.Millisecond
)

// awbCLICache keeps what awb last said, so awb runs once in awbCLIBaseTTL
// at most, always off the lock. gen tells a probe started before a reset (a
// test swapping the probe) that its answer is not wanted.
type awbCLICache struct {
	mu         sync.Mutex
	probe      func(context.Context) ([]byte, error) // nil: awbCLIStatus
	started    bool
	ready      chan struct{} // closed when the first answer lands
	refreshing bool
	gen        int
	base, note string
	at         time.Time
}

var awbCLIBaseCache awbCLICache

// awbCLIBase is the web base of the awb server the awb CLI is configured
// for, and why there is none. The first call starts asking awb and waits up
// to awbCLIFirstWait for it; after that a stale answer is served while a
// fresh one is read.
func awbCLIBase() (string, string) {
	c := &awbCLIBaseCache
	c.mu.Lock()
	if !c.started {
		c.started = true
		c.ready = make(chan struct{})
		c.startProbeLocked()
	}
	ready := c.ready
	c.mu.Unlock()
	select {
	case <-ready:
	case <-time.After(awbCLIFirstWait):
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready != ready || !closed(ready) {
		return "", "still asking the awb CLI for its server"
	}
	ttl := awbCLIBaseTTL
	if c.base == "" {
		ttl = awbCLIBaseRetry
	}
	if time.Since(c.at) > ttl && !c.refreshing {
		c.startProbeLocked()
	}
	return c.base, c.note
}

func closed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// startProbeLocked asks awb in the background, with the probe taken under
// c.mu (held by the caller); the answer is kept only if no reset came since.
func (c *awbCLICache) startProbeLocked() {
	c.refreshing = true
	probe, gen, ready := c.probe, c.gen, c.ready
	if probe == nil {
		probe = awbCLIStatus
	}
	go func() {
		base, note := probeAWBCLIBase(probe)
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.gen != gen {
			return
		}
		c.base, c.note, c.at, c.refreshing = base, note, time.Now(), false
		if !closed(ready) {
			close(ready)
		}
	}()
}

// setAWBCLIProbe swaps the probe (nil: awbCLIStatus) and forgets what the
// last one said, so the next read asks again.
func setAWBCLIProbe(fn func(context.Context) ([]byte, error)) (prev func(context.Context) ([]byte, error)) {
	c := &awbCLIBaseCache
	c.mu.Lock()
	defer c.mu.Unlock()
	prev = c.probe
	c.probe = fn
	c.started, c.refreshing, c.base, c.note, c.ready = false, false, "", "", nil
	c.gen++
	return prev
}

// probeAWBCLIBase asks awb once: a remote connection's server is the base
// its issue links are on (awb's issue_link is <server>#/issues/<id>).
func probeAWBCLIBase(probe func(context.Context) ([]byte, error)) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), awbCLIProbeTimeout)
	defer cancel()
	out, err := probe(ctx)
	if err != nil {
		return "", err.Error()
	}
	var st struct {
		Connection struct {
			Mode   string `json:"mode"`
			Server string `json:"server"`
		} `json:"connection"`
	}
	if json.Unmarshal(out, &st) != nil {
		return "", "awb status gave no JSON"
	}
	if st.Connection.Mode != "remote" || strings.TrimSpace(st.Connection.Server) == "" {
		return "", "the awb CLI uses a local database, which has no web link"
	}
	base, fault := validateAWBBaseURL(st.Connection.Server)
	if fault != nil {
		return "", "the awb CLI's server is not a usable base URL"
	}
	return base, ""
}

// ---- Linear ----

// A Linear issue's link is https://linear.app/<workspace>/issue/<ID>, and an
// id alone does not say the workspace. It is learned per team from Linear's
// own answer to a lookup (resolve_ref, an import, the dashboard's ref
// lookup) — never from a link someone stored on a ref, which any board
// writer (a linked node too) could point at another workspace.

// linearWorkspaceCacheKey is the git cache key of a team's workspace base.
func linearWorkspaceCacheKey(team string) string {
	h := sha256.Sum256([]byte("linear-workspace\x00" + strings.ToUpper(team)))
	return "tlinws_" + hex.EncodeToString(h[:8])
}

// linearTeamOf is a Linear id's team key (LIN of LIN-12), "" for anything
// else.
func linearTeamOf(id string) string {
	if !taskRefLinearRe.MatchString(strings.ToUpper(id)) {
		return ""
	}
	team, _, _ := strings.Cut(strings.ToUpper(id), "-")
	return team
}

// linearWorkspaceOf is a Linear issue link's workspace base and its team:
// https://linear.app/acme/issue/LIN-7/… gives https://linear.app/acme, LIN.
func linearWorkspaceOf(rawURL string) (base, team string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" || u.User != nil {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	if host != "linear.app" && !strings.HasSuffix(host, ".linear.app") {
		return "", "", false
	}
	segs := pathSegments(u.Path)
	for i, s := range segs {
		if strings.EqualFold(s, "issue") && i >= 1 && i+1 < len(segs) {
			if team = linearTeamOf(segs[i+1]); team == "" {
				return "", "", false
			}
			return "https://" + u.Host + "/" + strings.Join(segs[:i], "/"), team, true
		}
	}
	return "", "", false
}

// learnLinearWorkspace keeps the workspace a Linear issue link is in, for its
// team's link-less refs. It writes only what is new.
func learnLinearWorkspace(rawURL string) {
	base, team, ok := linearWorkspaceOf(rawURL)
	if !ok {
		return
	}
	key := linearWorkspaceCacheKey(team)
	if row, err := db.LoadGitCache(key); err == nil && row != nil {
		var have struct {
			Base string `json:"base"`
		}
		if json.Unmarshal(row.Data, &have) == nil && have.Base == base {
			return
		}
	}
	data, err := json.Marshal(map[string]string{"base": base})
	if err != nil {
		return
	}
	if err := db.SaveGitCache(key, data, time.Now()); err != nil {
		slog.Warn("taskboard: failed to keep a Linear workspace", "error", err, "module", "agentd")
	}
}

// ---- the read path ----

// taskRefLinker links a board read's link-less refs: add each ref as the
// read makes it, then link them all at once (one awb read, one batch read of
// the Linear workspaces it needs).
type taskRefLinker struct {
	awbRead          bool
	awbBase, awbNote string
	linear           map[string][]*taskboardRef // team -> refs waiting for its workspace
}

func (l *taskRefLinker) add(ref *taskboardRef) {
	switch ref.Kind {
	case "awb":
		if !l.awbRead {
			l.awbBase, l.awbNote = awbLinkBase()
			l.awbRead = true
		}
		// A ref linked on this server is Local, with its id, whether its
		// link is stored or derived (taskboardRef.Local).
		linkLocalAWBRef(ref, l.awbBase)
		if ref.URL == "" {
			if _, fault := validateAWBIssueRef(ref.Label); fault != nil {
				ref.LinkNote = "not an awb issue id"
			} else {
				ref.LinkNote = l.awbNote
			}
		}
	case "linear":
		if ref.URL != "" {
			return
		}
		team := linearTeamOf(ref.Label)
		if team == "" {
			ref.LinkNote = "not a Linear issue id"
			return
		}
		if l.linear == nil {
			l.linear = map[string][]*taskboardRef{}
		}
		l.linear[team] = append(l.linear[team], ref)
	case "github":
		if ref.URL == "" {
			if m := taskRefGitHubRe.FindStringSubmatch(ref.Label); m != nil {
				ref.URL = "https://github.com/" + m[1] + "/" + m[2] + "/issues/" + m[3]
			} else {
				ref.LinkNote = "not owner/repo#n"
			}
		}
	default:
		if ref.URL == "" {
			ref.LinkNote = "the ref has no link"
		}
	}
}

// finish links the Linear refs whose team's workspace is known.
func (l *taskRefLinker) finish() {
	if len(l.linear) == 0 {
		return
	}
	keys := make([]string, 0, len(l.linear))
	for team := range l.linear {
		keys = append(keys, linearWorkspaceCacheKey(team))
	}
	rows, err := db.LoadGitCacheBatch(keys)
	for team, refs := range l.linear {
		base := ""
		if err == nil {
			if row := rows[linearWorkspaceCacheKey(team)]; row != nil {
				var have struct {
					Base string `json:"base"`
				}
				if json.Unmarshal(row.Data, &have) == nil {
					base = have.Base
				}
			}
		}
		for _, ref := range refs {
			id := strings.ToUpper(ref.Label)
			if u := base + "/issue/" + id; base != "" && validateTaskRefURL(u) == nil {
				ref.URL = u
				continue
			}
			ref.LinkNote = fmt.Sprintf("the Linear workspace of team %s is not known yet: it is learned from any %s issue's link, "+
				"so look one up (taskboard edit <task> --resolve-ref) or give one ref by its URL", team, team)
		}
	}
}

// ---- resolve notes ----

// A resolve (resolve_ref) that leaves a ref without a link says why: each
// such reason is kept on the request and answered as resolve_notes.

type refNotesKey struct{}

type refNotes struct {
	mu    sync.Mutex
	notes []string
}

// withRefNotes is r carrying a list for resolve notes.
func withRefNotes(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), refNotesKey{}, &refNotes{}))
}

// noteRefResolve keeps why ref label was not linked, on r's list.
func noteRefResolve(r *http.Request, label, note string) {
	if note == "" {
		return
	}
	if n, ok := r.Context().Value(refNotesKey{}).(*refNotes); ok {
		n.mu.Lock()
		n.notes = append(n.notes, label+": "+note)
		n.mu.Unlock()
	}
}

// refResolveNotes is what r's resolves kept.
func refResolveNotes(r *http.Request) []string {
	if n, ok := r.Context().Value(refNotesKey{}).(*refNotes); ok {
		n.mu.Lock()
		defer n.mu.Unlock()
		return append([]string(nil), n.notes...)
	}
	return nil
}
