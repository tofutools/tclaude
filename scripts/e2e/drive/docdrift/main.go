// docdrift walks the dashboard pages in scripts/docs/tour.json on a seeded server
// and records what each one shows, for comparing the docs with the real UI. Run it
// through scripts/docs/drift.sh, which builds the server (TestDocShotsServe in
// pkg/claude/agentd) and isolates HOME. It only reads: a stop that fails is
// recorded and the tour goes on, because a page that is gone is itself a finding.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// stepTimeout bounds one wait. A wait that never matches fails the stop.
const stepTimeout = 8 * time.Second

// Step opens something a page does not show on load. Text narrows the selector
// to elements containing that text.
type Step struct {
	Click string `json:"click,omitempty"`
	Open  string `json:"open,omitempty"`
	Wait  string `json:"wait,omitempty"`
	Text  string `json:"text,omitempty"`
}

// Stop is one view of the dashboard.
type Stop struct {
	Name    string `json:"name"`
	Fixture string `json:"fixture"`
	Page    string `json:"page"`
	Tab     string `json:"tab,omitempty"`
	Steps   []Step `json:"steps,omitempty"`
	// Error says why the stop could not be reached; set in index.json only.
	Error string `json:"error,omitempty"`
}

type tour struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Stops  []Stop `json:"stops"`
}

// Open folds a <details> out, unlike Click on its summary: the fold is remembered
// on the server, so a toggle would close it for the next stop.

// freeze stops the browser clock at the server's, so ages read the same each run.
const freeze = `(t => { const R = Date; window.Date = class extends R {
  constructor(...a) { if (a.length) super(...a); else super(t); } static now() { return t; } }; })`

const find = `window.__find = (s, t) => Array.from(document.querySelectorAll(s))
  .find(e => !t || e.textContent.includes(t));`

// record returns the visible text, then every label a control carries. Controls
// in closed dialogs and menus are listed too, marked hidden, so a label that is
// only one click away does not read as missing.
const record = `() => {
  const out = [document.body.innerText, '', '--- controls ---'];
  const seen = new Set();
  const sel = 'button, a, summary, label, option, th, h1, h2, h3, h4, legend, [role=tab], [role=menuitem], input, textarea, select, [title], [aria-label]';
  for (const e of document.querySelectorAll(sel)) {
    const hidden = e.getClientRects().length === 0;
    const words = [e.matches('input, textarea, select') ? '' : e.textContent, e.title, e.getAttribute('aria-label'), e.placeholder];
    for (const w of words) {
      const label = (w || '').replace(/\s+/g, ' ').trim();
      const line = (hidden ? 'hidden: ' : 'shown: ') + label;
      if (label && label.length <= 120 && !seen.has(line)) { seen.add(line); out.push(line); }
    }
  }
  const values = Array.from(document.querySelectorAll('input, textarea')).map(e => e.value).filter(Boolean);
  return [out.join('\n'), values.join('\n')];
}`

func main() {
	tourPath := flag.String("tour", "scripts/docs/tour.json", "tour manifest")
	serverBin := flag.String("server", "", "compiled pkg/claude/agentd test binary")
	serverDir := flag.String("server-dir", "pkg/claude/agentd", "directory to run the server binary in")
	out := flag.String("out", "docs-drift-out/tour", "output directory")
	frozenTime := flag.String("frozen-time", "", "RFC 3339 instant the server's clock is frozen at")
	hostWords := flag.String("host-words", "", "comma-separated words that mark real host data; a stop showing one is refused")
	flag.Parse()
	if err := run(*tourPath, *serverBin, *serverDir, *out, *frozenTime, strings.Split(*hostWords, ",")); err != nil {
		fmt.Fprintln(os.Stderr, "docdrift:", err)
		os.Exit(1)
	}
}

func run(tourPath, serverBin, serverDir, out, frozenTime string, hostWords []string) error {
	raw, err := os.ReadFile(tourPath)
	if err != nil {
		return err
	}
	var t tour
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return fmt.Errorf("%s: %w", tourPath, err)
	}
	at, err := time.Parse(time.RFC3339, frozenTime)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	browser, stopBrowser, err := launchChrome(t)
	if err != nil {
		return err
	}
	defer stopBrowser()

	// Stops share a server, which keeps a tour of several dozen pages quick. The
	// server remembers folds and lenses, so a stop names the view it needs.
	servers := map[string]string{}
	var failed int
	for i := range t.Stops {
		s := &t.Stops[i]
		url, ok := servers[s.Fixture]
		if !ok {
			var stopServer func()
			url, stopServer, err = startServer(serverBin, serverDir, s.Fixture)
			if err != nil {
				return fmt.Errorf("fixture %s: %w", s.Fixture, err)
			}
			defer stopServer()
			servers[s.Fixture] = url
		}
		if err := visit(browser, url, t, *s, out, at.UnixMilli(), hostWords); err != nil {
			s.Error, _, _ = strings.Cut(err.Error(), "\n")
			failed++
			fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", s.Name, err)
			continue
		}
		fmt.Println("stop", s.Name)
	}
	index, err := json.MarshalIndent(t.Stops, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "index.json"), index, 0o644); err != nil {
		return err
	}
	fmt.Printf("%d stops, %d unreachable\n", len(t.Stops), failed)
	return nil
}

