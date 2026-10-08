package proxy

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type httpParams struct {
	Name     string   `pos:"true" help:"Configured proxy instance name"`
	Path     string   `pos:"true" help:"Path relative to the configured base URL (may include a query)"`
	Method   string   `long:"method" short:"X" default:"GET" help:"HTTP method"`
	Header   []string `long:"header" short:"H" optional:"true" help:"Request header as Name: value; repeatable"`
	BodyFile string   `long:"body-file" optional:"true" help:"Request body file, or - for stdin (maximum 4 MiB)"`
	JSON     bool     `long:"json" help:"Output status, headers and base64 body as JSON"`
	AskHuman string   `long:"ask-human" optional:"true" help:"Ask human on permission denial with this timeout"`
}

func httpCmd() *cobra.Command {
	return boa.CmdT[httpParams]{
		Use: "http", Short: "Send a request through a named HTTP proxy",
		Long:        "Send a request relative to a configured service URL. The daemon adds its configured header. Requires proxy.http, optionally scoped with http_proxy=name. Redirects are returned without following them. By default prints the response body; --json includes status and headers. HTTP 4xx/5xx responses exit with an error status.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *httpParams, _ *cobra.Command, _ []string) {
			os.Exit(httpProxyCall(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func httpProxyCall(p *httpParams, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintln(stderr, "Error:", err); return rcInvalidArg }
	ask, err := agent.ParseAskHuman(p.AskHuman)
	if err != nil {
		return fail(err)
	}
	headers := map[string]string{}
	for _, raw := range p.Header {
		name, value, ok := strings.Cut(raw, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return fail(fmt.Errorf("header must be Name: value"))
		}
		headers[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	var data []byte
	if p.BodyFile != "" {
		input := stdin
		if p.BodyFile != "-" {
			f, err := os.Open(p.BodyFile)
			if err != nil {
				return fail(err)
			}
			defer f.Close()
			input = f
		}
		data, err = io.ReadAll(io.LimitReader(input, 4*1024*1024+1))
		if err != nil {
			return fail(err)
		}
		if len(data) > 4*1024*1024 {
			return fail(fmt.Errorf("body exceeds 4 MiB"))
		}
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Status  int         `json:"status"`
		Headers http.Header `json:"headers"`
		Body    []byte      `json:"body"`
	}
	body := map[string]any{"name": p.Name, "path": p.Path, "method": p.Method, "headers": headers, "body": data}
	if err := agent.DaemonRequest(http.MethodPost, "/v1/http/request", body, &resp, agent.DaemonOpts{AskHuman: ask, Timeout: 75 * time.Second, NoRetry: true}); err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return agent.MapDaemonErrorToRC(err)
	}
	if p.JSON {
		if rc := writeJSONIndent(stdout, resp); rc != rcOK {
			return rc
		}
	} else if _, err := stdout.Write(resp.Body); err != nil {
		return rcIOFailure
	}
	if resp.Status >= 400 {
		return rcIOFailure
	}
	return rcOK
}
