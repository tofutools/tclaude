//go:build linux

package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
)

func tclaudeLayerRouteHelperCmd() *cobra.Command {
	var (
		socketPath       string
		agentID          string
		convID           string
		launchGeneration string
		credentialFIFO   string
		groupIDs         []int64
	)
	cmd := &cobra.Command{
		Use:    tclaudeLayerRouteHelperCommand,
		Short:  "Run the namespace-local group-route helper (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			credential, err := readRouteHelperCredentialFIFO(credentialFIFO)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "ready"); err != nil {
				return fmt.Errorf("signal route helper readiness: %w", err)
			}
			return routeadapter.RunHelper(cmd.Context(), routeadapter.HelperConfig{
				SocketPath: socketPath, AgentID: agentID, ConvID: convID,
				LaunchGeneration: launchGeneration, Credential: credential, GroupIDs: groupIDs,
			})
		},
	}
	cmd.Flags().StringVar(&socketPath, "socket", "", "agentd Unix socket (internal)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "stable agent identity (internal)")
	cmd.Flags().StringVar(&convID, "conv-id", "", "conversation identity (internal)")
	cmd.Flags().StringVar(&launchGeneration, "launch-generation", "", "launch generation (internal)")
	cmd.Flags().StringVar(&credentialFIFO, "credential-fifo", "", "one-shot helper credential FIFO (internal)")
	cmd.Flags().Int64SliceVar(&groupIDs, "group-id", nil, "explicit route-enabled group id (repeatable)")
	return cmd
}

func readRouteHelperCredentialFIFO(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("route helper credential FIFO is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open route helper credential FIFO: %w", err)
	}
	defer f.Close()
	credential, err := io.ReadAll(io.LimitReader(bufio.NewReader(f), 4096))
	if err != nil {
		return "", fmt.Errorf("read route helper credential FIFO: %w", err)
	}
	value := strings.TrimSpace(string(credential))
	if value == "" {
		return "", errors.New("route helper credential FIFO was empty")
	}
	return value, nil
}
