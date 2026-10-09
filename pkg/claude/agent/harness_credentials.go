package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/harnesscredentials"
)

type credentialParams struct {
	Harness      string `pos:"true" help:"Harness name; push accepts a comma-separated chosen set (claude,codex,opencode,gemini)"`
	Node         string `long:"node" help:"Trusted peer label/ID; omit for local backup/restore"`
	ConfirmShare bool   `long:"confirm-share" help:"Confirm that remote agents will act as you with these providers"`
	Backup       string `long:"backup" help:"Restore a specific backup ID; omit for newest matching backup"`
	JSON         bool   `long:"json" help:"Output JSON metadata; credential contents are never returned"`
}

func harnessCredentialCommand() *cobra.Command {
	commands := []*cobra.Command{}
	for _, action := range []string{"push", "backup", "restore", "ls"} {
		commands = append(commands, boa.CmdT[credentialParams]{Use: action, Short: map[string]string{"push": "Push your own credentials to a peer, backing up before replacement", "backup": "Back up local or peer credentials privately", "restore": "Restore a credential backup, backing up current files first", "ls": "List credential backup metadata"}[action], ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *credentialParams, _ *cobra.Command, _ []string) {
			os.Exit(runCredentialOperation(action, p, os.Stdout, os.Stderr))
		}}.ToCobra())
	}
	return boa.CmdT[struct{}]{Use: "credentials", Short: "Push your credentials and manage private backups (operator only)", ParamEnrich: common.DefaultParamEnricher(), SubCmds: commands}.ToCobra()
}
func runCredentialOperation(action string, p *credentialParams, stdout, stderr io.Writer) int {
	names := strings.Split(p.Harness, ",")
	if action != "push" && len(names) != 1 || action == "push" && (p.Node == "" || !p.ConfirmShare) || action != "push" && p.ConfirmShare || action != "restore" && p.Backup != "" || p.Backup != "" && !harnesscredentials.ValidBackupID(p.Backup) {
		fmt.Fprintln(stderr, "Error: push requires --node and --confirm-share (remote agents will act as you); --backup is only valid for restore")
		return rcInvalidArg
	}
	seen := map[string]bool{}
	for _, name := range names {
		switch name {
		case "claude", "codex", "opencode", "gemini":
		default:
			fmt.Fprintln(stderr, "Error: file credentials unsupported for this harness; use its login flow")
			return rcInvalidArg
		}
		if seen[name] {
			fmt.Fprintln(stderr, "Error: duplicate harness")
			return rcInvalidArg
		}
		seen[name] = true
	}
	if rc := RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	results := []json.RawMessage{}
	rc := 0
	for _, name := range names {
		tail := "harnesses/credentials/" + action
		method := http.MethodPost
		var in any = struct {
			Harness      string `json:"harness"`
			ConfirmShare bool   `json:"confirm_share,omitempty"`
			Backup       string `json:"backup,omitempty"`
		}{name, p.ConfirmShare, p.Backup}
		if action == "ls" {
			tail = "harnesses/credentials/backups?harness=" + url.QueryEscape(name)
			method = http.MethodGet
			in = nil
		}
		path := "/v1/" + tail
		if p.Node != "" {
			path = "/v1/federation/peer/" + url.PathEscape(p.Node) + "/" + tail
		}
		var raw json.RawMessage
		if err := DaemonRequest(method, path, in, &raw, DaemonOpts{Timeout: 20 * time.Second, NoRetry: true}); err != nil {
			var de *DaemonError
			if errors.As(err, &de) && json.Valid(de.Raw) {
				fmt.Fprintln(stderr, string(de.Raw))
			} else {
				fmt.Fprintln(stderr, "Error:", err)
			}
			rc = MapDaemonErrorToRC(err)
			continue
		}
		results = append(results, raw)
		if !p.JSON {
			var response struct {
				Receipt harnesscredentials.RestoreReceipt `json:"receipt"`
				Backups []harnesscredentials.BackupInfo   `json:"backups"`
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				fmt.Fprintln(stderr, "Error: invalid credential response")
				rc = rcIOFailure
				continue
			}
			if action == "ls" {
				for _, b := range response.Backups {
					fmt.Fprintf(stdout, "%s %s %s\n", updateCell(b.ID), b.CreatedAt.Format(time.RFC3339), updateCell(b.Location))
				}
			} else {
				fmt.Fprintf(stdout, "%s %s: backup %s (%s)\n", name, action, updateCell(response.Receipt.BackupID), updateCell(response.Receipt.BackupLocation))
			}
		}
	}
	if p.JSON {
		var out any = results
		if len(names) == 1 && len(results) == 1 {
			out = results[0]
		}
		if json.NewEncoder(stdout).Encode(out) != nil {
			return rcIOFailure
		}
	}
	return rc
}
