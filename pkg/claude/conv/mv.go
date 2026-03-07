package conv

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/spf13/cobra"
)

type MvParams struct {
	ConvID   string `pos:"true" help:"Conversation ID to move"`
	DestPath string `pos:"true" help:"Destination directory path (real path, not Claude project path)"`
	Force    bool   `short:"f" help:"Force overwrite without confirmation"`
	Global   bool   `short:"g" help:"Search for conversation across all projects"`
}

func MvCmd() *cobra.Command {
	return boa.CmdT[MvParams]{
		Use:         "mv",
		Short:       "Move a Claude conversation to another project directory",
		Long:        "Move a Claude Code conversation from the current directory to another project directory.\nThe destination path should be a real filesystem path (e.g., /home/user/myproject).",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *MvParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				global, _ := cmd.Flags().GetBool("global")
				return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveDefault
		},
		RunFunc: func(params *MvParams, cmd *cobra.Command, args []string) {
			exitCode := RunMv(params, os.Stdout, os.Stderr, os.Stdin)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

func RunMv(params *MvParams, stdout, stderr *os.File, stdin *os.File) int {
	// Extract just the ID from autocomplete format (e.g., "0459cd73_[tofu_claude]_prompt..." -> "0459cd73")
	convID := clcommon.ExtractIDFromCompletion(params.ConvID)

	var srcEntry *SessionEntry
	var srcProjectPath string
	var srcIndex *SessionsIndex
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
				srcIndex = index
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
		var err2 error
		srcIndex, err2 = LoadSessionsIndex(srcProjectPath)
		if err2 != nil {
			fmt.Fprintf(stderr, "Error loading source sessions index: %v\n", err2)
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

	// Use the full session ID from the found entry
	convID = srcEntry.SessionID

	// Check if conversation already exists in destination
	existingEntry, _ := FindSessionByID(dstIndex, convID)
	if existingEntry != nil {
		if !params.Force {
			fmt.Fprintf(stdout, "Conversation %s already exists in destination.\n", convID[:8])
			fmt.Fprintf(stdout, "Overwrite? [y/N]: ")
			reader := bufio.NewReader(stdin)
			response, _ := reader.ReadString('\n')
			response = strings.TrimSpace(strings.ToLower(response))
			if response != "y" && response != "yes" {
				fmt.Fprintf(stdout, "Aborted.\n")
				return 0
			}
		}
		// Remove existing conversation files from destination
		existingFile := filepath.Join(dstProjectPath, convID+".jsonl")
		existingDir := filepath.Join(dstProjectPath, convID)
		os.Remove(existingFile)
		os.RemoveAll(existingDir)
		// Remove existing entry from destination index
		RemoveSessionByID(dstIndex, convID)
	}

	// Move conversation file
	srcConvFile := filepath.Join(srcProjectPath, convID+".jsonl")
	dstConvFile := filepath.Join(dstProjectPath, convID+".jsonl")

	if err := os.Rename(srcConvFile, dstConvFile); err != nil {
		// If rename fails (e.g., cross-device), fall back to copy+delete
		if err := CopyFile(srcConvFile, dstConvFile); err != nil {
			fmt.Fprintf(stderr, "Error moving conversation file: %v\n", err)
			return 1
		}
		os.Remove(srcConvFile)
	}

	// Move conversation directory if it exists
	srcConvDir := filepath.Join(srcProjectPath, convID)
	dstConvDir := filepath.Join(dstProjectPath, convID)
	if info, err := os.Stat(srcConvDir); err == nil && info.IsDir() {
		if err := os.Rename(srcConvDir, dstConvDir); err != nil {
			// Fall back to copy+delete
			if err := CopyDir(srcConvDir, dstConvDir); err != nil {
				fmt.Fprintf(stderr, "Error moving conversation directory: %v\n", err)
				return 1
			}
			os.RemoveAll(srcConvDir)
		}
	}

	// Update file info for destination
	dstInfo, err := os.Stat(dstConvFile)
	if err != nil {
		fmt.Fprintf(stderr, "Error getting destination file info: %v\n", err)
		return 1
	}

	// Create new entry for destination
	newEntry := SessionEntry{
		SessionID:    srcEntry.SessionID,
		FullPath:     dstConvFile,
		FileMtime:    dstInfo.ModTime().UnixMilli(),
		FirstPrompt:  srcEntry.FirstPrompt,
		Summary:      srcEntry.Summary,
		CustomTitle:  srcEntry.CustomTitle,
		MessageCount: srcEntry.MessageCount,
		Created:      srcEntry.Created,
		Modified:     srcEntry.Modified,
		GitBranch:    srcEntry.GitBranch,
		ProjectPath:  params.DestPath, // Update to new project path
		IsSidechain:  srcEntry.IsSidechain,
	}

	// Add to destination index
	dstIndex.Entries = append(dstIndex.Entries, newEntry)

	// Save destination index
	if err := SaveSessionsIndex(dstProjectPath, dstIndex); err != nil {
		fmt.Fprintf(stderr, "Error saving destination sessions index: %v\n", err)
		return 1
	}

	// Remove from source index
	RemoveSessionByID(srcIndex, convID)

	// Save source index
	if err := SaveSessionsIndex(srcProjectPath, srcIndex); err != nil {
		fmt.Fprintf(stderr, "Error saving source sessions index: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Moved conversation %s to %s\n", convID[:8], params.DestPath)
	return 0
}
