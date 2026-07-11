// Package dashsnap is a manually-run VISUAL SMOKE HARNESS for the agentd
// dashboard (JOH-386). It drives a real headless Chrome over the real dashboard
// handler and writes both-skins screenshots so a human — or an agent whose
// sandbox can't render — can eyeball the CSS cascade that string pins and
// `node --check` are blind to. It exists because the palette epic's only real
// visual blockers (a global `section{display:none}`; an inert `[hidden]` mode
// chooser) were caught solely by a human reading CSS.
//
// # Why rod, and why it lives here and nowhere else
//
// This package is the ONLY importer of the browser driver (github.com/go-rod/rod).
// It is imported ONLY by the env-gated driver test (dashsnap_test.go), never by
// the tclaude binary — so the browser dep stays OUT of the shipped binary's
// dependency graph. Proof: `go list -deps ./ | grep -i rod` is empty (the root
// package is the tclaude binary). rod was chosen over chromedp because it was
// already present in the module cache and drives a system Chrome offline; see the
// PR for the full rationale.
//
// # Runtime prerequisites (discovered on the JOH-386 WSL2 box — the next
// runner should not have to rediscover these)
//
//   - A LINUX-side headless Chrome/Chromium binary. On the dev box that is
//     Google Chrome at /usr/bin/google-chrome (-> /opt/google/chrome/chrome, a
//     real ELF x86-64 binary). A Windows Chrome under /mnt/c/... does NOT count —
//     it's the wrong process/display world.
//   - We pin Chrome via launcher.Bin() so rod NEVER tries to auto-download a
//     browser (there may be no network for that). Override the path with
//     TCLAUDE_DASHSNAP_CHROME=/path/to/chrome if it lives elsewhere.
//   - --no-sandbox is REQUIRED here: the agent sandbox can't grant Chrome its own
//     user namespace, so without it Chrome exits immediately.
//   - Leakless(false): rod's leakless helper would spawn a watchdog subprocess;
//     disabling it avoids that friction in a restricted sandbox. We MustClose the
//     browser ourselves instead.
//   - Chrome prints harmless stderr noise in this environment — crashpad
//     "Read-only file system" lines and dbus/UPower "ServiceUnknown" lines. They
//     are NOT failures; screenshots still render. Ignore them.
//
// # How to run
//
// The harness is a Go test gated behind an env var so `go test ./...` (CI) never
// launches a browser. Run it explicitly:
//
//	TCLAUDE_DASHSNAP=1 go test ./pkg/claude/agentd/ -run TestDashSnap -v -count=1 -timeout 300s
//
// Output lands under dashsnap-out/<timestamp>/ (gitignored): one PNG per state
// plus an index.html contact sheet. Open the index.html to eyeball everything.
//
// The state matrix the driver captures is defined in dashsnap_test.go —
// {default, wizard} skins × {groups tab, palette dock open, dock collapsed,
// summon dialog normal/reinforce/copy}.
//
// # Known trap: headless hover
//
// Headless Chrome matches `(hover: hover)` media queries but has no live cursor,
// so JS-driven `:hover` substitutes (the dashboard's `.quick-hover` class) must
// be stamped manually from a state's JS (dispatch a `mouseover`, or add the
// class). States that depend on a real hover the harness can't reproduce are
// skipped and noted in their caption.
//
// # Known cosmetic limitation: emoji glyphs
//
// If the host lacks a color-emoji font (e.g. fonts-noto-color-emoji), the dozens
// of emoji the dashboard uses for icons render as tofu boxes. This is purely
// cosmetic — layout, the CSS cascade, theming and the both-skins verification
// (the whole point of the harness) are unaffected. Install a color-emoji font to
// make the icons legible.
package dashsnap

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

// BrowserAction is one trusted DevTools input step (or an assertion evaluated
// between steps). Unlike dispatchEvent-based fixture JS, click/drag/key actions
// exercise Chrome's real pointer capture, focus, blur and click sequencing.
type BrowserAction struct {
	Kind     string
	Selector string
	Key      string
	Text     string
	JS       string
	DX, DY   float64
	Steps    int
}

