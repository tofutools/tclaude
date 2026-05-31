package workgraphcli

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/workgraph"
	"github.com/tofutools/tclaude/pkg/common"
)

// install resolves an external workgraph source (a dir: path or a git: repo) and
// copies it INTO the user templates dir (~/.tclaude/workgraphs/<name>), so it
// becomes a stable, offline, locally-editable `user:<name>` template.
//
// The fetch is entirely the workgraph package's job — workgraph.Resolve loads a
// dir: source and clones+caches a git: source (with all of JOH-12's hardening:
// no leading-dash url/ref, fenced args, ext-transport disabled, subpath +
// symlink-escape guards, per-clone deadline). This verb is a thin wrapper over
// that resolver plus a copy-in; it never shells out to git itself.
//
// The copy is deliberately symlink-rejecting (unlike a plain recursive copy): an
// external template is third-party data, and installing PERSISTS its files, so a
// symlinked config file (which the resolver would otherwise read through, per
// fetch.go's documented residual) must not be baked into the user dir where a
// later load would follow it to an arbitrary local path.

type installParams struct {
	Src   string `pos:"true" help:"Source: dir:<path> or git:<url>[@ref][#subpath] (a bare path is treated as dir:)"`
	Name  string `long:"name" optional:"true" help:"Install under this name (default: the template's own name)"`
	Force bool   `long:"force" help:"Overwrite an existing user template of the same name"`
	JSON  bool   `long:"json" help:"Output JSON"`
}

func installCmd() *cobra.Command {
	return boa.CmdT[installParams]{
		Use:   "install",
		Short: "Install an external template (dir:/git:) into the user workgraphs dir",
		Long: "Resolve an external workgraph source and copy it into ~/.tclaude/workgraphs,\n" +
			"making it a stable, offline `user:<name>` template.\n\n" +
			"  tclaude workgraph install dir:./my-template\n" +
			"  tclaude workgraph install git:https://example.com/repo#workgraphs/foo --name foo\n\n" +
			"The git fetch/cache is the workgraph resolver's job; this verb only copies in.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *installParams, _ *cobra.Command, _ []string) {
			os.Exit(runInstall(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runInstall(p *installParams, stdout, stderr io.Writer) int {
	src := normalizeInstallSrc(p.Src)

	// Resolve: loads a dir: template, or clones+caches a git: repo and loads the
	// template at its subpath. This is the only "fetch" — done by the resolver.
	tmpl, err := workgraph.Resolve(src, projectDirs()...)
	if err != nil {
		// workgraph.Resolve returns untyped errors, so a malformed external source
		// (bad git url/ref, an escaping subpath) can't be cleanly separated from a
		// genuine miss here — both collapse to rcNotFound. A typed-error split
		// would have to live in the workgraph package.
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	// An embedded example has no on-disk dir to copy from (and is already
	// available as example:<name>); nothing to install.
	if tmpl.Dir == "" {
		fmt.Fprintf(stderr, "Error: %q has no source directory to install from (embedded example?)\n", src)
		return rcInvalidArg
	}

	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = tmpl.Name
	}
	if !validInstallName(name) {
		fmt.Fprintf(stderr, "Error: invalid install name %q (must be a single path segment; pass --name)\n", name)
		return rcInvalidArg
	}

	ud := workgraph.UserDir()
	if ud == "" {
		fmt.Fprintln(stderr, "Error: cannot determine the user workgraphs dir (no home directory)")
		return rcIOFailure
	}
	dst := filepath.Join(ud, name)
	if _, statErr := os.Stat(dst); statErr == nil && !p.Force {
		fmt.Fprintf(stderr, "Error: user template %q already exists at %s (use --name or --force)\n", name, dst)
		return rcInvalidArg
	}

	if err := installCopyDir(tmpl.Dir, dst, p.Force); err != nil {
		fmt.Fprintf(stderr, "Error: install: %v\n", err)
		return rcIOFailure
	}

	installedRef := string(workgraph.SourceUser) + ":" + name
	if p.JSON {
		return writeJSON(stdout, stderr, map[string]any{
			"ref":        installedRef,
			"name":       name,
			"source":     src,
			"dir":        dst,
			"node_count": len(tmpl.Nodes),
		})
	}
	fmt.Fprintf(stdout, "installed %s from %s (%d nodes) → %s\n",
		installedRef, src, len(tmpl.Nodes), dst)
	return rcOK
}

// normalizeInstallSrc treats a source with no recognised "<source>:" prefix as a
// local directory path (the common `install ./my-template` / `install /abs`
// form), so it resolves through the dir: loader. A recognised prefix
// (dir:/git:/project:/user:/example:) is passed through unchanged.
func normalizeInstallSrc(src string) string {
	if pre, _, found := strings.Cut(src, ":"); found {
		switch workgraph.Source(pre) {
		case workgraph.SourceDir, workgraph.SourceGit,
			workgraph.SourceProject, workgraph.SourceUser, workgraph.SourceExample:
			return src
		}
	}
	return string(workgraph.SourceDir) + ":" + src
}

// validInstallName rejects a destination name that isn't a single safe path
// segment — so a template's own name (or a bad --name) can't escape the user
// workgraphs dir.
func validInstallName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}

// installCopyDir copies the template tree at src into dst, rejecting any symlink
// (an external template is untrusted and these files persist). The copy lands in
// a sibling temp dir first and is renamed into place, so a failure never leaves
// a half-installed template; with force, an existing dst is replaced atomically.
func installCopyDir(src, dst string, force bool) error {
	if _, err := os.Stat(dst); err == nil && !force {
		return fmt.Errorf("%s already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmp)
		}
	}()

	if err := copyTreeNoSymlinks(src, tmp); err != nil {
		return err
	}
	// Publish atomically: drop any existing dst, then rename our staged copy in.
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	committed = true
	return nil
}

// copyTreeNoSymlinks recursively copies regular files + directories from src to
// dst (already created), returning an error on the first symlink or other
// irregular entry. dst is assumed to exist (os.MkdirTemp made it).
func copyTreeNoSymlinks(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to install: %s is a symlink", rel)
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel == "." {
				// dst itself already exists (os.MkdirTemp made it, 0700); we
				// deliberately don't copy src's perms onto it — owner-only under
				// the per-user config dir is fine. Nested dirs get src's perms.
				return nil
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to install: %s is not a regular file", rel)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
