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
// files); a variable so tests never run the operator's awb.
var awbCLIStatus = func(ctx context.Context) ([]byte, error) {
	path, err := exec.LookPath("awb")
	if err != nil {
		return nil, errAWBCLIMissing
	}
	cmd := exec.CommandContext(ctx, path, "status", "--json", "--no-color")
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("awb status failed: %s", guideText(strings.TrimSpace(string(exit.Stderr)), 160))
		}
		return nil, fmt.Errorf("awb status failed: %v", err)
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
)

// awbCLIBaseCache keeps what awb last said, so a board read runs awb once in
// awbCLIBaseTTL at most. gen tells a refresh started before a reset (a test
// swapping awbCLIStatus) that its answer is no longer wanted.
var awbCLIBaseCache struct {
	mu         sync.Mutex
	loaded     bool
	refreshing bool
	gen        int
	base, note string
	at         time.Time
}

// awbCLIBase is the web base of the awb server the awb CLI is configured
// for, and why there is none. The first call asks awb and waits for it;
// after that a stale answer is served while a fresh one is read.
func awbCLIBase() (string, string) {
	c := &awbCLIBaseCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		c.base, c.note = probeAWBCLIBase()
		c.at, c.loaded = time.Now(), true
		return c.base, c.note
	}
	ttl := awbCLIBaseTTL
	if c.base == "" {
		ttl = awbCLIBaseRetry
	}
	if time.Since(c.at) > ttl && !c.refreshing {
		c.refreshing = true
		gen := c.gen
		go func() {
			base, note := probeAWBCLIBase()
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.gen != gen {
				return
			}
			c.base, c.note, c.at, c.refreshing = base, note, time.Now(), false
		}()
	}
	return c.base, c.note
}

// resetAWBCLIBase forgets what awb said, so the next read asks again.
func resetAWBCLIBase() {
	c := &awbCLIBaseCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loaded, c.refreshing, c.base, c.note = false, false, "", ""
	c.gen++
}

// probeAWBCLIBase asks awb once: a remote connection's server is the base
// its issue links are on (awb's issue_link is <server>#/issues/<id>).
func probeAWBCLIBase() (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), awbCLIProbeTimeout)
	defer cancel()
	out, err := awbCLIStatus(ctx)
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
// id alone does not say the workspace. It is learned per team from any link
// the board sees: one Linear returned on a lookup, or one a ref was given.

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
	learned          map[string]bool            // Linear links already learned from in this read
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
			if base, team, ok := linearWorkspaceOf(ref.URL); ok && !l.learned[team+" "+base] {
				if l.learned == nil {
					l.learned = map[string]bool{}
				}
				l.learned[team+" "+base] = true
				learnLinearWorkspace(ref.URL)
			}
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