func launchChrome(t tour) (*rod.Browser, func(), error) {
	bin := os.Getenv("TCLAUDE_DOCSHOTS_CHROME")
	if bin == "" {
		var ok bool
		if bin, ok = launcher.LookPath(); !ok {
			return nil, nil, errors.New("no Chrome/Chromium found; set TCLAUDE_DOCSHOTS_CHROME")
		}
	}
	profile, err := os.MkdirTemp("", "docdrift-chrome-")
	if err != nil {
		return nil, nil, err
	}
	// Chrome's crashpad ignores --user-data-dir and aborts when ~/.config is not
	// writable, so the XDG roots move into the disposable profile too.
	for _, x := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
		os.Setenv(x, filepath.Join(profile, x))
	}
	l := launcher.New().Bin(bin).UserDataDir(filepath.Join(profile, "profile")).
		Headless(true).NoSandbox(true).Leakless(false).
		Set("disable-gpu").Set("hide-scrollbars").
		Set("window-size", fmt.Sprintf("%d,%d", t.Width, t.Height))
	cleanup := func() { l.Kill(); os.RemoveAll(profile) }
	u, err := l.Launch()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("launch %s: %w", bin, err)
	}
	b := rod.New().ControlURL(u)
	if err := b.Connect(); err != nil {
		cleanup()
		return nil, nil, err
	}
	return b, func() { b.Close(); cleanup() }, nil
}

// startServer runs the seeded server for one fixture until stop is called.
func startServer(bin, dir, fixture string) (string, func(), error) {
	work, err := os.MkdirTemp("", "docdrift-server-")
	if err != nil {
		return "", nil, err
	}
	cmd := exec.Command(bin, "-test.run", "^TestDocShotsServe$", "-test.timeout", "30m")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TCLAUDE_DOCSHOTS_SERVE="+work, "TCLAUDE_DOCSHOTS_FIXTURE="+fixture, "TZ=UTC")
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stop := func() {
		os.WriteFile(filepath.Join(work, "stop"), nil, 0o600)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
		}
		os.RemoveAll(work)
	}
	deadline := time.After(90 * time.Second)
	for {
		if url, err := os.ReadFile(filepath.Join(work, "url")); err == nil {
			return string(url), stop, nil
		}
		select {
		case err := <-exited:
			os.RemoveAll(work)
			return "", nil, fmt.Errorf("server exited (%v):\n%s", err, log.String())
		case <-deadline:
			stop()
			return "", nil, fmt.Errorf("server not ready:\n%s", log.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func visit(browser *rod.Browser, url string, t tour, s Stop, out string, frozenMillis int64, hostWords []string) error {
	return rod.Try(func() {
		page := browser.MustPage("")
		defer page.Close()
		p := page.Timeout(60 * time.Second)
		p.MustSetViewport(t.Width, t.Height, 1, false)
		if _, err := p.EvalOnNewDocument(fmt.Sprintf("%s(%d);%s", freeze, frozenMillis, find)); err != nil {
			panic(err)
		}
		p.MustNavigate(url + s.Page)
		p.MustWaitLoad()
		waitFor(p, "body:not(.dashboard-booting)", "")
		waitFor(p, "nav [data-tab]", "")
		if s.Tab != "" {
			waitFor(p, fmt.Sprintf(`nav [data-tab=%q].active`, s.Tab), "")
		}
		for _, st := range s.Steps {
			switch {
			case st.Click != "":
				waitFor(p, st.Click, st.Text)
				p.MustEval(`(s, t) => window.__find(s, t).click()`, st.Click, st.Text)
			case st.Open != "":
				waitFor(p, st.Open, st.Text)
				p.MustEval(`(s, t) => { const d = window.__find(s, t); if (!d.open) d.querySelector('summary').click(); }`, st.Open, st.Text)
			default:
				waitFor(p, st.Wait, st.Text)
			}
		}
		// Let the first poll and any lazy module fill the page.
		time.Sleep(time.Second)
		seen := p.MustEval(record).Arr()
		if word := hostWord(seen[0].Str()+"\n"+seen[1].Str(), hostWords); word != "" {
			panic(fmt.Errorf("page shows %q from the host", word))
		}
		if err := os.WriteFile(filepath.Join(out, s.Name+".txt"), []byte(seen[0].Str()), 0o644); err != nil {
			panic(err)
		}
		img, err := p.Screenshot(true, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng})
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(out, s.Name+".png"), img, 0o644); err != nil {
			panic(err)
		}
	})
}

// hostWord returns the first word of words that text shows as a whole word.
// Captures go on tickets, so nothing from the real machine may be in one.
func hostWord(text string, words []string) string {
	for _, w := range words {
		if len(w) > 1 && regexp.MustCompile(`(?i)(^|[^\pL\pN])`+regexp.QuoteMeta(w)+`($|[^\pL\pN])`).MatchString(text) {
			return w
		}
	}
	return ""
}

// waitFor polls from Go: the page's clock is frozen, so an in-page deadline
// would never pass.
func waitFor(p *rod.Page, sel, text string) {
	deadline := time.Now().Add(stepTimeout)
	for {
		hit, err := p.Eval(`(s, t) => !!window.__find(s, t)`, sel, text)
		if err == nil && hit.Value.Bool() {
			return
		}
		if time.Now().After(deadline) {
			panic(fmt.Errorf("no element %q containing %q", sel, text))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
