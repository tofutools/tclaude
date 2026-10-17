package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type breakGlassParams struct {
	Args    []string `pos:"true" optional:"true" help:"Command and arguments (after --)"`
	Shell   string   `long:"command" short:"c" optional:"true" help:"Run a shell string using /bin/sh -c"`
	Cwd     string   `long:"cwd" optional:"true" help:"Working directory on the daemon host"`
	Timeout string   `long:"timeout" default:"10m" help:"Command timeout"`
	Env     []string `long:"env" optional:"true" help:"Environment override K=V (repeatable)"`
	Stdin   bool     `long:"stdin" help:"Forward standard input"`
	Reason  string   `long:"reason" optional:"true" help:"Reason for the command, recorded in the audit and run log"`
}

type breakGlassGrantParams struct {
	Target   string `pos:"true" help:"Agent selector"`
	Duration string `long:"for" default:"30m" help:"Grant duration (operator chooses)"`
	Reason   string `long:"reason" optional:"true" help:"Reason for granting host execution"`
}

type breakGlassRevokeParams struct {
	Target string `pos:"true" help:"Agent selector"`
}

func breakGlassCmd() *cobra.Command {
	status := func(name string) *cobra.Command {
		return boa.CmdT[struct{}]{Use: name, Short: "Show live grants (agents: self; operator: all)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			os.Exit(runBreakGlassManagement(http.MethodGet, "status", nil, os.Stdout, os.Stderr))
		}}.ToCobra()
	}
	return boa.CmdT[breakGlassParams]{
		Use: "break-glass", Short: "Execute on the host under an operator-enabled break-glass grant",
		Long:        "Runs as agentd outside the agent sandbox. Only the operator may enable this capability; agents cannot request a grant. Runs use agentd's environment and PATH. Command output is logged in the operator's private data directory.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *breakGlassParams, c *cobra.Command, _ []string) {
			os.Exit(runBreakGlass(c.Context(), p, os.Stdin, os.Stdout, os.Stderr))
		},
		SubCmds: []*cobra.Command{
			status("status"), status("ls"),
			boa.CmdT[breakGlassParams]{Use: "run", Short: "Execute a command using your live operator grant", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *breakGlassParams, c *cobra.Command, _ []string) {
				os.Exit(runBreakGlass(c.Context(), p, os.Stdin, os.Stdout, os.Stderr))
			}}.ToCobra(),
			boa.CmdT[struct{}]{Use: "logs", Short: "Operator only: list recent run logs", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
				os.Exit(runBreakGlassManagement(http.MethodGet, "runs", nil, os.Stdout, os.Stderr))
			}}.ToCobra(),
			boa.CmdT[breakGlassGrantParams]{Use: "grant", Short: "Operator only: enable host execution for an agent", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *breakGlassGrantParams, _ *cobra.Command, _ []string) {
				os.Exit(runBreakGlassManagement(http.MethodPost, "grant", map[string]any{"target": p.Target, "duration": p.Duration, "reason": p.Reason}, os.Stdout, os.Stderr))
			}}.ToCobra(),
			boa.CmdT[breakGlassRevokeParams]{Use: "revoke", Short: "Operator only: revoke host execution for an agent", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *breakGlassRevokeParams, _ *cobra.Command, _ []string) {
				os.Exit(runBreakGlassManagement(http.MethodPost, "revoke", map[string]any{"target": p.Target}, os.Stdout, os.Stderr))
			}}.ToCobra(),
		},
	}.ToCobra()
}

func runBreakGlassManagement(method, path string, body any, stdout, stderr io.Writer) int {
	var result any
	if err := DaemonRequest(method, "/v1/break-glass/"+path, body, &result, DaemonOpts{}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

type breakGlassFrame struct {
	Stream   string `json:"stream"`
	Data     []byte `json:"data,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

func runBreakGlass(ctx context.Context, p *breakGlassParams, stdin io.Reader, stdout, stderr io.Writer) int {
	if (len(p.Args) == 0) == (p.Shell == "") {
		fmt.Fprintln(stderr, "provide a command after -- or a shell string with -c")
		return 2
	}
	conn, err := DialDaemonWebSocket(ctx, "/v1/break-glass/exec")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer conn.Close()
	request := map[string]any{"args": p.Args, "shell": p.Shell, "cwd": p.Cwd, "timeout": p.Timeout, "env": p.Env, "stdin": p.Stdin, "reason": p.Reason}
	if err = conn.WriteJSON(request); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	var writer sync.Mutex
	if p.Stdin {
		go func() {
			buf := make([]byte, 16384)
			for {
				n, e := stdin.Read(buf)
				if n > 0 {
					writer.Lock()
					writeErr := conn.WriteJSON(breakGlassFrame{Stream: "stdin", Data: buf[:n]})
					writer.Unlock()
					if writeErr != nil {
						return
					}
				}
				if e != nil {
					writer.Lock()
					_ = conn.WriteJSON(breakGlassFrame{Stream: "eof"})
					writer.Unlock()
					return
				}
			}
		}()
	}
	for {
		var frame breakGlassFrame
		if err = conn.ReadJSON(&frame); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if frame.Error != "" {
			fmt.Fprintln(stderr, frame.Error)
		}
		switch frame.Stream {
		case "stdout":
			if _, err = stdout.Write(frame.Data); err != nil {
				return 1
			}
		case "stderr":
			if _, err = stderr.Write(frame.Data); err != nil {
				return 1
			}
		case "error":
			return 1
		case "exit":
			if frame.ExitCode == nil {
				return 1
			}
			return *frame.ExitCode
		}
	}
}
