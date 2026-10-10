package federationcmd

import (
	"context"
	"errors"
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common"
	"io"
	"net/url"
	"os"
	"time"
)

type fileGetParams struct {
	Target string `pos:"true" help:"Agent address agt_…@peer."`
	Path   string `pos:"true" help:"File inside the agent's project working directory."`
	Output string `long:"output" short:"o" help:"New local output file (existing files are refused)."`
}

func fileCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "file", Short: "Download project files shared by remote terminal sessions"}
	cmd.AddCommand(boa.CmdT[fileGetParams]{Use: "get", Short: "Save a remote file; requires sessions.files.read and terminal watch authority", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *fileGetParams, _ *cobra.Command, _ []string) { os.Exit(runFileGet(p, os.Stderr)) }}.ToCobra())
	return cmd
}
func runFileGet(p *fileGetParams, stderr io.Writer) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	body, err := agent.DaemonStreamGet(ctx, "/v1/federation/file?"+url.Values{"target": {p.Target}, "path": {p.Path}}.Encode())
	if err != nil {
		return fail(stderr, err)
	}
	defer body.Close()
	f, err := os.OpenFile(p.Output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail(stderr, err)
	}
	n, err := io.Copy(f, io.LimitReader(body, (32<<20)+1))
	err = errors.Join(err, f.Close())
	if n > 32<<20 {
		err = errors.Join(err, errors.New("file exceeds 32 MiB"))
	}
	if err != nil {
		_ = os.Remove(p.Output)
		return fail(stderr, err)
	}
	return 0
}
