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
//   - A local headless Chrome/Chromium binary. On the WSL2 dev box that is
//     Google Chrome at /usr/bin/google-chrome (-> /opt/google/chrome/chrome, a
//     real ELF x86-64 binary). A Windows Chrome under /mnt/c/... does NOT count —
//     it's the wrong process/display world. Unsandboxed macOS runs may use the
//     normal Google Chrome application binary.
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
//   - On macOS, Capture points MAC_CHROMIUM_TMPDIR at its disposable browser
//     directory unless the caller already set it. This avoids Chromium ignoring
//     TMPDIR and trying to create ProcessSingleton state under a restricted
//     _CS_DARWIN_USER_TEMP_DIR. It does not grant Chrome permission to register
//     its required Mach rendezvous service: coding-agent seatbelt sandboxes that
//     deny mach-register still cannot launch Chrome. Run there outside the agent
//     sandbox, or run the harness on Linux.
//
// # How to run
//
// The harness is a Go test gated behind an env var so `go test ./...` (CI) never
// launches a browser. The full matrix (every state × both skins) takes on the
// order of ten minutes, so the canonical invocation shards it — each shard is a
// deterministic round-robin subset that completes in a few minutes:
//
//	TCLAUDE_DASHSNAP=1 TCLAUDE_DASHSNAP_SHARD=1/4 go test ./pkg/claude/agentd/ -run TestDashSnap -v -count=1 -timeout 600s
//
// Run shards 1/4 through 4/4 to cover everything, or drop the shard variable
// and raise the timeout (e.g. -timeout 1800s) for a single full run. See
// docs/dashboard.md ("Visual smoke testing") for details.
//
// Output lands under dashsnap-out/<timestamp><shard-suffix>/ (gitignored): one
// PNG per state plus an index.html contact sheet. Open the index.html to
// eyeball everything.
//
// The state matrix the driver captures is defined in dashsnap_test.go —
// {default, wizard} skins × one state per dashboard surface/interaction under
// test. TCLAUDE_DASHSNAP_FILTER selects a substring-matched subset;
// TCLAUDE_DASHSNAP_SHARD picks a deterministic i-of-n slice of what remains.
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
	"errors"
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

