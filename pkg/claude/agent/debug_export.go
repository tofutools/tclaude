package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type debugExportParams struct {
	Target string `pos:"true" optional:"true" help:"Agent selector; omit to export the calling agent's own configuration"`
	File   string `long:"file" short:"f" optional:"true" help:"Write JSON to this path instead of stdout"`
}

func debugExportCmd() *cobra.Command {
	return boa.CmdT[debugExportParams]{
		Use:         "debug-export [target]",
		Short:       "Export an agent's recorded launch and sandbox configuration",
		Long:        "Exports three configurations side by side: requested (the original spawn request, including omitted fields), resolved (durable values and the exact composed sandbox profiles), and running (the latest recorded launch). Environment values and local paths are included because they are needed for sandbox diagnosis; treat the result as sensitive. The task briefing and one-time authorization token are redacted. Omit target when running as the agent itself; exporting another agent requires agent.debug-export or group ownership.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *debugExportParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Target).SetAlternativesFunc(completeConvSelectors)
			return nil
		},
		RunFunc: func(p *debugExportParams, _ *cobra.Command, _ []string) {
			os.Exit(runDebugExport(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runDebugExport(p *debugExportParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	path := "/v1/whoami/debug-export"
	if target := strings.TrimSpace(p.Target); target != "" {
		path = "/v1/agent/" + url.PathEscape(target) + "/debug-export"
	}
	raw, _, err := DaemonGetRaw(path)
	if err != nil {
		fmt.Fprintf(stderr, "Error: export debug info: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		fmt.Fprintf(stderr, "Error: malformed debug export JSON from daemon: %v\n", err)
		return rcIOFailure
	}
	out.WriteByte('\n')
	if file := strings.TrimSpace(p.File); file != "" {
		if err := writePrivateDebugExport(file, out.Bytes()); err != nil {
			fmt.Fprintf(stderr, "Error: writing %s: %v\n", file, err)
			return rcIOFailure
		}
		return rcOK
	}
	if _, err := stdout.Write(out.Bytes()); err != nil {
		fmt.Fprintf(stderr, "Error: writing export: %v\n", err)
		return rcIOFailure
	}
	return rcOK
}

func writePrivateDebugExport(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	// OpenFile's mode applies only at creation. Tighten an existing target
	// through the descriptor before replacing its contents.
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if err = f.Truncate(0); err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}