// State is one screenshot the harness captures: a named dashboard state reached
// by loading the dashboard (optionally in the wizard skin) and then running a
// small JS snippet that clicks/dispatches/mutates the DOM into the target state.
type State struct {
	// Key is the filename-safe slug for the PNG (e.g. "default-dock-open").
	Key string
	// Title is the short human label shown on the contact sheet.
	Title string
	// Caption explains what the shot proves (and notes any caveat/skip reason).
	Caption string
	// Wizard loads the dashboard in the wizard skin (via ?wizard=1) before JS.
	Wizard bool
	// JS runs after the page's first snapshot render settles. It drives the DOM
	// into the target state (open a dialog, collapse the dock, …). An empty JS
	// captures the freshly-loaded page. A thrown error marks the shot failed but
	// does not abort the run — the sheet records the error under the tile.
	JS string
	// Actions run after JS using Chrome's input domain. Supported kinds: click,
	// key-down, key-up, key, mouse-down, move-by, mouse-up, eval.
	Actions []BrowserAction
	// SettleMS optionally overrides the post-JS settle wait (ms) for states with
	// animations/transitions that need longer to paint. 0 uses cfg.SettleMS.
	SettleMS int
}

// Config drives a Capture run.
type Config struct {
	// BaseURL is the dashboard origin to screenshot (an httptest server URL).
	BaseURL string
	// OutDir is where PNGs + index.html are written (created if missing).
	OutDir string
	// ChromeBin overrides the Chrome binary; "" resolves via env then defaults.
	ChromeBin string
	// Width/Height is the browser window (and viewport) size. 0 uses defaults.
	Width, Height int
	// States is the matrix to capture, in order.
	States []State
	// ReadySelector is a CSS selector that only exists after the first snapshot
	// render; Capture waits for it after each navigation. "" uses a default.
	ReadySelector string
	// StateTimeoutMS bounds the WHOLE per-state sequence (navigate → ready →
	// settle → JS → screenshot), so one stuck state fails fast instead of eating
	// the matrix's budget. 0 uses a default.
	StateTimeoutMS int
	// LoadSettleMS is a fixed pause after ReadySelector appears, letting the
	// morph reconciler + async renders settle. 0 uses a default.
	LoadSettleMS int
	// SettleMS is the default post-JS settle wait. 0 uses a default.
	SettleMS int
}

// Shot is the outcome of capturing one State.
type Shot struct {
	State State
	// File is the PNG's basename within OutDir ("" if the shot failed early).
	File string
	// Err is a non-empty message if this state failed to capture; the run
	// continues so one bad state never sinks the whole sheet.
	Err string
}

const (
	defaultChromeBin      = "/usr/bin/google-chrome"
	defaultWidth          = 1680
	defaultHeight         = 1050
	defaultReadySelector  = `details[data-dnd-target-group]`
	defaultStateTimeoutMS = 25000
	defaultLoadSettleMS   = 700
	defaultSettleMS       = 500
)

// resolveChrome finds a Chrome binary WITHOUT ever triggering rod's auto-download
// (there may be no network for it). Order: explicit arg, TCLAUDE_DASHSNAP_CHROME,
// the known default path, then rod's own LookPath over the usual install spots.
func resolveChrome(explicit string) (string, error) {
	candidates := []string{
		explicit,
		os.Getenv("TCLAUDE_DASHSNAP_CHROME"),
		defaultChromeBin,
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}
	// rod's finder checks $PATH + common locations. It does NOT download.
	if p, ok := launcher.LookPath(); ok {
		return p, nil
	}
	return "", fmt.Errorf("no Linux Chrome/Chromium found (looked at TCLAUDE_DASHSNAP_CHROME, %s, and $PATH); "+
		"install one or set TCLAUDE_DASHSNAP_CHROME", defaultChromeBin)
}

