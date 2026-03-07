package conv

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

type CpParams struct {
	ConvID   string `pos:"true" help:"Conversation ID to copy"`
	DestPath string `pos:"true" help:"Destination directory path (real path, not Claude project path)"`
	Force    bool   `short:"f" help:"Force overwrite without confirmation"`
	Global   bool   `short:"g" help:"Search for conversation across all projects"`
}

func CpCmd() *cobra.Command {
	return boa.CmdT[CpParams]{
		Use:         "cp",
		Short:       "Copy a Claude conversation to another project directory",
		Long:        "Copy a Claude Code conversation from the current directory to another project directory.\nThe destination path should be a real filesystem path (e.g., /home/user/myproject).\nThe copied conversation gets a new UUID.",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *CpParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				global, _ := cmd.Flags().GetBool("global")
				return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveDefault
		},
		RunFunc: func(params *CpParams, cmd *cobra.Command, args []string) {
			exitCode := RunCp(params, os.Stdout, os.Stderr, os.Stdin)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

func RunCp(params *CpParams, stdout, stderr *os.File, stdin *os.File) int {
	// Extract just the ID from autocomplete format (e.g., "0459cd73_[myproject]_prompt..." -> "0459cd73")
	convID := clcommon.ExtractIDFromCompletion(params.ConvID)

	var srcEntry *SessionEntry
	var srcProjectPath string
	dstProjectPath := GetClaudeProjectPath(params.DestPath)

	if params.Global {
		// Search all projects
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			fmt.Fprintf(stderr, "Error reading projects directory: %v\n", err)
			return 1
		}

		for _, dirEntry := range entries {
			if !dirEntry.IsDir() {
				continue
			}
			projPath := projectsDir + "/" + dirEntry.Name()
			index, err := LoadSessionsIndex(projPath)
			if err != nil {
				continue
			}
			if found, _ := FindSessionByID(index, convID); found != nil {
				srcEntry = found
				srcProjectPath = projPath
				break
			}
		}
	} else {
		// Search current directory
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
			return 1
		}

		srcProjectPath = GetClaudeProjectPath(cwd)
		srcIndex, err := LoadSessionsIndex(srcProjectPath)
		if err != nil {
			fmt.Fprintf(stderr, "Error loading source sessions index: %v\n", err)
			return 1
		}

		srcEntry, _ = FindSessionByID(srcIndex, convID)
	}

	if srcEntry == nil {
		fmt.Fprintf(stderr, "Conversation %s not found\n", convID)
		if !params.Global {
			fmt.Fprintf(stderr, "Hint: use -g to search all projects\n")
		}
		return 1
	}

	// Check if destination project directory exists, create if not
	if _, err := os.Stat(dstProjectPath); os.IsNotExist(err) {
		if err := os.MkdirAll(dstProjectPath, 0700); err != nil {
			fmt.Fprintf(stderr, "Error creating destination project directory: %v\n", err)
			return 1
		}
	}

	// Load destination index
	dstIndex, err := LoadSessionsIndex(dstProjectPath)
	if err != nil {
		fmt.Fprintf(stderr, "Error loading destination sessions index: %v\n", err)
		return 1
	}

	// Generate new UUID for the copy
	newConvID := uuid.New().String()
	oldConvID := srcEntry.SessionID

	// Source and destination files
	srcConvFile := filepath.Join(srcProjectPath, oldConvID+".jsonl")
	dstConvFile := filepath.Join(dstProjectPath, newConvID+".jsonl")

	// Check if destination file already exists (shouldn't with new UUID, but be safe)
	if _, err := os.Stat(dstConvFile); err == nil {
		if !params.Force {
			fmt.Fprintf(stdout, "Destination file already exists (unlikely UUID collision!).\n")
			fmt.Fprintf(stdout, "Overwrite? [y/N]: ")
			reader := bufio.NewReader(stdin)
			response, _ := reader.ReadString('\n')
			response = strings.TrimSpace(strings.ToLower(response))
			if response != "y" && response != "yes" {
				fmt.Fprintf(stdout, "Aborted.\n")
				return 0
			}
		}
	}

	// Copy and transform conversation file (update sessionId references)
	if err := CopyConversationFile(srcConvFile, dstConvFile, oldConvID, newConvID); err != nil {
		fmt.Fprintf(stderr, "Error copying conversation file: %v\n", err)
		return 1
	}

	// Copy conversation directory if it exists (with new name)
	srcConvDir := filepath.Join(srcProjectPath, oldConvID)
	dstConvDir := filepath.Join(dstProjectPath, newConvID)
	if info, err := os.Stat(srcConvDir); err == nil && info.IsDir() {
		if err := CopyDir(srcConvDir, dstConvDir); err != nil {
			fmt.Fprintf(stderr, "Error copying conversation directory: %v\n", err)
			return 1
		}
	}

	// Update file info for destination
	dstInfo, err := os.Stat(dstConvFile)
	if err != nil {
		fmt.Fprintf(stderr, "Error getting destination file info: %v\n", err)
		return 1
	}

	// Create new entry for destination with new UUID
	now := time.Now().UTC().Format(time.RFC3339)
	newEntry := SessionEntry{
		SessionID:    newConvID,
		FullPath:     dstConvFile,
		FileMtime:    dstInfo.ModTime().UnixMilli(),
		FirstPrompt:  srcEntry.FirstPrompt,
		Summary:      srcEntry.Summary,
		CustomTitle:  srcEntry.CustomTitle,
		MessageCount: srcEntry.MessageCount,
		Created:      now, // New creation time for the copy
		Modified:     now,
		GitBranch:    srcEntry.GitBranch,
		ProjectPath:  params.DestPath,
		IsSidechain:  srcEntry.IsSidechain,
	}

	// Add to destination index
	dstIndex.Entries = append(dstIndex.Entries, newEntry)

	// Save destination index
	if err := SaveSessionsIndex(dstProjectPath, dstIndex); err != nil {
		fmt.Fprintf(stderr, "Error saving destination sessions index: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Copied conversation %s -> %s in %s\n", oldConvID[:8], newConvID[:8], params.DestPath)
	return 0
}
