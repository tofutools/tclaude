// drive: step-wise control of a long-lived headless Chrome for manual E2E runs.
//
// Chrome is started once (scripts/e2e/e2e.sh browser start) with --remote-debugging-port.
// Each invocation connects, acts on the active tab, and exits, so a human or
// agent can drive the UI one step at a time and inspect results between steps.
//
//	drive open <url>             navigate the active tab
//	drive tab <n>                list tabs, or switch to tab n
//	drive shot <file.png>        screenshot of the viewport
//	drive text [css]             visible text of the page (or of the first match)
//	drive click <css>            click the first element matching css
//	drive clicktext <text>       click the smallest visible element whose text contains text
//	drive type <css> <text>      focus css and type text
//	drive key <Enter|Escape|...> press a key
//	drive eval <js>              evaluate a JS expression and print the result
//	drive wait <text> [secs]     wait until page text contains text
//	drive size <w> <h>           set viewport size
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

func die(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...); os.Exit(1) }

func main() {
	if len(os.Args) < 2 {
		die("usage: drive <cmd> [args]")
	}
	port := os.Getenv("DRIVE_PORT")
	if port == "" {
		port = "19222"
	}
	u, err := launcher.ResolveURL("127.0.0.1:" + port)
	if err != nil {
		die("resolve chrome: %v", err)
	}
	b := rod.New().ControlURL(u)
	if err := b.Connect(); err != nil {
		die("connect: %v", err)
	}
	pages, err := b.Pages()
	if err != nil || len(pages) == 0 {
		die("no pages: %v", err)
	}
	cur := 0
	if raw, err := os.ReadFile(stateFile()); err == nil {
		cur, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	if cur >= len(pages) {
		cur = 0
	}
	p := pages[cur].Timeout(30 * time.Second)
	args := os.Args[2:]
	switch os.Args[1] {
	case "open":
		must(p.Navigate(args[0]))
		_ = p.WaitLoad()
		time.Sleep(1500 * time.Millisecond)
		fmt.Println("at", p.MustInfo().URL)
	case "tab":
		if len(args) == 0 {
			for i, pg := range pages {
				info := pg.MustInfo()
				fmt.Printf("%d %s %s\n", i, info.Title, info.URL)
			}
			return
		}
		must(os.WriteFile(stateFile(), []byte(args[0]), 0o600))
	case "shot":
		img, err := p.Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatPng})
		must(err)
		must(os.WriteFile(args[0], img, 0o644))
		fmt.Println("wrote", args[0])
	case "text":
		sel := "body"
		if len(args) > 0 {
			sel = args[0]
		}
		el, err := p.Element(sel)
		must(err)
		fmt.Println(el.MustText())
	case "click":
		el, err := p.Element(args[0])
		must(err)
		must(el.ScrollIntoView())
		must(el.Click(proto.InputMouseButtonLeft, 1))
		time.Sleep(700 * time.Millisecond)
		fmt.Println("clicked", args[0])
	case "clicktext":
		// Smallest visible element containing the text: buttons/links win over containers.
		res, err := p.Eval(`(t) => {
			const all = [...document.querySelectorAll('button,a,[role=button],[role=tab],summary,label,li,span,div,td')];
			const vis = all.filter(e => e.offsetParent !== null && (e.innerText||'').includes(t));
			vis.sort((a,b) => (a.innerText||'').length - (b.innerText||'').length);
			if (!vis.length) return 'NOTFOUND';
			vis[0].scrollIntoView({block:'center'}); vis[0].click();
			return vis[0].tagName + ': ' + (vis[0].innerText||'').slice(0,80);
		}`, args[0])
		must(err)
		time.Sleep(700 * time.Millisecond)
		fmt.Println(res.Value.String())
	case "type":
		el, err := p.Element(args[0])
		must(err)
		must(el.Focus())
		must(el.Input(args[1]))
		fmt.Println("typed into", args[0])
	case "key":
		k := map[string]input.Key{"Enter": input.Enter, "Escape": input.Escape, "Tab": input.Tab}[args[0]]
		must(p.Keyboard.Type(k))
		time.Sleep(500 * time.Millisecond)
		fmt.Println("key", args[0])
	case "eval":
		res, err := p.Eval("() => (" + args[0] + ")")
		must(err)
		fmt.Println(res.Value.String())
	case "wait":
		secs := 15
		if len(args) > 1 {
			secs, _ = strconv.Atoi(args[1])
		}
		deadline := time.Now().Add(time.Duration(secs) * time.Second)
		for time.Now().Before(deadline) {
			if t, err := p.Eval(`() => document.body.innerText`); err == nil && strings.Contains(t.Value.String(), args[0]) {
				fmt.Println("found", args[0])
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		die("timeout waiting for %q", args[0])
	case "size":
		w, _ := strconv.Atoi(args[0])
		h, _ := strconv.Atoi(args[1])
		must(p.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: w, Height: h, DeviceScaleFactor: 1}))
		fmt.Println("viewport", w, h)
	default:
		die("unknown cmd %s", os.Args[1])
	}
}

func stateFile() string { return os.TempDir() + "/drive-tab-" + os.Getenv("DRIVE_PORT") }

func must(err error) {
	if err != nil {
		die("%v", err)
	}
}