// Capture launches one headless Chrome, walks the state matrix, writes a PNG per
// state and an index.html contact sheet into cfg.OutDir, and returns the per-shot
// outcomes. A launch/connect failure returns an error; a per-state failure is
// recorded on that Shot and the run continues.
func Capture(cfg Config) ([]Shot, error) {
	cfg = withDefaults(cfg)

	chromeBin, err := resolveChrome(cfg.ChromeBin)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("create out dir: %w", err)
	}

	l := launcher.New().
		Bin(chromeBin).
		Headless(true).
		NoSandbox(true).
		Leakless(false).
		Set("disable-gpu").
		Set("hide-scrollbars").
		Set("window-size", fmt.Sprintf("%d,%d", cfg.Width, cfg.Height))
	// Leakless(false) disables rod's reaper watchdog, so once Launch may have
	// spawned a process we must kill it ourselves or orphan a headless Chrome.
	// Registered BEFORE Launch: Launch can fail after the spawn (e.g. "Failed to
	// get the debug url"), and Kill is a guarded no-op when nothing started. It
	// also precedes browser.Close's defer so it runs LAST (LIFO): a graceful
	// Close on the happy path, then this reaps whatever's left.
	defer l.Kill()
	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("launch chrome (%s): %w", chromeBin, err)
	}

	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		return nil, fmt.Errorf("connect to chrome: %w", err)
	}
	defer func() { _ = browser.Close() }()

	// One reused page (navigated per state) — creating/closing a target per state
	// was the bulk of the wall-clock. A full navigation reloads the document, so
	// residual body classes / DOM from the previous state are cleared. Wrapped in
	// rod.Try so a page-create failure returns an error (matching this function's
	// contract) rather than panicking out.
	var page *rod.Page
	if err := rod.Try(func() {
		page = browser.MustPage("")
		page.MustSetViewport(cfg.Width, cfg.Height, 1, false)
	}); err != nil {
		return nil, fmt.Errorf("open page: %w", err)
	}

	shots := make([]Shot, 0, len(cfg.States))
	for _, st := range cfg.States {
		shot := Shot{State: st}
		png, capErr := captureState(page, cfg, st)
		if capErr != nil {
			shot.Err = capErr.Error()
			shots = append(shots, shot)
			continue
		}
		file := st.Key + ".png"
		if err := os.WriteFile(filepath.Join(cfg.OutDir, file), png, 0o644); err != nil {
			shot.Err = "write png: " + err.Error()
			shots = append(shots, shot)
			continue
		}
		shot.File = file
		shots = append(shots, shot)
	}

	if err := writeContactSheet(cfg, shots); err != nil {
		return shots, fmt.Errorf("write contact sheet: %w", err)
	}
	return shots, nil
}