// ErrBrowserUnavailable marks a failure to find, launch, or connect to a local
// Chrome/Chromium — an ENVIRONMENT gap, not a dashboard regression. The
// env-gated browser smokes t.Skip on errors.Is(err, ErrBrowserUnavailable) so
// a host without a usable browser reads as a skip, while per-state failures
// (Shot.Err) remain hard product failures.
var ErrBrowserUnavailable = errors.New("browser unavailable")

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
	// Width/Height optionally override the capture viewport for this state. A
	// zero value inherits the Config dimension. This lets one state exercise a
	// narrow responsive boundary without shrinking every later screenshot.
	Width, Height int
	// JS runs after the page's first snapshot render settles. It drives the DOM
	// into the target state (open a dialog, collapse the dock, …). An empty JS
	// captures the freshly-loaded page. A thrown error marks the shot failed but
	// does not abort the run — the sheet records the error under the tile.
	JS string
	// InitJS, when non-empty, is installed via Page.addScriptToEvaluateOnNewDocument
	// before this state's navigation and removed again when the state finishes, so
	// it runs in the fresh document BEFORE any dashboard module executes. Use it
	// for deterministic seams the page scripts capture at bootstrap (e.g. wrapping
	// window.fetch to hold one request open) — the post-load JS above runs too
	// late for those. Scoped strictly to its own state: the reused page never
	// carries it into the next navigation.
	InitJS string
	// Actions run after JS using Chrome's input domain. Supported kinds: click,
	// dblclick, input, type-text, key-down, key-up, key, mouse-down,
	// mouse-down-at, move-by, move-to-at, mouse-up, eval, popup-eval,
	// popup-close.
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
	// ShowScrollbars omits Chrome's default --hide-scrollbars capture flag.
	// Keep false for ordinary layout snapshots; opt in only when scrollbar
	// rendering itself is part of the visual contract under test.
	ShowScrollbars bool
	// GrantClipboard grants the BaseURL origin clipboard read/write permission
	// before any state runs, so copy flows can assert the REAL
	// navigator.clipboard round trip instead of the legacy execCommand
	// fallback headless Chrome would otherwise force.
	GrantClipboard bool
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
	// Elapsed is the wall-clock cost of this state (navigate → settle → JS →
	// screenshot), recorded so budget drift is visible on every sheet.
	Elapsed time.Duration
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
// the known Linux default path, then rod's own LookPath over the usual install
// spots for the current platform.
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
	return "", fmt.Errorf("%w: no Chrome/Chromium found (looked at TCLAUDE_DASHSNAP_CHROME, %s, and the platform's usual install locations); "+
		"install one or set TCLAUDE_DASHSNAP_CHROME", ErrBrowserUnavailable, defaultChromeBin)
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
	browserRoot, err := os.MkdirTemp(cfg.OutDir, ".dashsnap-chrome-")
	if err != nil {
		return nil, fmt.Errorf("create browser temp dir: %w", err)
	}
	// Registered before the launcher's reap defer below so browser processes are
	// dead before their profile/temp directory is removed (defer is LIFO).
	defer func() { _ = os.RemoveAll(browserRoot) }()

	l := launcher.New().
		Bin(chromeBin).
		UserDataDir(filepath.Join(browserRoot, "profile")).
		Headless(true).
		NoSandbox(true).
		Leakless(false).
		Set("disable-gpu")
	configureScrollbarVisibility(l, cfg.ShowScrollbars)
	l.Set("window-size", fmt.Sprintf("%d,%d", cfg.Width, cfg.Height))
	configurePlatformChrome(l, browserRoot)
	// Leakless(false) disables rod's reaper watchdog, so once Launch may have
	// spawned a process we must kill it ourselves or orphan a headless Chrome.
	// Registered BEFORE Launch: Launch can fail after the spawn (e.g. "Failed to
	// get the debug url"), and Kill is a guarded no-op when nothing started. It
	// also precedes browser.Close's defer so it runs LAST (LIFO): a graceful
	// Close on the happy path, then this reaps whatever's left.
	defer l.Kill()
	controlURL, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("launch chrome (%s): %w: %w%s", chromeBin, ErrBrowserUnavailable, err, platformLaunchHint)
	}

	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		return nil, fmt.Errorf("connect to chrome: %w: %w", ErrBrowserUnavailable, err)
	}
	defer func() { _ = browser.Close() }()

	if cfg.GrantClipboard {
		err := proto.BrowserGrantPermissions{
			Permissions: []proto.BrowserPermissionType{
				proto.BrowserPermissionTypeClipboardReadWrite,
				proto.BrowserPermissionTypeClipboardSanitizedWrite,
			},
			Origin: cfg.BaseURL,
		}.Call(browser)
		if err != nil {
			return nil, fmt.Errorf("grant clipboard permission for %s: %w", cfg.BaseURL, err)
		}
	}

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
		start := time.Now()
		png, tainted, capErr := captureState(page, cfg, st)
		shot.Elapsed = time.Since(start)
		if tainted {
			// The state's InitJS script could not be confirmed removed, so this
			// page would carry it into every later navigation. Isolation is the
			// seam's contract: replace the page rather than continue on hidden
			// state. A failed replacement aborts the run — there is no clean page
			// to continue on.
			if renewErr := rod.Try(func() {
				_ = page.Close()
				page = browser.MustPage("")
				page.MustSetViewport(cfg.Width, cfg.Height, 1, false)
			}); renewErr != nil {
				shot.Err = errors.Join(capErr, renewErr).Error()
				shots = append(shots, shot)
				return shots, fmt.Errorf("replace page after unconfirmed InitJS removal: %w", renewErr)
			}
		}
		if capErr != nil {
			shot.Err = capErr.Error()
			var evalErr *rod.EvalError
			if errors.As(capErr, &evalErr) && evalErr.Exception != nil && evalErr.Exception.Description != "" {
				shot.Err = evalErr.Exception.Description
			}
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

func configureScrollbarVisibility(l *launcher.Launcher, show bool) {
	if !show {
		l.Set("hide-scrollbars")
	}
}

// captureState navigates the reused page to the dashboard (optionally
// wizard-skinned), waits for the first render, runs the state's JS, settles, and
// returns the PNG bytes. All rod calls run inside rod.Try (so a driving failure
// becomes a returned error, not a panic) and under a per-state deadline (so one
// stuck state can't consume the whole matrix's budget).
//
// tainted reports that the state's InitJS script was installed but its removal
// could not be CONFIRMED — the page may still carry the script, so the caller
// must not reuse it for later states. A confirmed removal failure also joins
// the returned error, so a leak can never pass silently.
//
// It deliberately does NOT WaitDOMStable: the dashboard repaints on a 2s poll
// (+ wizard FX), so the DOM is never "stable" and the wait would block for
// seconds every state. Waiting for the ready selector + a fixed settle yields a
// coherent frame far faster.
func captureState(page *rod.Page, cfg Config, st State) (png []byte, tainted bool, err error) {
	var cleanupErr error
	err = rod.Try(func() {
		sp := page.Timeout(time.Duration(cfg.StateTimeoutMS) * time.Millisecond)
		defer sp.CancelTimeout() // release the per-state timeout's timer
		if strings.TrimSpace(st.InitJS) != "" {
			res, initErr := proto.PageAddScriptToEvaluateOnNewDocument{Source: st.InitJS}.Call(sp)
			if initErr != nil {
				panic(initErr)
			}
			tainted = true
			// Remove under a FRESH bounded context: the state's own deadline may
			// already be spent when this defer runs, and the removal must neither
			// inherit that dead deadline nor hang the matrix on a stalled CDP.
			// Only a confirmed removal clears tainted; a failure is surfaced (via
			// cleanupErr below) and makes the caller retire the page.
			defer func() {
				cp := page.Timeout(10 * time.Second)
				defer cp.CancelTimeout()
				if rmErr := (proto.PageRemoveScriptToEvaluateOnNewDocument{Identifier: res.Identifier}).Call(cp); rmErr != nil {
					cleanupErr = fmt.Errorf("remove InitJS script (would leak into later states): %w", rmErr)
					return
				}
				tainted = false
			}()
		}
		width, height := cfg.Width, cfg.Height
		if st.Width > 0 {
			width = st.Width
		}
		if st.Height > 0 {
			height = st.Height
		}
		sp.MustSetViewport(width, height, 1, false)

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
			case "dblclick":
				// A real double click (two clicks in one gesture) — used to
				// word-select text inside an xterm. Position comes from JS
				// ({x,y}) when set, else the selector's element centre.
				point := actionPoint(sp, action)
				if err := sp.Mouse.MoveTo(point); err != nil {
					panic(err)
				}
				if err := sp.Mouse.Click(proto.InputMouseButtonLeft, 2); err != nil {
					panic(err)
				}
			case "input":
				sp.MustElement(action.Selector).MustInput(action.Text)
			case "type-text":
				// Types Text into the focused element as REAL per-key events
				// (keydown/keyup with text), which is what an xterm consumes.
				// Covers the single-rune keys of a US layout plus \r/\n
				// (Enter) — enough for terminal smokes; unknown runes panic
				// via input.Key.Info, failing the state loudly.
				for _, r := range action.Text {
					key := input.Key(r)
					if r == '\r' || r == '\n' {
						key = input.Enter
					}
					if err := sp.Keyboard.Type(key); err != nil {
						panic(err)
					}
				}
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
			case "mouse-down-at":
				var x, y float64
				if strings.TrimSpace(action.JS) != "" {
					position := sp.MustEval(`() => { ` + action.JS + ` }`)
					x, y = position.Get("x").Num(), position.Get("y").Num()
				} else {
					position := sp.MustElement(action.Selector).MustEval(`() => {
                      const rect = this.getBoundingClientRect();
                      return {x: rect.left, y: rect.top};
                    }`)
					x, y = position.Get("x").Num()+action.DX, position.Get("y").Num()+action.DY
				}
				point := proto.NewPoint(x, y)
				if err := sp.Mouse.MoveTo(point); err != nil {
					panic(err)
				}
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
			case "move-to-at":
				position := sp.MustEval(`() => { ` + action.JS + ` }`)
				steps := action.Steps
				if steps < 1 {
					steps = 1
				}
				if err := sp.Mouse.MoveLinear(proto.NewPoint(position.Get("x").Num(), position.Get("y").Num()), steps); err != nil {
					panic(err)
				}
			case "mouse-up":
				sp.Mouse.MustUp(proto.InputMouseButtonLeft)
				mouseDown = false
			case "eval":
				sp.MustEval(`() => { ` + action.JS + ` }`)
			case "popup-eval":
				// Evaluates JS on ANOTHER page of the same browser — one whose
				// URL contains Selector — so a state can assert inside a
				// window.open pop-out (e.g. the /terminals?solo=1 page). All
				// popup work runs within what REMAINS of this state's budget.
				budget := popupBudget(sp, cfg)
				popup := findPopupPage(sp, action.Selector, budget)
				pp := popup.Timeout(budget)
				pp.MustWaitLoad()
				pp.MustEval(`() => { ` + action.JS + ` }`)
				pp.CancelTimeout()
			case "popup-close":
				// Closes the pop-out page whose URL contains Selector,
				// accepting a beforeunload confirmation if the page raises one
				// (the terminal pop-out arms one while a pane is open). The
				// close runs within the state's remaining budget so a
				// mishandled dialog fails this state instead of hanging the
				// whole capture.
				budget := popupBudget(sp, cfg)
				popup := findPopupPage(sp, action.Selector, budget)
				pp := popup.Timeout(budget)
				closePopupPage(pp)
				pp.CancelTimeout()
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
	if cleanupErr != nil {
		err = errors.Join(err, cleanupErr)
	}
	return png, tainted, err
}

// actionPoint resolves where a positional action lands: the {x,y} object the
// action's JS returns when set, else the centre of the Selector's element.
func actionPoint(sp *rod.Page, action BrowserAction) proto.Point {
	if strings.TrimSpace(action.JS) != "" {
		position := sp.MustEval(`() => { ` + action.JS + ` }`)
		return proto.NewPoint(position.Get("x").Num(), position.Get("y").Num())
	}
	position := sp.MustElement(action.Selector).MustEval(`() => {
      const rect = this.getBoundingClientRect();
      return {x: rect.left + rect.width / 2, y: rect.top + rect.height / 2};
    }`)
	return proto.NewPoint(position.Get("x").Num(), position.Get("y").Num())
}

// popupBudget returns how much of the enclosing state's deadline remains —
// popup discovery/eval/close must fit INSIDE the per-state budget, not restart
// a fresh one (browser-level calls do not inherit sp's timeout context).
func popupBudget(sp *rod.Page, cfg Config) time.Duration {
	budget := time.Duration(cfg.StateTimeoutMS) * time.Millisecond
	if deadline, ok := sp.GetContext().Deadline(); ok {
		if remaining := time.Until(deadline); remaining < budget {
			budget = remaining
		}
	}
	return budget
}

// findPopupPage polls the browser's page list for a page other than sp whose
// URL contains urlPart, panicking (into the state's rod.Try) when none appears
// within budget. Used by the popup-eval / popup-close actions.
func findPopupPage(sp *rod.Page, urlPart string, budget time.Duration) *rod.Page {
	deadline := time.Now().Add(budget)
	for {
		pages, err := sp.Browser().Pages()
		if err != nil {
			panic(fmt.Errorf("list browser pages: %w", err))
		}
		for _, candidate := range pages {
			if candidate.TargetID == sp.TargetID {
				continue
			}
			info, err := candidate.Info()
			if err != nil {
				continue // page may be mid-navigation or already gone
			}
			if strings.Contains(info.URL, urlPart) {
				return candidate
			}
		}
		if time.Now().After(deadline) {
			panic(fmt.Sprintf("no popup page with URL containing %q appeared", urlPart))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// closePopupPage closes a popup, accepting the beforeunload confirmation if
// the page raises one (rod's Page.Close otherwise waits on that dialog
// forever). The dialog waiter runs in a goroutine because the dialog may or
// may not appear; if it never does, the waiter ends with the page's session.
// A FAILED dialog accept is still surfaced: Close then cannot complete, so it
// errors out under the caller-provided timeout page instead of hanging.
func closePopupPage(popup *rod.Page) {
	wait, handle := popup.HandleDialog()
	go func() {
		// The page (and its event stream) dying without a dialog can panic the
		// waiter; that is the expected no-dialog outcome, so swallow it.
		defer func() { _ = recover() }()
		wait()
		_ = handle(&proto.PageHandleJavaScriptDialog{Accept: true})
	}()
	if err := popup.Close(); err != nil {
		panic(fmt.Errorf("close popup page: %w", err))
	}
}

func browserKey(name string) input.Key {
	switch strings.ToLower(name) {
	case "control", "ctrl":
		return input.ControlLeft
	case "meta", "command", "cmd":
		return input.MetaLeft
	case "c":
		return input.KeyC
	case "v":
		return input.KeyV
	case "delete":
		return input.Delete
	case "backspace":
		return input.Backspace
	case "enter":
		return input.Enter
	case "escape":
		return input.Escape
	case "tab":
		return input.Tab
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
	var total time.Duration
	for _, s := range shots {
		if s.Err == "" {
			ok++
		} else {
			failed++
		}
		total += s.Elapsed
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
	fmt.Fprintf(&b, ` · captured in %s`, html.EscapeString(total.Round(time.Second).String()))
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
		fmt.Fprintf(&b, `<div class="row"><span class="key">%s</span><span class="dur">%.1fs</span><span class="badge %s">%s</span></div>`,
			html.EscapeString(s.State.Title), s.Elapsed.Seconds(), skinClass, skin)
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
.key{font-weight:600;color:#e6edf3;flex:1}
.dur{color:#6e7681;font-size:11px;white-space:nowrap}
.cap{margin:6px 0 0;color:#8b949e;font-size:12.5px}
.err{margin:6px 0 0;color:#f85149;font-size:12.5px;font-family:ui-monospace,monospace}
.badge{font-size:11px;padding:2px 8px;border-radius:999px;white-space:nowrap}
.badge-default{background:#1f6feb33;color:#79c0ff;border:1px solid #1f6feb55}
.badge-wizard{background:#a371f733;color:#d2a8ff;border:1px solid #a371f755}
`
