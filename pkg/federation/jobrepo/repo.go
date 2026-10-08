// Package jobrepo prepares isolated checkouts of operator-allowlisted Git
// repositories. Remote inputs select a ref, never a local path or Git config.
package jobrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/tofutools/tclaude/pkg/common/executil"
)

var repoName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var objectID = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
var sshHost = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`)
var sshUser = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

type Identity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}
type Definition struct {
	URL      string   `json:"url"`
	Clone    string   `json:"clone"`
	GitDir   string   `json:"git_dir"`
	Identity Identity `json:"identity"`
	Groups   []int64  `json:"groups"`
}
type Checkout struct {
	Root       string
	Path       string
	Commit     string
	Resolution string
}

func ValidName(name string) bool { return repoName.MatchString(name) }

// ValidateURL admits ordinary Git transports only. A file URL can be explicitly
// configured by the operator; a remote requester still cannot choose a path.
// Reject inline HTTPS credentials, helpers, option-like SSH hosts and rewrites.
func ValidateURL(raw string) error {
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\x00\r\n\t ") {
		return errors.New("invalid repository URL")
	}
	if !strings.Contains(raw, "://") {
		left, path, ok := strings.Cut(raw, ":")
		if !ok || path == "" || strings.HasPrefix(path, "-") || left == "file" {
			return errors.New("use an HTTPS, SSH or explicit file repository URL")
		}
		user, host, hasUser := strings.Cut(left, "@")
		if !hasUser {
			host = left
		} else if !sshUser.MatchString(user) {
			return errors.New("invalid SSH user")
		}
		if !sshHost.MatchString(host) {
			return errors.New("invalid SSH host")
		}
		return nil
	}
	u, e := url.Parse(raw)
	if e != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path == "" || strings.ContainsAny(u.Path, "\x00\r\n") {
		return errors.New("invalid repository URL")
	}
	switch u.Scheme {
	case "https":
		if u.Hostname() == "" || u.User != nil {
			return errors.New("HTTPS repository URLs must not contain credentials")
		}
	case "ssh":
		if !sshHost.MatchString(u.Hostname()) {
			return errors.New("invalid SSH host")
		}
		if u.User != nil {
			if _, set := u.User.Password(); set || !sshUser.MatchString(u.User.Username()) {
				return errors.New("invalid SSH user")
			}
		}
	case "file":
		if u.Host != "" && u.Host != "localhost" {
			return errors.New("file repository URL must be local")
		}
		if u.User != nil || !filepath.IsAbs(u.Path) {
			return errors.New("file repository URL must be absolute")
		}
	default:
		return errors.New("unsupported repository transport")
	}
	return nil
}
func fileIdentity(path string) (Identity, error) {
	info, e := os.Lstat(path)
	if e != nil {
		return Identity{}, e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return Identity{}, errors.New("repository path must be a directory")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, errors.New("filesystem identity unavailable")
	}
	return Identity{Device: uint64(st.Dev), Inode: uint64(st.Ino)}, nil
}
func gitEnvironment() []string {
	env := []string{}
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		env = append(env, v)
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
}
func git(ctx context.Context, dir string, args ...string) (string, error) {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "submodule.recurse=false", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always", "-c", "protocol.file.allow=always"}
	cmd := executil.CommandContextWithGrace(ctx, 0, "git", append(base, args...)...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Env = gitEnvironment()
	// Git errors may contain repository credentials/configuration; callers expose
	// a bounded operation name, never raw stderr from a transport process.
	out, e := cmd.Output()
	if e != nil {
		return "", fmt.Errorf("git %s failed", args[0])
	}
	return strings.TrimSpace(string(out)), nil
}
func Inspect(ctx context.Context, rawURL, path string, groups []int64) (Definition, error) {
	d := Definition{URL: rawURL, Groups: append([]int64{}, groups...)}
	if e := ValidateURL(rawURL); e != nil {
		return d, e
	}
	if len(groups) == 0 {
		return d, errors.New("at least one allowed receiving group is required")
	}
	for _, g := range groups {
		if g <= 0 {
			return d, errors.New("invalid receiving group")
		}
	}
	absolute, e := filepath.Abs(path)
	if e != nil {
		return d, e
	}
	d.Clone, e = filepath.EvalSymlinks(absolute)
	if e != nil {
		return d, e
	}
	if _, e = fileIdentity(d.Clone); e != nil {
		return d, e
	}
	gd, e := git(ctx, d.Clone, "rev-parse", "--absolute-git-dir")
	if e != nil {
		return d, errors.New("configured clone is not a Git repository")
	}
	d.GitDir, e = filepath.EvalSymlinks(gd)
	if e != nil {
		return d, e
	}
	d.Identity, e = fileIdentity(d.GitDir)
	return d, e
}
func Revalidate(ctx context.Context, d Definition) error {
	if e := ValidateURL(d.URL); e != nil {
		return e
	}
	clone, e := filepath.EvalSymlinks(d.Clone)
	if e != nil || clone != d.Clone {
		return errors.New("configured clone path changed")
	}
	gd, e := git(ctx, clone, "rev-parse", "--absolute-git-dir")
	if e != nil {
		return errors.New("configured clone unavailable")
	}
	gd, e = filepath.EvalSymlinks(gd)
	if e != nil || gd != d.GitDir {
		return errors.New("configured Git directory changed")
	}
	id, e := fileIdentity(gd)
	if e != nil || id != d.Identity {
		return errors.New("configured repository identity changed")
	}
	return nil
}
func NormalizeRef(ctx context.Context, ref string) (string, error) {
	if len(ref) == 0 || len(ref) > 256 || strings.ContainsAny(ref, "\x00\r\n\t ") || strings.HasPrefix(ref, "-") {
		return "", errors.New("invalid Git ref")
	}
	if objectID.MatchString(ref) {
		return strings.ToLower(ref), nil
	}
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	cmd := exec.CommandContext(ctx, "git", "check-ref-format", ref)
	cmd.Env = gitEnvironment()
	if cmd.Run() != nil {
		return "", errors.New("invalid Git ref")
	}
	return ref, nil
}

// Prepare creates a fresh per-job repository rather than checking out through
// the configured clone's hooks/filters. Its .git stays inside the worker's cwd;
// the worker needs no access to private daemon state or the operator's clone.
func Prepare(ctx context.Context, d Definition, root, ref string) (*Checkout, error) {
	if e := Revalidate(ctx, d); e != nil {
		return nil, e
	}
	safe, e := NormalizeRef(ctx, ref)
	if e != nil {
		return nil, e
	}
	if !filepath.IsAbs(root) {
		return nil, errors.New("job checkout root must be absolute")
	}
	// The parent is daemon-selected and the final root must be newly created.
	if e = os.Mkdir(root, 0700); e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(root)
		}
	}()
	path := filepath.Join(root, "tree")
	if e = os.Mkdir(path, 0700); e != nil {
		return nil, e
	}
	templates := filepath.Join(root, "empty-templates")
	if e = os.Mkdir(templates, 0700); e != nil {
		return nil, e
	}
	if _, e = git(ctx, path, "init", "--template="+templates, "."); e != nil {
		return nil, e
	}
	// Read only object data from the verified clone, including common objects
	// for a linked Git worktree. Never copy its configuration, hooks or refs.
	objects, e := git(ctx, d.Clone, "rev-parse", "--git-path", "objects")
	if e != nil {
		return nil, e
	}
	if !filepath.IsAbs(objects) {
		objects = filepath.Join(d.Clone, objects)
	}
	objects, e = filepath.EvalSymlinks(objects)
	if e != nil || strings.ContainsAny(objects, "\r\n") {
		return nil, errors.New("configured clone object directory unavailable")
	}
	alternate := filepath.Join(path, ".git", "objects", "info", "alternates")
	if e = os.WriteFile(alternate, []byte(objects+"\n"), 0600); e != nil {
		return nil, e
	}
	// Borrow the clone's shallow boundary as object metadata, so repack does
	// not traverse parents that a deliberately shallow clone does not contain.
	shallow, e := git(ctx, d.Clone, "rev-parse", "--git-path", "shallow")
	if e != nil {
		return nil, e
	}
	if !filepath.IsAbs(shallow) {
		shallow = filepath.Join(d.Clone, shallow)
	}
	f, e := os.Open(shallow)
	if e == nil {
		raw, readErr := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		_ = f.Close()
		if readErr != nil || len(raw) > 1<<20 {
			return nil, errors.New("invalid clone shallow boundary")
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			if !objectID.MatchString(line) {
				return nil, errors.New("invalid clone shallow boundary")
			}
		}
		if e = os.WriteFile(filepath.Join(path, ".git", "shallow"), raw, 0600); e != nil {
			return nil, e
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	// A new Git directory has no configured filters, include files, hooks or
	// remotes. Fetch transfers only objects not already available in the clone.
	resolution := "fetched"
	if _, e = git(ctx, path, "fetch", "--no-tags", "--no-recurse-submodules", "--", d.URL, safe+":refs/tclaude/job"); e != nil {
		// A branch must be freshly fetched; only a content-pinned SHA can safely
		// use local objects after a transport/authentication failure.
		if !objectID.MatchString(safe) {
			return nil, e
		}
		commit, localErr := git(ctx, path, "rev-parse", "--verify", safe+"^{commit}")
		if localErr != nil || commit != safe {
			return nil, errors.New("pinned commit unavailable in local clone after fetch failed")
		}
		if _, e = git(ctx, path, "update-ref", "refs/tclaude/job", commit); e != nil {
			return nil, e
		}
		resolution = "resolved from local clone (fetch failed)"
	}
	commit, e := git(ctx, path, "rev-parse", "--verify", "refs/tclaude/job^{commit}")
	if e != nil || !objectID.MatchString(commit) {
		return nil, errors.New("requested ref does not resolve to a commit")
	}
	if _, e = git(ctx, path, "checkout", "--detach", commit, "--"); e != nil {
		return nil, e
	}
	// Materialize borrowed objects so the worker needs no access to the clone
	// and later clone maintenance cannot invalidate the running checkout.
	if _, e = git(ctx, path, "repack", "-a", "-d"); e != nil {
		return nil, e
	}
	if e = os.Remove(alternate); e != nil {
		return nil, e
	}
	if e = Revalidate(ctx, d); e != nil {
		return nil, e
	}
	ok = true
	return &Checkout{Root: root, Path: path, Commit: strings.ToLower(commit), Resolution: resolution}, nil
}