// captureState navigates the reused page to the dashboard (optionally
// wizard-skinned), waits for the first render, runs the state's JS, settles, and
// returns the PNG bytes. All rod calls run inside rod.Try (so a driving failure
// becomes a returned error, not a panic) and under a per-state deadline (so one
// stuck state can't consume the whole matrix's budget).
//
// It deliberately does NOT WaitDOMStable: the dashboard repaints on a 2s poll
// (+ wizard FX), so the DOM is never "stable" and the wait would block for
// seconds every state. Waiting for the ready selector + a fixed settle yields a
// coherent frame far faster.
func captureState(page *rod.Page, cfg Config, st State) (png []byte, err error) {
	err = rod.Try(func() {
		sp := page.Timeout(time.Duration(cfg.StateTimeoutMS) * time.Millisecond)
		defer sp.CancelTimeout() // release the per-state timeout's timer

		url := cfg.BaseURL
		if st.Wizard {
			url += "?wizard=1"
		}
		sp.MustNavigate(url)
		sp.MustWaitLoad()

		// Wait for the first snapshot render to materialise a real group box
		// (the dock + summon states all need at least one drop-target group).
		sp.MustElement(cfg.ReadySelector)
		sleep(sp, cfg.LoadSettleMS)

		if strings.TrimSpace(st.JS) != "" {
			// Wrap so a selector-not-found surfaces as a clean JS error string.
			sp.MustEval(`() => { ` + st.JS + ` }`)
		}

		mouseDown := false
		pressed := map[input.Key]bool{}
		defer func() {
			if mouseDown {
				_ = sp.Mouse.Up(proto.InputMouseButtonLeft, 1)
			}
			for key := range pressed {
				_ = sp.Keyboard.Release(key)
			}
		}()
		for _, action := range st.Actions {
			switch action.Kind {
			case "click":
				position := sp.MustElement(action.Selector).MustEval(`() => {
                  if (this instanceof SVGPathElement) {
                    const point = this.getPointAtLength(this.getTotalLength() / 2);
                    const screen = new DOMPoint(point.x, point.y).matrixTransform(this.getScreenCTM());
                    return {x: screen.x, y: screen.y};
                  }
                  const rect = this.getBoundingClientRect();
                  return {x: rect.left + rect.width / 2, y: rect.top + rect.height / 2};
                }`)
				point := proto.NewPoint(position.Get("x").Num(), position.Get("y").Num())
				if err := sp.Mouse.MoveTo(point); err != nil {
					panic(err)
				}
				if err := sp.Mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
					panic(err)
				}
			case "input":
				sp.MustElement(action.Selector).MustInput(action.Text)
			case "key-down":
				key := browserKey(action.Key)
				if err := sp.Keyboard.Press(key); err != nil {
					panic(err)
				}
				pressed[key] = true
			case "key-up":
				key := browserKey(action.Key)
				if err := sp.Keyboard.Release(key); err != nil {
					panic(err)
				}
				delete(pressed, key)
			case "key":
				sp.Keyboard.MustType(browserKey(action.Key))
			case "mouse-down":
				sp.MustElement(action.Selector).MustHover()
				sp.Mouse.MustDown(proto.InputMouseButtonLeft)
				mouseDown = true
			case "move-by":
				position := sp.Mouse.Position()
				steps := action.Steps
				if steps < 1 {
					steps = 1
				}
				if err := sp.Mouse.MoveLinear(proto.NewPoint(position.X+action.DX, position.Y+action.DY), steps); err != nil {
					panic(err)
				}
			case "mouse-up":
				sp.Mouse.MustUp(proto.InputMouseButtonLeft)
				mouseDown = false
			case "eval":
				sp.MustEval(`() => { ` + action.JS + ` }`)
			default:
				panic(fmt.Sprintf("unknown browser action %q", action.Kind))
			}
		}

		settle := cfg.SettleMS
		if st.SettleMS > 0 {
			settle = st.SettleMS
		}
		sleep(sp, settle)

		png = sp.MustScreenshot()
	})
	return png, err
}

func browserKey(name string) input.Key {
	switch strings.ToLower(name) {
	case "control", "ctrl":
		return input.ControlLeft
	case "delete":
		return input.Delete
	case "backspace":
		return input.Backspace
	case "enter":
		return input.Enter
	case "escape":
		return input.Escape
	default:
		panic(fmt.Sprintf("unknown browser key %q", name))
	}
}

// sleep pauses for ms by awaiting a setTimeout promise IN the page (Page.Eval
// enables AwaitPromise), so the wait runs on the browser's clock and the JS
// stays ordered — not a host-side time.Sleep.
func sleep(page *rod.Page, ms int) {
	if ms <= 0 {
		return
	}
	page.MustEval(`(ms) => new Promise(r => setTimeout(r, ms))`, ms)
}

func withDefaults(cfg Config) Config {
	if cfg.Width == 0 {
		cfg.Width = defaultWidth
	}
	if cfg.Height == 0 {
		cfg.Height = defaultHeight
	}
	if cfg.ReadySelector == "" {
		cfg.ReadySelector = defaultReadySelector
	}
	if cfg.StateTimeoutMS == 0 {
		cfg.StateTimeoutMS = defaultStateTimeoutMS
	}
	if cfg.LoadSettleMS == 0 {
		cfg.LoadSettleMS = defaultLoadSettleMS
	}
	if cfg.SettleMS == 0 {
		cfg.SettleMS = defaultSettleMS
	}
	return cfg
}

