package agentd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type federationLandingRepo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Ref  string `json:"ref"`
}
type federationLandingCandidate struct {
	ID               string                 `json:"id"`
	Cwd              string                 `json:"cwd"`
	Reason           string                 `json:"reason"`
	Exists           bool                   `json:"exists"`
	CheckoutRequired bool                   `json:"checkout_required,omitempty"`
	Repo             *federationLandingRepo `json:"repo,omitempty"`
}
type federationLandingPreview struct {
	federationLandingCandidate
	SourceCwd  string                       `json:"source_cwd"`
	SourceRepo string                       `json:"source_repo"`
	Candidates []federationLandingCandidate `json:"candidates"`
}
type federationLandingPlan struct {
	Preview  federationLandingPreview
	repo     *db.FederationRepo
	root     string
	checkout *jobrepo.Checkout
}

// A hint is compared, never fetched. Only a receiver's allowlisted definition
// can supply a transport URL to jobrepo.Prepare.
func federationRepoKey(raw string) string {
	if len(raw) > 4096 || jobrepo.ValidateURL(raw) != nil {
		return ""
	}
	var host, path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "file" {
			return ""
		}
		if u.User != nil {
			if _, password := u.User.Password(); password || u.Scheme != "ssh" {
				return ""
			}
		}
		host, path = u.Host, u.Path
	} else {
		left, right, ok := strings.Cut(raw, ":")
		if !ok {
			return ""
		}
		_, host, _ = strings.Cut(left, "@")
		if host == "" {
			host = left
		}
		path = right
	}
	return strings.ToLower(host) + "/" + strings.TrimSuffix(strings.Trim(path, "/"), ".git")
}
func federationLandingDirectory(path string) (string, bool, error) {
	if path == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(path) {
		return path, false, errors.New("landing directory must be an absolute receiver path")
	}
	clean, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path), false, nil
	}
	st, err := os.Stat(clean)
	if err != nil || !st.IsDir() {
		return clean, false, nil
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return clean, true, errors.New("landing directory must be owned by the receiving service user")
	}
	return clean, true, nil
}
func federationLandingBroad(path string) bool {
	home, _ := os.UserHomeDir()
	if a, e := os.Stat(path); e == nil {
		if b, e := os.Stat(home); e == nil && os.SameFile(a, b) {
			return true
		}
	}
	if runtime.GOOS == "darwin" {
		path = strings.ToLower(path)
	}
	for _, root := range []string{"/", "/etc", "/usr", "/var", "/private", "/private/etc", "/private/var", "/System", "/Library", "/bin", "/sbin", "/dev", "/proc", "/sys"} {
		if runtime.GOOS == "darwin" {
			root = strings.ToLower(root)
		}
		if path == root {
			return true
		}
	}
	for _, root := range []string{"/etc/", "/usr/", "/private/etc/", "/System/", "/Library/", "/dev/", "/proc/", "/sys/"} {
		if runtime.GOOS == "darwin" {
			root = strings.ToLower(root)
		}
		if strings.HasPrefix(path, root) {
			return true
		}
	}
	return false
}
func resolveFederationLanding(ctx context.Context, o *db.FederationBundleOffer, g *db.AgentGroup, paths agentbundle.Paths, in *fedBundleImportRequest, teleport *db.FederationTeleport) (*federationLandingPlan, error) {
	p := &federationLandingPlan{Preview: federationLandingPreview{SourceCwd: paths.Cwd, SourceRepo: paths.RepoURL, Candidates: []federationLandingCandidate{}}}
	p.Preview.Reason = "none"
	repos, err := db.ListFederationRepos()
	if err != nil {
		return p, err
	}
	slices.SortFunc(repos, func(a, b db.FederationRepo) int {
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	plans := map[string]*db.FederationRepo{}
	explicitRef := teleport != nil && teleport.Intent.GitRef != ""
	for _, repo := range repos {
		if !jobRepoAllows(&repo, g.ID) {
			continue
		}
		if explicitRef {
			if teleport.Repo == nil || repo.ID != teleport.Repo.ID {
				continue
			}
		} else if key := federationRepoKey(paths.RepoURL); key == "" || key != federationRepoKey(repo.Definition.URL) {
			continue
		}
		if jobrepo.Revalidate(ctx, repo.Definition) != nil {
			continue
		}
		ref := ""
		if explicitRef {
			ref = teleport.Intent.GitRef
		} else {
			ref, err = jobrepo.Head(ctx, repo.Definition)
			if err != nil {
				continue
			}
		}
		root := filepath.Join(config.DataDir(), "federation", "landing-checkouts", o.Peer, o.Descriptor.ID)
		c := federationLandingCandidate{ID: "repo:" + repo.ID, Cwd: filepath.Join(root, "tree"), Reason: "repo_match", CheckoutRequired: true, Repo: &federationLandingRepo{repo.ID, repo.Name, ref}}
		p.Preview.Candidates = append(p.Preview.Candidates, c)
		copy := repo
		plans[c.ID] = &copy
	}
	add := func(id, path, reason string) {
		if path == "" {
			return
		}
		cwd, exists, err := federationLandingDirectory(path)
		if err != nil {
			return
		}
		p.Preview.Candidates = append(p.Preview.Candidates, federationLandingCandidate{ID: id, Cwd: cwd, Reason: reason, Exists: exists})
	}
	add("same_path", paths.Cwd, "same_path")
	add("group_default", g.DefaultCwd, "group_default")
	if authority := teleportLandingFromRequestContext(ctx); authority != nil {
		add("landing_policy", authority.record.Landing.Cwd, "landing_policy")
	}
	selectCandidate := func(c federationLandingCandidate) {
		p.Preview.federationLandingCandidate = c
		p.repo = plans[c.ID]
		if p.repo != nil {
			p.root = filepath.Dir(c.Cwd)
		}
	}
	if explicitRef && (in.Cwd != "" || in.KeepPaths || in.Landing != "" && !strings.HasPrefix(in.Landing, "repo:")) {
		return p, errors.New("git-ref requires its receiver allowlisted repository checkout")
	}
	if in.Cwd != "" {
		cwd, exists, err := federationLandingDirectory(in.Cwd)
		p.Preview.federationLandingCandidate = federationLandingCandidate{Cwd: cwd, Reason: "explicit", Exists: exists}
		return p, err
	}
	choice := in.Landing
	if in.KeepPaths {
		choice = "same_path"
	}
	if choice != "" {
		for _, c := range p.Preview.Candidates {
			if c.ID == choice {
				selectCandidate(c)
				return p, nil
			}
		}
		return p, errors.New("landing candidate is unavailable; reload the preview")
	}
	for _, c := range p.Preview.Candidates {
		if explicitRef && c.Repo == nil {
			continue
		}
		if !c.Exists && !c.CheckoutRequired || c.Reason == "same_path" && federationLandingBroad(c.Cwd) {
			continue
		}
		selectCandidate(c)
		return p, nil
	}
	return p, nil
}
func (p *federationLandingPlan) prepare(ctx context.Context, group int64) error {
	if p.repo == nil {
		cwd, exists, err := federationLandingDirectory(p.Preview.Cwd)
		if err != nil {
			return err
		}
		if !exists {
			return errors.New("landing directory is missing; choose an existing receiver directory")
		}
		p.Preview.Cwd = cwd
		return nil
	}
	current, err := db.GetFederationRepo(p.repo.ID)
	if err != nil || current == nil || current.Revision != p.repo.Revision || !jobRepoAllows(current, group) {
		return errors.New("landing repository allowlist changed; reload the preview")
	}
	if err := os.MkdirAll(filepath.Dir(p.root), 0700); err != nil {
		return err
	}
	checkout, err := jobrepo.Prepare(ctx, current.Definition, p.root, p.Preview.Repo.Ref)
	if err != nil {
		return err
	}
	current, err = db.GetFederationRepo(p.repo.ID)
	if err != nil || current == nil || current.Revision != p.repo.Revision || !jobRepoAllows(current, group) {
		_ = os.RemoveAll(p.root)
		return errors.New("landing repository allowlist changed")
	}
	p.checkout = checkout
	p.Preview.Cwd = checkout.Path
	p.Preview.Exists = true
	return nil
}
func teleportLandingFromRequestContext(ctx context.Context) *teleportLandingAuthority {
	a, _ := ctx.Value(teleportLandingContextKey{}).(*teleportLandingAuthority)
	return a
}
func federationLandingError(written string) string {
	return fmt.Sprintf("%s; choose --cwd or --landing from the receiver preview", written)
}

// Call only after the DB has positively released an undispatched reservation.
// An uncertain or dispatched launch may still be using its checkout.
func cleanupReleasedFederationLanding(o *db.FederationBundleOffer) {
	if !proto.ValidInstanceID(o.Peer) || !proto.ValidStreamID(o.Descriptor.ID) {
		return
	}
	_ = os.RemoveAll(filepath.Join(config.DataDir(), "federation", "landing-checkouts", o.Peer, o.Descriptor.ID))
	if row, err := db.GetFederationTeleport("in", o.Peer, o.Descriptor.ID); err == nil && row != nil {
		old := row.State
		row.State, row.TargetAgent = "pending", ""
		cleanupUnlaunchedTeleportCheckout(row)
		_, _ = db.TransitionFederationTeleport(*row, old)
	}
}
