package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

type pafConsentParams struct {
	File      string   `pos:"true" help:"PAF file to consent to."`
	Out       string   `long:"out" short:"o" help:"Write the PAF with the consent record here (may be the input file)."`
	Audience  string   `long:"audience" help:"Who may receive it: self, recipients, board or public."`
	Recipient []string `long:"recipient" optional:"true" help:"A recipient's instance id, for --audience recipients. Repeatable."`
	Board     string   `long:"board" optional:"true" help:"The board, for --audience board."`
	Yes       bool     `long:"yes" short:"y" help:"Record consent without the interactive prompt, after reviewing the summary."`
}

// PAFConsentCmd is `tclaude paf consent`: a human's recorded confirmation
// that a PAF may go to an audience.
func PAFConsentCmd() *cobra.Command {
	return boa.CmdT[pafConsentParams]{Use: "consent <file>", Short: "Record your consent to sharing a PAF with an audience",
		Long: "Show what a PAF would share (size, conversation time range and turn count, this node's credential scan " +
			"and the recorded redactions), then, once you confirm, record your consent in its privacy.json, bound to the " +
			"content digest. The content is copied byte for byte and the file is re-signed by this node. Changing the " +
			"content afterwards (for example redacting it) makes the consent stale. Only the human operator can consent; " +
			"agents are refused.\n\n" +
			"  tclaude paf consent reviewer.paf --audience recipients --recipient inst_7c21 -o reviewer.paf",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *pafConsentParams, _ *cobra.Command, _ []string) {
			os.Exit(runPAFConsent(p, os.Stdin, os.Stdout, os.Stderr, term.IsTerminal(int(os.Stdin.Fd()))))
		}}.ToCobra()
}

func runPAFConsent(p *pafConsentParams, stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
	if p.Out == "" {
		fmt.Fprintln(stderr, "--out is required")
		return rcInvalidArg
	}
	in, err := os.Open(p.File)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcInvalidArg
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	a, err := paf.Open(in, st.Size(), paf.Options{Purpose: paf.PurposeInspect})
	if err != nil {
		fmt.Fprintln(stderr, "consent is recorded in PAF files only:", err)
		return rcInvalidArg
	}
	s, err := privacy.SummarizeForConsent(a, harness.MaxHistoryRecordBytes)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	printConsentSummary(stdout, p, s)
	if !p.Yes {
		if !interactive {
			fmt.Fprintln(stderr, "not a terminal: review the summary and pass --yes to consent")
			return rcInvalidArg
		}
		fmt.Fprint(stdout, "\nRecord your consent? [y/N]: ")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(stdout, "No consent recorded.")
			return rcOK
		}
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	q := url.Values{"audience": {p.Audience}, "recipient": p.Recipient, "content_digest": {s.Digest}}
	if p.Board != "" {
		q.Set("board", p.Board)
	}
	raw, err := exchangePAF("/v1/paf/consent?"+q.Encode(), in)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if err := writeFileAtomic(p.Out, raw); err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	fmt.Fprintln(stdout, "Consent recorded in", p.Out)
	return rcOK
}

func printConsentSummary(out io.Writer, p *pafConsentParams, s *privacy.ConsentSummary) {
	audience := p.Audience
	switch {
	case len(p.Recipient) > 0:
		audience += ": " + strings.Join(p.Recipient, ", ")
	case p.Board != "":
		audience += ": " + p.Board
	}
	fmt.Fprintf(out, "%s would go to %s\n", consentSafe(p.File), consentSafe(audience))
	fmt.Fprintf(out, "  %-12s %d bytes, %d agent(s)\n", "Size:", s.Bytes, s.Agents)
	conv := fmt.Sprintf("%d human turn(s)", s.Turns)
	if s.From != nil && s.To != nil {
		conv += fmt.Sprintf(", %s to %s", s.From.Format(time.RFC3339), s.To.Format(time.RFC3339))
	}
	if s.NativeOnly > 0 {
		conv += fmt.Sprintf("; %d agent(s) with native history only, not counted", s.NativeOnly)
	}
	fmt.Fprintf(out, "  %-12s %s\n", "History:", conv)
	if len(s.Findings) == 0 {
		fmt.Fprintf(out, "  %-12s %s\n", "Scan:", "no suspected credentials")
	}
	for _, f := range s.Findings {
		fmt.Fprintf(out, "  %-12s %s: %d\n", "Scan:", f.Kind, f.Count)
	}
	if len(s.Redactions) == 0 {
		fmt.Fprintf(out, "  %-12s %s\n", "Redactions:", "none; the history is carried verbatim")
	}
	for _, r := range s.Redactions {
		fmt.Fprintf(out, "  %-12s %s: %d\n", "Redactions:", consentSafe(r.Kind), r.Count)
	}
	fmt.Fprintf(out, "  %-12s %s\n", "Digest:", s.Digest)
	for _, c := range s.Existing {
		state := "current"
		if !c.Current {
			state = "stale"
		}
		fmt.Fprintf(out, "  %-12s %s by %s at %s (%s; kept only if this node signed the file and for the same audience)\n", "Existing:",
			consentSafe(c.Audience), consentSafe(c.By), c.At.Format(time.RFC3339), state)
	}
}

// consentSafe keeps file text from controlling the terminal.
func consentSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return '�'
		}
		return r
	}, s)
}

// exchangePAF posts a PAF to the daemon and returns the PAF it answers with.
func exchangePAF(path string, f *os.File) ([]byte, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, "http://_"+path, f)
	if err != nil {
		return nil, err
	}
	req.ContentLength = st.Size()
	req.GetBody = func() (io.ReadCloser, error) { return os.Open(f.Name()) }
	attachCallerIdentity(req)
	req.Header.Set("Content-Type", paf.MediaType)
	client := httpClientWithTimeout(bundletransfer.DefaultTTL)
	policy, err := retryPolicyForRequest(client, req, os.Stderr)
	if err != nil {
		return nil, err
	}
	resp, err := doDaemonRequest(client, req, os.Stderr, policy)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := daemonResponseBytes(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, decodeDaemonError(resp.StatusCode, raw)
	}
	if len(raw) == 0 {
		return nil, errors.New("daemon returned no archive")
	}
	return raw, nil
}

// writeFileAtomic writes beside path and renames into place, so the input
// file can be the destination and a failure leaves it intact.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".paf-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
