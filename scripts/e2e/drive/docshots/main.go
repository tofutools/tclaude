// docshots captures the dashboard images in README.md and docs/ from the
// manifest at scripts/docs/shots.json. Run it through scripts/docs/screenshots.sh,
// which builds the seeded server (TestDocShotsServe in pkg/claude/agentd) and
// isolates HOME. A shot whose page, tab or target is missing fails the whole run,
// and nothing is installed, so a renamed tab cannot produce a wrong image quietly.
package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// stepTimeout bounds one step. A wait that never matches is a failed shot.
const stepTimeout = 8 * time.Second

// changedShare is the share of pixels that must differ before a new image
// replaces the installed one, so font-rendering noise causes no diff.
const changedShare = 0.0005

// Step is one setup action. Exactly one verb field is set; Text narrows the
// verb's CSS selector to elements containing that text.
type Step struct {
	Click   string `json:"click,omitempty"`
	Wait    string `json:"wait,omitempty"`
	Fill    string `json:"fill,omitempty"`
	Check   string `json:"check,omitempty"`
	Scroll  string `json:"scroll,omitempty"`
	Hide    string `json:"hide,omitempty"`
	Remove  string `json:"remove,omitempty"`
	Open    string `json:"open,omitempty"`
	Key     string `json:"key,omitempty"`
	Eval    string `json:"eval,omitempty"`
	Text    string `json:"text,omitempty"`
	Value   string `json:"value,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// Shot is one image in the manifest.
type Shot struct {
	Name    string `json:"name"`
	Fixture string `json:"fixture"`
	Page    string `json:"page"`
	Tab     string `json:"tab"`
	Target  string `json:"target"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	// Element, when set, clips the image to that element.
	Element string `json:"element,omitempty"`
	Steps   []Step `json:"steps"`
}

