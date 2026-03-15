package session

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type replayParams struct {
	File  string `pos:"true" help:"JSONL file to replay"`
	Delay string `short:"d" long:"delay" optional:"true" help:"Delay between hook callbacks (e.g. 100ms, 1s)"`
}

func ReplayCmd() *cobra.Command {
	return boa.CmdT[replayParams]{
		Use:         "replay",
		Short:       "Replay a recorded hook JSONL file to simulate a session",
		Long:        "Replays a JSONL file of hook inputs (recorded with record_hooks: true) by running hook-callback for each line. Useful for testing and debugging session state changes without running Claude.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *replayParams, cmd *cobra.Command, args []string) {
			var delay time.Duration
			if p.Delay != "" {
				var err error
				delay, err = time.ParseDuration(p.Delay)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: invalid delay %q: %v\n", p.Delay, err)
					os.Exit(1)
				}
			}

			if err := runReplay(p.File, delay); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runReplay(file string, delay time.Duration) error {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", file, err)
	}
	defer f.Close()

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to determine executable path: %w", err)
	}

	sessionID := strings.TrimSuffix(file, filepath.Ext(file))

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}

	// Create session state (starts as idle, waiting for user input)
	state := &SessionState{
		ID:          sessionID,
		TmuxSession: sessionID,
		PID:         0,
		Cwd:         cwd,
		ConvID:      "",
		Status:      StatusIdle,
		Created:     time.Now(),
		Updated:     time.Now(),
	}

	if err := SaveSessionState(state); err != nil {
		return fmt.Errorf("failed to save session state: %w", err)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MiB line buffer
	lineNum := 0

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		lineNum++

		fmt.Fprintf(os.Stderr, "[replay] line %d\n", lineNum)

		cmd := exec.Command(self, "session", "hook-callback")
		cmd.Env = append(os.Environ(), fmt.Sprintf("TCLAUDE_SESSION_ID=%s", sessionID), "TCLAUDE_REPLAY_MODE=true")
		cmd.Stdin = bytes.NewReader(line)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "[replay] hook-callback failed on line %d: %v\n", lineNum, err)
		}

		if delay > 0 {
			time.Sleep(delay)
		}
	}

	DeleteSessionState(sessionID)

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[replay] done (%d lines)\n", lineNum)
	return nil
}
