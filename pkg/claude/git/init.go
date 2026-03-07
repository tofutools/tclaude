package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type InitParams struct {
	RepoURL string `pos:"true" help:"Git repository URL to sync with"`
	Reset   bool   `long:"reset" help:"Delete existing sync directory and reinitialize"`
}

func InitCmd() *cobra.Command {
	return boa.CmdT[InitParams]{
		Use:         "init <repo-url>",
		Short:       "Initialize git sync for Claude conversations",
		Long:        "Set up ~/.claude/projects_sync as a git repository for syncing conversations.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(params *InitParams, cmd *cobra.Command, args []string) {
			if err := runInit(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runInit(params *InitParams) error {
	syncDir := SyncDir()

	// Handle reset
	if params.Reset {
		if IsInitialized() {
			fmt.Printf("Removing existing sync directory: %s\n", syncDir)
			if err := os.RemoveAll(syncDir); err != nil {
				return fmt.Errorf("failed to remove sync directory: %w", err)
			}
		}
	} else if IsInitialized() {
		return fmt.Errorf("sync already initialized at %s\nUse --reset to reinitialize, or 'tclaude git status' to check status", syncDir)
	}

	// Create sync directory if it doesn't exist
	if err := os.MkdirAll(syncDir, 0755); err != nil {
		return fmt.Errorf("failed to create sync directory: %w", err)
	}

	// Check if remote repo has content by trying to clone
	fmt.Printf("Checking remote repository...\n")

	// Try to clone into a temp directory first to check if repo has content
	tempDir, err := os.MkdirTemp("", "tclaude-sync-")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	cloneCmd := exec.Command("git", "clone", "--depth=1", params.RepoURL, tempDir)
	cloneOutput, cloneErr := cloneCmd.CombinedOutput()

	if cloneErr != nil {
		// Clone failed - might be empty repo or invalid URL
		// Try to init locally and add remote
		fmt.Printf("Remote appears empty or new. Initializing fresh sync...\n")

		// Init git repo
		initCmd := exec.Command("git", "init")
		initCmd.Dir = syncDir
		if output, err := initCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git init failed: %s\n%w", output, err)
		}

		// Add remote
		remoteCmd := exec.Command("git", "remote", "add", "origin", params.RepoURL)
		remoteCmd.Dir = syncDir
		if output, err := remoteCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git remote add failed: %s\n%w", output, err)
		}

		// Create initial .gitignore
		gitignore := "# Temporary files\n*.tmp\n*.swp\n"
		if err := os.WriteFile(syncDir+"/.gitignore", []byte(gitignore), 0644); err != nil {
			return fmt.Errorf("failed to create .gitignore: %w", err)
		}

		fmt.Printf("Initialized empty sync repository at %s\n", syncDir)
		fmt.Printf("Run 'tclaude git sync' to sync your conversations.\n")
		return nil
	}

	// Clone succeeded - repo has content
	// Check if there are files (not just .git)
	entries, _ := os.ReadDir(tempDir)
	hasContent := false
	for _, e := range entries {
		if e.Name() != ".git" {
			hasContent = true
			break
		}
	}

	if hasContent {
		fmt.Printf("Remote has existing conversations. Cloning...\n")
		// Remove our empty sync dir and clone properly
		os.RemoveAll(syncDir)

		fullClone := exec.Command("git", "clone", params.RepoURL, syncDir)
		if output, err := fullClone.CombinedOutput(); err != nil {
			return fmt.Errorf("git clone failed: %s\n%w", output, err)
		}

		fmt.Printf("Cloned existing conversations to %s\n", syncDir)
		fmt.Printf("Run 'tclaude git sync' to merge with your local conversations.\n")
	} else {
		// Empty repo - init locally
		fmt.Printf("Remote is empty. Initializing fresh sync...\n")

		initCmd := exec.Command("git", "init")
		initCmd.Dir = syncDir
		if output, err := initCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git init failed: %s\n%w", output, err)
		}

		remoteCmd := exec.Command("git", "remote", "add", "origin", params.RepoURL)
		remoteCmd.Dir = syncDir
		if output, err := remoteCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git remote add failed: %s\n%w", output, err)
		}

		fmt.Printf("Initialized sync repository at %s\n", syncDir)
		fmt.Printf("Run 'tclaude git sync' to sync your conversations.\n")
	}

	// Create symlink for sync_config.json if it exists in sync dir
	setupConfigSymlink(syncDir)

	_ = cloneOutput // Suppress unused warning
	return nil
}

// setupConfigSymlink creates a symlink from ~/.claude/sync_config.json to the sync dir
func setupConfigSymlink(syncDir string) {
	syncConfig := filepath.Join(syncDir, "sync_config.json")
	if _, err := os.Stat(syncConfig); os.IsNotExist(err) {
		return // No config in sync dir
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	localConfig := filepath.Join(home, ".claude", "sync_config.json")

	// Check if local config already exists
	if info, err := os.Lstat(localConfig); err == nil {
		// If it's already a symlink pointing to the right place, we're done
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(localConfig)
			if target == syncConfig {
				return
			}
		}
		// Remove existing file/symlink to replace it
		os.Remove(localConfig)
	}

	// Create symlink
	if err := os.Symlink(syncConfig, localConfig); err != nil {
		fmt.Printf("Note: Could not create config symlink: %v\n", err)
		return
	}

	fmt.Printf("Created symlink: %s -> %s\n", localConfig, syncConfig)
}
