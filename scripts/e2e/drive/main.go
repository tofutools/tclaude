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
//	drive caption [text]         show (or clear) a caption banner for recordings
//	drive record <dir> [out.mp4] screencast the tab into <dir> until SIGINT/SIGTERM,
//	                             then encode out.mp4 with ffmpeg when given
//
// DRIVE_SHOW=1 outlines each click/type target briefly first, so recordings
// show what is being clicked.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	need := map[string]int{"open": 1, "shot": 1, "click": 1, "clicktext": 1, "type": 2, "key": 1, "eval": 1, "wait": 1, "size": 2, "record": 1}
	if len(args) < need[os.Args[1]] {
		die("%s needs %d argument(s)", os.Args[1], need[os.Args[1]])
	}
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
		show(p, el, "")
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
			vis[0].scrollIntoView({block:'center'});
			window.__driveTarget = vis[0];
			return vis[0].tagName + ': ' + (vis[0].innerText||'').slice(0,80);
		}`, args[0])
		must(err)
		if res.Value.String() != "NOTFOUND" {
			el, err := p.Element("body")
			must(err)
			show(p, el, "window.__driveTarget")
			_, err = p.Eval(`() => window.__driveTarget.click()`)
			must(err)
		}
		time.Sleep(700 * time.Millisecond)
		fmt.Println(res.Value.String())
	case "type":
		el, err := p.Element(args[0])
		must(err)
		show(p, el, "")
		must(el.Focus())
		must(el.Input(args[1]))
		fmt.Println("typed into", args[0])
	case "key":
		k, ok := map[string]input.Key{"Enter": input.Enter, "Escape": input.Escape, "Tab": input.Tab}[args[0]]
		if !ok {
			die("unknown key %s (Enter, Escape or Tab)", args[0])
		}
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
		p = pages[cur].Timeout(time.Duration(secs+5) * time.Second)
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
	case "caption":
		_, err := p.Eval(`(t) => {
			let c = document.getElementById('drive-caption');
			if (!t) { if (c) c.remove(); return; }
			if (!c) {
				c = document.createElement('div'); c.id = 'drive-caption';
				c.style.cssText = 'position:fixed;left:50%;bottom:28px;transform:translateX(-50%);z-index:2147483647;' +
					'background:rgba(20,24,32,.92);color:#fff;font:600 18px system-ui,sans-serif;padding:10px 18px;' +
					'border-radius:10px;border:1px solid #58a6ff;box-shadow:0 4px 18px rgba(0,0,0,.5);pointer-events:none;max-width:80vw;text-align:center';
				document.body.appendChild(c);
			}
			c.textContent = t;
		}`, strings.Join(args, " "))
		must(err)
		time.Sleep(300 * time.Millisecond)
	case "record":
		record(p, args)
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

// show outlines the target for a moment when DRIVE_SHOW=1, so a recording
// shows what is about to be clicked. js, when set, names the target instead.
func show(p *rod.Page, el *rod.Element, js string) {
	if os.Getenv("DRIVE_SHOW") != "1" {
		return
	}
	const outline = `(e) => { e.scrollIntoView({block:'center'}); const o = e.style.outline; e.style.outline = '3px solid #f0b400';
		e.style.outlineOffset = '2px'; setTimeout(() => { e.style.outline = o; }, 900); }`
	if js != "" {
		_, _ = p.Eval(`() => (` + outline + `)(` + js + `)`)
	} else {
		_, _ = el.Eval(`function() { (` + outline + `)(this) }`)
	}
	time.Sleep(900 * time.Millisecond)
}

// record saves screencast frames with their arrival times. Chrome only sends
// a frame when the page changes, so the ffmpeg concat list carries each
// frame's on-screen duration.
func record(p *rod.Page, args []string) {
	if len(args) == 0 {
		die("usage: drive record <dir> [out.mp4]")
	}
	dir := args[0]
	must(os.MkdirAll(dir, 0o755))
	type frame struct {
		name string
		at   time.Time
	}
	var frames []frame
	p = p.Timeout(24 * time.Hour)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	wait := p.EachEvent(func(e *proto.PageScreencastFrame) bool {
		name := fmt.Sprintf("f%06d.jpg", len(frames))
		if err := os.WriteFile(filepath.Join(dir, name), e.Data, 0o644); err == nil {
			frames = append(frames, frame{name, time.Now()})
		}
		_ = proto.PageScreencastFrameAck{SessionID: e.SessionID}.Call(p)
		select {
		case <-stop:
			return true
		default:
			return false
		}
	})
	q := 85
	must(proto.PageStartScreencast{Format: proto.PageStartScreencastFormatJpeg, Quality: &q}.Call(p))
	fmt.Println("recording into", dir, "(SIGINT/SIGTERM to stop)")
	// A static page sends no frames, so poke a repaint to deliver the stop.
	go func() {
		<-stop
		stop <- syscall.SIGTERM
		for range 20 {
			_, _ = p.Eval(`() => { document.body.style.outline = document.body.style.outline ? '' : '0px solid transparent' }`)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	wait()
	_ = proto.PageStopScreencast{}.Call(p)
	if len(frames) == 0 {
		die("no frames captured")
	}
	var list strings.Builder
	for i, f := range frames {
		d := time.Second
		if i+1 < len(frames) {
			d = frames[i+1].at.Sub(f.at)
		}
		fmt.Fprintf(&list, "file '%s'\nduration %.3f\n", f.name, d.Seconds())
	}
	// The concat demuxer ignores the last duration unless the file repeats.
	fmt.Fprintf(&list, "file '%s'\n", frames[len(frames)-1].name)
	must(os.WriteFile(filepath.Join(dir, "frames.txt"), []byte(list.String()), 0o644))
	fmt.Printf("%d frames, %.1fs\n", len(frames), frames[len(frames)-1].at.Sub(frames[0].at).Seconds()+1)
	if len(args) < 2 {
		return
	}
	out, err := filepath.Abs(args[1])
	must(err)
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "concat", "-safe", "0", "-i", "frames.txt",
		"-vf", "fps=25,scale=trunc(iw/2)*2:trunc(ih/2)*2,format=yuv420p", "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", out)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	must(cmd.Run())
	fmt.Println("wrote", out)
}