type manifest struct {
	// FrozenTime is the instant both the seeded server and the browser see.
	FrozenTime string `json:"frozenTime"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Shots      []Shot `json:"shots"`
}

// initScript freezes everything that would differ between two runs, before any
// dashboard module runs.
func initScript(frozenTime string) string {
	return `(() => {
  const T = Date.parse(` + strconv.Quote(frozenTime) + `), Real = Date;
  class Frozen extends Real {
    constructor(...a) { if (a.length) super(...a); else super(T); }
    static now() { return T; }
  }
  window.Date = Frozen;
  let s = 0x7c1a0de5;
  Math.random = () => {
    s = s + 0x6D2B79F5 | 0;
    let t = Math.imul(s ^ s >>> 15, 1 | s);
    t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
    return ((t ^ t >>> 14) >>> 0) / 4294967296;
  };
  const realFetch = window.fetch;
  window.__docshotsHold = false;
  window.fetch = (...a) => window.__docshotsHold ? new Promise(() => {}) : realFetch(...a);
  const style = () => {
    const el = document.createElement('style');
    el.textContent = ` + "`" + `
      @font-face { font-family: ui-sans-serif; src: local("Noto Sans Regular"), local("NotoSans-Regular"), local("Noto Sans"); font-weight: 400; }
      @font-face { font-family: ui-sans-serif; src: local("Noto Sans Bold"), local("NotoSans-Bold"); font-weight: 700; }
      @font-face { font-family: ui-monospace; src: local("DejaVu Sans Mono"), local("DejaVuSansMono"); font-weight: 400; }
      @font-face { font-family: ui-monospace; src: local("DejaVu Sans Mono Bold"), local("DejaVuSansMono-Bold"); font-weight: 700; }
      *, *::before, *::after { animation: none !important; transition: none !important; caret-color: transparent !important; }
    ` + "`" + `;
    document.documentElement.appendChild(el);
  };
  if (document.documentElement) style();
  else new MutationObserver((_, o) => { if (document.documentElement) { o.disconnect(); style(); } }).observe(document, {childList: true});
})();`
}

// helpers is evaluated into each page. find returns the first match by CSS
// selector and optional text.
const helpers = `window.__find = (sel, text) => Array.from(document.querySelectorAll(sel))
  .find(e => !text || e.textContent.includes(text));
window.__scrub = () => [document.body.innerText,
  ...Array.from(document.querySelectorAll('input, textarea')).map(e => e.value || e.placeholder || '')].join('\n');`

func main() {
	manifestPath := flag.String("manifest", "scripts/docs/shots.json", "shot manifest")
	root := flag.String("root", ".", "repository root; manifest targets are relative to it")
	serverBin := flag.String("server", "", "compiled pkg/claude/agentd test binary")
	serverDir := flag.String("server-dir", "pkg/claude/agentd", "directory to run the server binary in")
	out := flag.String("out", "docshots-out", "output directory")
	only := flag.String("only", "", "regexp of shot names to capture")
	install := flag.Bool("install", false, "copy changed images onto their targets")
	overlay := flag.String("write-overlay", "", "write a go build overlay freezing time.Now into this directory, then exit")
	flag.Parse()
	if *overlay != "" {
		if err := writeOverlay(*manifestPath, *overlay); err != nil {
			fmt.Fprintln(os.Stderr, "docshots:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*manifestPath, *root, *serverBin, *serverDir, *out, *only, *install); err != nil {
		fmt.Fprintln(os.Stderr, "docshots:", err)
		os.Exit(1)
	}
}

func loadManifest(path string) (manifest, error) {
	var m manifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// writeOverlay patches a copy of the toolchain's time.Now so its wall clock
// stands still at the manifest's frozenTime. The monotonic reading stays real, so
// time.Since, timeouts and timers behave normally. The server binary alone is
// built with it (`go test -c -overlay`); no code path in tclaude changes.
func writeOverlay(manifestPath, dir string) error {
	m, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}
	at, err := time.Parse(time.RFC3339, m.FrozenTime)
	if err != nil {
		return err
	}
	goroot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return err
	}
	src := filepath.Join(strings.TrimSpace(string(goroot)), "src", "time", "time.go")
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	const anchor = "func Now() Time {\n\tsec, nsec, mono := runtimeNow()\n"
	if !bytes.Contains(raw, []byte(anchor)) {
		return fmt.Errorf("%s has no %q: the Go toolchain changed time.Now, update writeOverlay", src, anchor)
	}
	patched := bytes.Replace(raw, []byte(anchor), fmt.Appendf(nil, "%ssec, nsec = %d, 0\n", anchor, at.Unix()), 1)
	copyPath := filepath.Join(dir, "time.go")
	if err := os.WriteFile(copyPath, patched, 0o644); err != nil {
		return err
	}
	overlay, err := json.Marshal(map[string]map[string]string{"Replace": {src: copyPath}})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "overlay.json"), overlay, 0o644)
}

func run(manifestPath, root, serverBin, serverDir, out, only string, install bool) error {
	m, err := loadManifest(manifestPath)
	if err != nil {
		return err
	}
	if only != "" {
		re, err := regexp.Compile(only)
		if err != nil {
			return err
		}
		m.Shots = slices.DeleteFunc(m.Shots, func(s Shot) bool { return !re.MatchString(s.Name) })
		if len(m.Shots) == 0 {
			return fmt.Errorf("no shot matches %q", only)
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	browser, stop, err := launchChrome(m)
	if err != nil {
		return err
	}
	defer stop()

	var fixtures []string
	for _, s := range m.Shots {
		if !slices.Contains(fixtures, s.Fixture) {
			fixtures = append(fixtures, s.Fixture)
		}
	}
	var failures []string
	for _, fixture := range fixtures {
		url, stopServer, err := startServer(serverBin, serverDir, fixture)
		if err != nil {
			return fmt.Errorf("fixture %s: %w", fixture, err)
		}
		for _, s := range m.Shots {
			if s.Fixture != fixture {
				continue
			}
			if err := capture(browser, url, m, s, out); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", s.Name, err))
				fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", s.Name, err)
				continue
			}
			fmt.Println("shot", s.Name)
		}
		stopServer()
	}
	if len(failures) > 0 {
		return fmt.Errorf("%d of %d shots failed, nothing installed:\n  %s", len(failures), len(m.Shots), strings.Join(failures, "\n  "))
	}
	if err := contactSheet(browser, m, out); err != nil {
		return fmt.Errorf("contact sheet: %w", err)
	}
	return report(m, root, out, install)
}

func launchChrome(m manifest) (*rod.Browser, func(), error) {
	bin := os.Getenv("TCLAUDE_DOCSHOTS_CHROME")
	if bin == "" {
		var ok bool
		if bin, ok = launcher.LookPath(); !ok {
			return nil, nil, errors.New("no Chrome/Chromium found; set TCLAUDE_DOCSHOTS_CHROME")
		}
	}
	profile, err := os.MkdirTemp("", "docshots-chrome-")
	if err != nil {
		return nil, nil, err
	}
	// Chrome's crashpad ignores --user-data-dir and aborts when ~/.config is not
	// writable, so the XDG roots move into the disposable profile too.
	for _, x := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
		os.Setenv(x, filepath.Join(profile, strings.ToLower(x)))
	}
	l := launcher.New().Bin(bin).UserDataDir(filepath.Join(profile, "profile")).
		Headless(true).NoSandbox(true).Leakless(false).
		Set("disable-gpu").Set("hide-scrollbars").
		Set("force-device-scale-factor", "1").
		Set("font-render-hinting", "none").
		Set("disable-lcd-text").
		Set("force-color-profile", "srgb").
		Set("window-size", fmt.Sprintf("%d,%d", m.Width, m.Height))
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

// startServer runs the seeded server for one fixture. The child gets a fresh
// HOME from newFlow; the wrapper script has already moved this process's own.
func startServer(bin, dir, fixture string) (string, func(), error) {
	work, err := os.MkdirTemp("", "docshots-server-")
	if err != nil {
		return "", nil, err
	}
	cmd := exec.Command(bin, "-test.run", "^TestDocShotsServe$", "-test.timeout", "0")
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

func capture(browser *rod.Browser, url string, m manifest, s Shot, out string) error {
	return firstLine(rod.Try(func() {
		page := browser.MustPage("")
		defer page.Close()
		p := page.Timeout(60 * time.Second)
		w, h := cmp.Or(s.Width, m.Width), cmp.Or(s.Height, m.Height)
		p.MustSetViewport(w, h, 1, false)
		// The dashboard's CSP forbids the local() fonts initScript pins.
		if err := (proto.PageSetBypassCSP{Enabled: true}).Call(p); err != nil {
			panic(err)
		}
		if _, err := p.EvalOnNewDocument(initScript(m.FrozenTime)); err != nil {
			panic(err)
		}
		if err := (proto.EmulationSetTimezoneOverride{TimezoneID: "UTC"}).Call(p); err != nil {
			panic(err)
		}
		if err := (proto.EmulationSetLocaleOverride{Locale: "en-US"}).Call(p); err != nil {
			panic(err)
		}
		if _, err := p.EvalOnNewDocument(helpers); err != nil {
			panic(err)
		}
		p.MustNavigate(url + s.Page)
		p.MustWaitLoad()
		waitFor(p, "body:not(.dashboard-booting)", "")
		waitFor(p, "nav [data-tab]", "")
		if s.Tab != "" {
			waitFor(p, fmt.Sprintf(`nav [data-tab=%q].active`, s.Tab), "")
		}
		p.MustEval(`() => document.fonts.load('14px ui-sans-serif').then(a => document.fonts.load('14px ui-monospace').then(b => { if (!a.length || !b.length) throw new Error('Noto Sans / DejaVu Sans Mono not installed'); }))`)
		for i, st := range s.Steps {
			if err := runStep(p, st); err != nil {
				panic(fmt.Errorf("step %d: %w", i+1, err))
			}
		}
		// Stop the 2s poll from repainting between here and the screenshot.
		p.MustEval(`() => { window.__docshotsHold = true; }`)
		p.MustEval(`() => new Promise(r => setTimeout(r, 400))`)
		p.MustEval(`() => document.fonts.ready.then(() => true)`)
		if err := scrub(p); err != nil {
			panic(err)
		}
		var clip *proto.PageViewport
		if s.Element != "" {
			waitFor(p, s.Element, "")
			box := p.MustEval(`(s) => { const r = document.querySelector(s).getBoundingClientRect(); return {x: r.x, y: r.y, w: r.width, h: r.height}; }`, s.Element)
			clip = &proto.PageViewport{X: box.Get("x").Num(), Y: box.Get("y").Num(), Width: box.Get("w").Num(), Height: box.Get("h").Num(), Scale: 1}
		}
		img, err := p.Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng, Clip: clip})
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(out, s.Name+".png"), img, 0o644); err != nil {
			panic(err)
		}
	}))
}

// firstLine drops the stack trace rod.Try appends to a recovered panic.
func firstLine(err error) error {
	if err == nil {
		return nil
	}
	var evalErr *rod.EvalError
	if errors.As(err, &evalErr) && evalErr.Exception != nil {
		return errors.New(evalErr.Exception.Description)
	}
	var tryErr *rod.TryError
	if errors.As(err, &tryErr) {
		if inner, ok := tryErr.Value.(error); ok {
			return inner
		}
	}
	msg, _, _ := strings.Cut(err.Error(), "\n")
	return errors.New(msg)
}

// waitFor polls from Go: the page's clock is frozen, so an in-page deadline
// would never pass.
func waitFor(p *rod.Page, sel, text string) {
	if err := poll(p, sel, text); err != nil {
		panic(err)
	}
}

func poll(p *rod.Page, sel, text string) error {
	deadline := time.Now().Add(stepTimeout)
	for {
		hit, err := p.Eval(`(s, t) => !!window.__find(s, t)`, sel, text)
		if err == nil && hit.Value.Bool() {
			return nil
		}
		if time.Now().After(deadline) {
			if text != "" {
				return fmt.Errorf("no element %q containing %q", sel, text)
			}
			return fmt.Errorf("no element %q", sel)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func runStep(p *rod.Page, st Step) error {
	act := func(sel, js string, args ...any) error {
		if err := poll(p, sel, st.Text); err != nil {
			return err
		}
		_, err := p.Eval(`(s, t, ...a) => { const e = window.__find(s, t); (`+js+`)(e, ...a); }`, append([]any{sel, st.Text}, args...)...)
		return err
	}
	switch {
	case st.Click != "":
		return act(st.Click, `e => e.click()`)
	case st.Wait != "":
		return poll(p, st.Wait, st.Text)
	case st.Fill != "":
		return act(st.Fill, `(e, v) => {
		  const proto = e instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : e instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype;
		  Object.getOwnPropertyDescriptor(proto, 'value').set.call(e, v);
		  e.dispatchEvent(new Event('input', {bubbles: true}));
		  e.dispatchEvent(new Event('change', {bubbles: true}));
		}`, st.Value)
	case st.Check != "":
		return act(st.Check, `e => { const b = e.matches('input') ? e : e.querySelector('input'); if (!b) throw new Error('no checkbox'); if (!b.checked) b.click(); }`)
	case st.Scroll != "":
		return act(st.Scroll, `e => e.scrollIntoView({block: 'center'})`)
	case st.Hide != "":
		return act(st.Hide, `e => { (e.closest('[data-docshots-keep]') || e).style.setProperty('display', 'none', 'important'); }`)
	case st.Remove != "":
		// Hide every match: the selector is added as a stylesheet rule so the
		// dashboard's own repaints cannot bring the element back.
		return addCSS(p, st.Remove+` { display: none !important; }`)
	case st.Open != "":
		_, err := p.Eval(`(s) => document.querySelectorAll(s).forEach(d => { d.open = true; })`, st.Open)
		return err
	case st.Key != "":
		_, err := p.Eval(`(k) => document.activeElement.dispatchEvent(new KeyboardEvent('keydown', {key: k, bubbles: true}))`, st.Key)
		return err
	case st.Eval != "":
		_, err := p.Eval(`async () => { ` + st.Eval + ` }`)
		return err
	}
	return fmt.Errorf("step has no verb: %+v", st)
}

func addCSS(p *rod.Page, css string) error {
	_, err := p.Eval(`(c) => { const e = document.createElement('style'); e.textContent = c; document.head.appendChild(e); }`, css)
	return err
}

// scrub fails a shot whose page shows anything from the machine it ran on.
func scrub(p *rod.Page) error {
	text := p.MustEval(`() => window.__scrub()`).Str()
	var banned []string
	for _, name := range []string{"USER", "LOGNAME"} {
		banned = append(banned, os.Getenv(name))
	}
	if host, err := os.Hostname(); err == nil {
		banned = append(banned, host)
	}
	banned = append(banned, os.Getenv("DOCSHOTS_REAL_HOME"), os.TempDir())
	for _, b := range banned {
		if len(b) > 1 && strings.Contains(text, b) {
			return fmt.Errorf("page shows %q from the host", b)
		}
	}
	return nil
}

func contactSheet(browser *rod.Browser, m manifest, out string) error {
	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset=utf-8><style>body{margin:0;padding:16px;background:#fff;font:14px sans-serif;columns:3;column-gap:16px}
figure{margin:0 0 16px;break-inside:avoid;border:1px solid #ccc}img{width:100%;display:block}figcaption{padding:6px 8px;background:#eee}</style>`)
	for _, s := range m.Shots {
		abs, err := filepath.Abs(filepath.Join(out, s.Name+".png"))
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, `<figure><figcaption>%s</figcaption><img src="file://%s"></figure>`, html.EscapeString(s.Name), html.EscapeString(abs))
	}
	sheet, err := filepath.Abs(filepath.Join(out, "contact-sheet.html"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(sheet, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return rod.Try(func() {
		p := browser.MustPage("file://" + sheet)
		defer p.Close()
		p.Timeout(30*time.Second).MustSetViewport(2400, 1400, 1, false)
		p.MustWaitLoad()
		img, err := p.Screenshot(true, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng})
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(out, "contact-sheet.png"), img, 0o644); err != nil {
			panic(err)
		}
	})
}