// writeContactSheet renders a self-contained dark-themed index.html listing every
// shot (image + title + caption + skin badge + any error) for fast eyeballing.
func writeContactSheet(cfg Config, shots []Shot) error {
	var b strings.Builder
	ok, failed := 0, 0
	for _, s := range shots {
		if s.Err == "" {
			ok++
		} else {
			failed++
		}
	}

	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>dashsnap contact sheet</title><style>`)
	b.WriteString(contactCSS)
	b.WriteString(`</style></head><body>`)
	b.WriteString(`<header><h1>dashsnap — dashboard visual smoke sheet</h1>`)
	fmt.Fprintf(&b, `<p class="meta">%d states · <span class="ok">%d ok</span>`,
		len(shots), ok)
	if failed > 0 {
		fmt.Fprintf(&b, ` · <span class="fail">%d failed</span>`, failed)
	}
	fmt.Fprintf(&b, ` · generated %s</p></header>`,
		html.EscapeString(time.Now().Format("2006-01-02 15:04:05 MST")))
	b.WriteString(`<main class="grid">`)

	for _, s := range shots {
		skin := "default"
		skinClass := "badge-default"
		if s.State.Wizard {
			skin, skinClass = "wizard", "badge-wizard"
		}
		b.WriteString(`<figure class="card">`)
		if s.File != "" {
			fmt.Fprintf(&b, `<a href="%s" target="_blank"><img loading="lazy" src="%s" alt="%s"></a>`,
				html.EscapeString(s.File), html.EscapeString(s.File),
				html.EscapeString(s.State.Title))
		} else {
			b.WriteString(`<div class="noimg">no image</div>`)
		}
		b.WriteString(`<figcaption>`)
		fmt.Fprintf(&b, `<div class="row"><span class="key">%s</span><span class="badge %s">%s</span></div>`,
			html.EscapeString(s.State.Title), skinClass, skin)
		if s.State.Caption != "" {
			fmt.Fprintf(&b, `<p class="cap">%s</p>`, html.EscapeString(s.State.Caption))
		}
		if s.Err != "" {
			fmt.Fprintf(&b, `<p class="err">⚠ %s</p>`, html.EscapeString(s.Err))
		}
		b.WriteString(`</figcaption></figure>`)
	}

	b.WriteString(`</main></body></html>`)
	return os.WriteFile(filepath.Join(cfg.OutDir, "index.html"), []byte(b.String()), 0o644)
}

const contactCSS = `
:root{color-scheme:dark}
*{box-sizing:border-box}
body{margin:0;background:#0d1117;color:#c9d1d9;font:14px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
header{padding:20px 24px;border-bottom:1px solid #21262d;position:sticky;top:0;background:#0d1117;z-index:1}
h1{margin:0 0 4px;font-size:18px}
.meta{margin:0;color:#8b949e;font-size:13px}
.ok{color:#3fb950}.fail{color:#f85149}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(420px,1fr));gap:18px;padding:24px}
.card{margin:0;background:#161b22;border:1px solid #21262d;border-radius:10px;overflow:hidden;display:flex;flex-direction:column}
.card img{display:block;width:100%;height:auto;background:#010409;border-bottom:1px solid #21262d}
.noimg{padding:60px 0;text-align:center;color:#6e7681;background:#010409;border-bottom:1px solid #21262d}
figcaption{padding:12px 14px}
.row{display:flex;align-items:center;justify-content:space-between;gap:8px}
.key{font-weight:600;color:#e6edf3}
.cap{margin:6px 0 0;color:#8b949e;font-size:12.5px}
.err{margin:6px 0 0;color:#f85149;font-size:12.5px;font-family:ui-monospace,monospace}
.badge{font-size:11px;padding:2px 8px;border-radius:999px;white-space:nowrap}
.badge-default{background:#1f6feb33;color:#79c0ff;border:1px solid #1f6feb55}
.badge-wizard{background:#a371f733;color:#d2a8ff;border:1px solid #a371f755}
`
