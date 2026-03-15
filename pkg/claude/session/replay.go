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
	cmd := boa.CmdT[replayParams]{
		Use:         "replay [file]",
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
	cmd.Args = cobra.MaximumNArgs(1)
	return cmd
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

	sessionId := strings.TrimSuffix(file, filepath.Ext(file))

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
		cmd.Env = append(os.Environ(), fmt.Sprintf("TCLAUDE_SESSION_ID=%s", sessionId), "TCLAUDE_REPLAY_MODE=true")
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

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "[replay] done (%d lines)\n", lineNum)
	return nil
}