// report prints how far each image moved from its installed copy and, with
// install, replaces those that moved more than rendering noise.
func report(m manifest, root, out string, install bool) error {
	for _, s := range m.Shots {
		fresh := filepath.Join(out, s.Name+".png")
		target := filepath.Join(root, s.Target)
		share, err := diffShare(fresh, target)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		state := "unchanged"
		if share > changedShare {
			state = fmt.Sprintf("changed %.2f%%", share*100)
			if install {
				data, err := os.ReadFile(fresh)
				if err != nil {
					return err
				}
				if err := os.WriteFile(target, data, 0o644); err != nil {
					return err
				}
				state += ", installed"
			}
		}
		fmt.Printf("%-36s %s\n", s.Name, state)
	}
	return nil
}

// diffShare is the share of pixels that differ; a missing or differently sized
// installed image counts as fully changed.
func diffShare(a, b string) (float64, error) {
	ia, err := readPNG(a)
	if err != nil {
		return 0, err
	}
	ib, err := readPNG(b)
	if errors.Is(err, os.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	if ia.Bounds() != ib.Bounds() {
		return 1, nil
	}
	r := ia.Bounds()
	changed := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if ia.At(x, y) != ib.At(x, y) {
				changed++
			}
		}
	}
	return float64(changed) / float64(r.Dx()*r.Dy()), nil
}

func readPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}
