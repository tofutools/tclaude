// Package harnessops runs fixed official package recipes as the daemon user.
package harnessops

import (
	"context"
	"fmt"
	"github.com/tofutools/tclaude/pkg/common/executil"
	"github.com/tofutools/tclaude/pkg/common/harnesspath"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Recipe struct {
	Harness        string `json:"harness"`
	Package        string `json:"package"`
	Channel        string `json:"channel"`
	InstallCommand string `json:"install_command"`
	UpdateCommand  string `json:"update_command"`
	// UpdateSource says what runs the update: "builtin" (the harness's own
	// updater), "npm", or "custom" (the operator's updates.harnesses override).
	UpdateSource  string `json:"update_source"`
	Documentation string `json:"documentation"`
	// builtin is the harness's own update subcommand. A harness updates
	// itself through it when it has one: it knows how it was installed
	// (native installer, npm, Homebrew, …) where tclaude can only guess.
	builtin []string
}

var recipes = []Recipe{
	{"claude", "@anthropic-ai/claude-code", "latest", "npm install -g @anthropic-ai/claude-code@latest", "claude update", "builtin", "https://code.claude.com/docs/en/setup", []string{"update"}},
	{"codex", "@openai/codex", "latest", "npm install -g @openai/codex@latest", "codex update", "builtin", "https://developers.openai.com/codex/cli/", []string{"update"}},
	{"opencode", "opencode-ai", "latest", "npm install -g opencode-ai@latest", "opencode upgrade", "builtin", "https://opencode.ai/docs/", []string{"upgrade"}},
	{"copilot", "@github/copilot", "latest", "npm install -g @github/copilot@latest", "copilot update", "builtin", "https://docs.github.com/en/copilot/how-tos/copilot-cli/set-up-copilot-cli/install-copilot-cli", []string{"update"}},
	// Gemini CLI has no update subcommand; it is updated through npm.
	{"gemini", "@google/gemini-cli", "latest", "npm install -g @google/gemini-cli@latest", "npm install -g @google/gemini-cli@latest", "npm", "https://geminicli.com/docs/get-started/installation/", nil},
	{"pi", "@earendil-works/pi-coding-agent", "latest", "npm install -g @earendil-works/pi-coding-agent@latest", "pi update --self", "builtin", "https://github.com/earendil-works/pi", []string{"update", "--self"}},
}

func Recipes() []Recipe { return append([]Recipe{}, recipes...) }
func recipe(name string) (Recipe, error) {
	for _, r := range recipes {
		if r.Harness == name {
			return r, nil
		}
	}
	return Recipe{}, fmt.Errorf("unsupported harness")
}
func UserPrefix(home string) string    { return harnesspath.UserPrefix(home) }
func BinaryDir(home string) string     { return harnesspath.BinaryDir(home) }
func EnableUserPath(home string) error { return harnesspath.Enable(home) }

type Command struct {
	Path string
	Args []string
	Env  []string
	// Recipe names what runs, for the job result and audit.
	Recipe string
}
type ManualRequired struct{ Command string }

func (e *ManualRequired) Error() string { return "manual install/update required: " + e.Command }
func Plan(ctx context.Context, home, action, name string) (Command, error) {
	r, err := recipe(name)
	if err != nil {
		return Command{}, err
	}
	if os.Geteuid() == 0 {
		return Command{}, &ManualRequired{r.InstallCommand}
	}
	if action == "update" && len(r.builtin) > 0 {
		installed, err := exec.LookPath(name)
		if err != nil {
			return Command{}, fmt.Errorf("harness is not installed")
		}
		return Command{Path: installed, Args: r.builtin, Env: os.Environ(), Recipe: r.UpdateCommand}, nil
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		return Command{}, &ManualRequired{r.InstallCommand}
	}
	prefix := UserPrefix(home)
	if action == "update" {
		installed, err := exec.LookPath(name)
		if err != nil {
			return Command{}, fmt.Errorf("harness is not installed")
		}
		resolved, err := filepath.EvalSymlinks(installed)
		if err != nil {
			return Command{}, err
		}
		owned, err := filepath.Abs(prefix)
		if err != nil {
			return Command{}, err
		}
		if !strings.HasPrefix(resolved, owned+string(os.PathSeparator)) {
			// Recognize an existing global npm install by the real package path; use
			// its own prefix, rather than silently replacing a different install.
			suffix := string(os.PathSeparator) + filepath.Join("lib", "node_modules", r.Package) + string(os.PathSeparator)
			i := strings.Index(resolved, suffix)
			if i >= 0 {
				prefix = resolved[:i]
			} else {
				return Command{}, &ManualRequired{r.UpdateCommand}
			}
		}
	}
	check := prefix
	for {
		if _, err := os.Stat(check); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return Command{}, &ManualRequired{r.InstallCommand}
		}
		parent := filepath.Dir(check)
		if parent == check {
			return Command{}, &ManualRequired{r.InstallCommand}
		}
		check = parent
	}
	if unix.Access(check, unix.W_OK) != nil {
		return Command{}, &ManualRequired{r.InstallCommand}
	}
	return Command{Path: npm, Args: []string{"install", "--global", "--prefix", prefix, "--no-audit", "--no-fund", r.Package + "@latest"}, Env: os.Environ(), Recipe: "npm:" + r.Package + "@latest"}, nil
}

// CustomUpdate is the operator's own update command for a harness, from this
// node's updates.harnesses config; it runs with /bin/sh -c.
func CustomUpdate(command string) Command {
	return Command{Path: "/bin/sh", Args: []string{"-c", command}, Env: os.Environ(), Recipe: "custom"}
}

func Execute(ctx context.Context, c Command) error {
	cmd := executil.CommandContextWithGrace(ctx, time.Second, c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.WaitDelay = time.Second
	// Vendor output may contain credential-bearing proxy URLs. Store only fixed
	// phase messages and the exit status, never stdout/stderr or the environment.
	if err := cmd.Run(); err != nil {
		if c.Recipe == "custom" {
			return fmt.Errorf("custom update command failed: %w", err)
		}
		return fmt.Errorf("official recipe failed: %w", err)
	}
	return nil
}
