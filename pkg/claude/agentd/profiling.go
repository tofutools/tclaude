package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/perfstats"
	"github.com/tofutools/tclaude/pkg/common"
)

// Profiling is strictly opt-in (`serve --pprof` / `--pprof-addr`). Profiles
// expose stack traces, command lines and heap contents, so the default
// endpoint is a 0600 Unix socket in the private data directory — never the
// agent-reachable ~/.tclaude/api tree — and the TCP variant is loopback-only.

const pprofSocketName = "agentd-pprof.sock"

// statsPath serves perfstats.Take() as JSON next to the standard pprof routes.
const statsPath = "/debug/tclaude/stats"

// pprofSocketPath is where `serve --pprof` listens and `agentd profile`
// connects by default.
func pprofSocketPath() string {
	dir := common.TclaudeDataDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, pprofSocketName)
}

func profilingMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", httppprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", httppprof.Trace)
	mux.HandleFunc(statsPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(perfstats.Take())
	})
	return mux
}

// validatePprofAddr accepts only an explicit loopback host:port. An empty host
// (":6060") would bind every interface, so it is rejected rather than read as
// loopback the way the dashboard bind treats it.
func validatePprofAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--pprof-addr %q: %w", addr, err)
	}
	if strings.TrimSpace(host) == "" || !isLoopbackHost(host) {
		return fmt.Errorf("--pprof-addr %q: host must be an explicit loopback address, e.g. 127.0.0.1:6060", addr)
	}
	return nil
}

// loopbackHostOnly rejects requests whose Host header is not a loopback name.
// The TCP endpoint has no other authentication, so this is what stops a web
// page that DNS-rebinds its own hostname to 127.0.0.1 from reading profiles.
func loopbackHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host != "localhost" {
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// startProfilingServer brings up the profiling endpoints requested on the
// serve command line. It returns a stop func that is safe to call when
// nothing was started. Any failure is fatal to startup: the operator asked
// for profiling explicitly, and a daemon silently running without it would
// waste the measurement session.
func startProfilingServer(socket bool, addr string, out io.Writer) (func(), error) {
	if !socket && addr == "" {
		return func() {}, nil
	}
	var listeners []net.Listener
	var socketPath string
	fail := func(err error) (func(), error) {
		closeListeners(listeners)
		if socketPath != "" {
			removeSocketPaths([]string{socketPath})
		}
		return nil, err
	}
	if socket {
		socketPath = pprofSocketPath()
		if socketPath == "" {
			return fail(errors.New("could not determine pprof socket path"))
		}
		if err := prepareSocketPath(socketPath); err != nil {
			return fail(err)
		}
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			return fail(fmt.Errorf("listen %s: %w", socketPath, err))
		}
		listeners = append(listeners, ln)
		if err := os.Chmod(socketPath, 0o600); err != nil {
			return fail(fmt.Errorf("chmod pprof socket %s: %w", socketPath, err))
		}
	}
	if addr != "" {
		if err := validatePprofAddr(addr); err != nil {
			return fail(err)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fail(fmt.Errorf("listen %s: %w", addr, err))
		}
		listeners = append(listeners, ln)
	}

	// Block and mutex profiles are empty unless sampling is switched on; keep
	// it on for the daemon's lifetime while the endpoint is up.
	pinContentionProfiling()

	var servers []*http.Server
	for _, ln := range listeners {
		var handler http.Handler = profilingMux()
		if _, isTCP := ln.Addr().(*net.TCPAddr); isTCP {
			handler = loopbackHostOnly(handler)
		}
		srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
		servers = append(servers, srv)
		slog.Info("agentd profiling endpoint listening", "addr", ln.Addr().String())
		fmt.Fprintf(out, "  profiling (pprof): %s\n", ln.Addr().String())
		go func(ln net.Listener) {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Warn("agentd profiling endpoint stopped", "addr", ln.Addr().String(), "error", err)
			}
		}(ln)
	}
	return func() {
		for _, srv := range servers {
			_ = srv.Close()
		}
		if socketPath != "" {
			removeSocketPaths([]string{socketPath})
		}
	}, nil
}

type profileParams struct {
	Seconds      int    `long:"seconds" default:"30" help:"CPU profile duration. Run it while the CPU usage you want to explain is happening."`
	TraceSeconds int    `long:"trace-seconds" default:"5" help:"Execution trace duration, captured at the start of the CPU window (0 disables). Traces grow fast; a few seconds is usually enough to see wakeup patterns."`
	Label        string `long:"label" optional:"true" help:"Short note stored with the capture (shown in the dashboard list)."`
	Out          string `long:"out" short:"o" optional:"true" help:"Output directory. Default: a new capture in ~/.tclaude/data/profiles/, listed under Diagnostics › Performance in the dashboard."`
	Addr         string `long:"addr" optional:"true" help:"Profile a daemon started with --pprof-addr (host:port) instead of the default --pprof Unix socket."`
}

func profileCmd() *cobra.Command {
	return boa.CmdT[profileParams]{
		Use:   "profile",
		Short: "Capture CPU/heap/goroutine profiles and counters from a running agentd",
		Long: "Captures a CPU profile, an execution trace, heap/allocs/goroutine/block/mutex profiles and " +
			"before/after counter snapshots from a daemon started with `serve --pprof` (or --pprof-addr), " +
			"and writes them plus a summary.txt to one directory. Inspect with e.g. `go tool pprof -http=: cpu.pprof` " +
			"and `go tool trace trace.out`.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *profileParams, _ *cobra.Command, _ []string) {
			if err := runProfile(p, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

// profileClient returns an HTTP client and base URL for the profiling
// endpoint: TCP when addr is set, the private Unix socket otherwise.
func profileClient(addr string) (*http.Client, string, error) {
	if addr != "" {
		return &http.Client{}, "http://" + addr, nil
	}
	sock := pprofSocketPath()
	if sock == "" {
		return nil, "", errors.New("could not determine pprof socket path")
	}
	if _, err := os.Stat(sock); err != nil {
		return nil, "", fmt.Errorf("no profiling socket at %s — start agentd with `tclaude agentd serve --pprof`: %w", sock, err)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	return &http.Client{Transport: tr}, "http://agentd", nil
}

func runProfile(p *profileParams, out io.Writer) error {
	if p.Seconds <= 0 || p.Seconds > profileMaxSeconds {
		return fmt.Errorf("--seconds must be between 1 and %d", profileMaxSeconds)
	}
	traceSecs := min(max(p.TraceSeconds, 0), p.Seconds)
	client, base, err := profileClient(p.Addr)
	if err != nil {
		return err
	}
	now := time.Now()
	var dir string
	c := &profileCapture{
		Label: p.Label, Source: "cli", Status: profileStatusRunning,
		Seconds: p.Seconds, TraceSeconds: traceSecs, StartedAt: now,
	}
	if p.Out != "" {
		dir = p.Out
		c.ID = filepath.Base(dir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create output dir: %w", err)
		}
	} else {
		c.ID, dir, err = createProfileDir(now)
		if err != nil {
			return err
		}
	}
	if err := writeProfileMeta(dir, c); err != nil {
		return err
	}
	if err := captureProfileOverHTTP(client, base, dir, c, out); err != nil {
		failProfileCapture(dir, c, err)
		return err
	}
	summary, _ := os.ReadFile(filepath.Join(dir, profileSummaryFile))
	fmt.Fprint(out, string(summary))
	fmt.Fprintf(out, "\nWrote %s. Next: go tool pprof -http=: %s\n", dir, filepath.Join(dir, "cpu.pprof"))
	return nil
}

// captureProfileOverHTTP pulls every capture artifact from a daemon's
// profiling endpoint into dir and finishes the capture record.
func captureProfileOverHTTP(client *http.Client, base, dir string, c *profileCapture, out io.Writer) error {
	fetch := func(path, file string) error {
		resp, err := client.Get(base + path)
		if err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
		}
		f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			_ = f.Close()
			return fmt.Errorf("write %s: %w", file, err)
		}
		return f.Close()
	}
	readStats := func(file string) (perfstats.Snapshot, error) {
		var s perfstats.Snapshot
		if err := fetch(statsPath, file); err != nil {
			return s, err
		}
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return s, err
		}
		return s, json.Unmarshal(raw, &s)
	}

	before, err := readStats("stats-before.json")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Profiling agentd for %ds (trace %ds) into %s ...\n", c.Seconds, c.TraceSeconds, dir)
	var wg sync.WaitGroup
	var cpuErr, traceErr error
	deltaErrs := make([]error, len(profileFiles))
	wg.Go(func() { cpuErr = fetch(fmt.Sprintf("/debug/pprof/profile?seconds=%d", c.Seconds), "cpu.pprof") })
	// Block and mutex profiles are cumulative since daemon start; ?seconds=N
	// makes net/http/pprof serve only the window's delta.
	for i, f := range profileFiles {
		if f.Delta {
			wg.Go(func() { deltaErrs[i] = fetch(fmt.Sprintf("/debug/pprof/%s?seconds=%d", f.Lookup, c.Seconds), f.Name) })
		}
	}
	if c.TraceSeconds > 0 {
		wg.Go(func() { traceErr = fetch(fmt.Sprintf("/debug/pprof/trace?seconds=%d", c.TraceSeconds), "trace.out") })
	}
	wg.Wait()
	if cpuErr != nil {
		return cpuErr
	}
	if traceErr != nil {
		fmt.Fprintf(out, "  warning: trace capture failed: %v\n", traceErr)
		_ = os.Remove(filepath.Join(dir, "trace.out"))
	}
	for _, err := range deltaErrs {
		if err != nil {
			fmt.Fprintf(out, "  warning: %v\n", err)
		}
	}
	after, err := readStats("stats-after.json")
	if err != nil {
		return err
	}
	for _, f := range profileFiles {
		if f.Lookup == "" || f.Delta {
			continue
		}
		path := "/debug/pprof/" + f.Lookup
		if f.Debug != 0 {
			path += fmt.Sprintf("?debug=%d", f.Debug)
		}
		if err := fetch(path, f.Name); err != nil {
			fmt.Fprintf(out, "  warning: %v\n", err)
		}
	}
	return finishProfileCapture(dir, c, before, after, pprofTop(filepath.Join(dir, "cpu.pprof")))
}

// formatProfileSummary renders the counter delta over the capture window:
// the numbers that answer "what is top showing" before opening a profile.
func formatProfileSummary(before, after perfstats.Snapshot) string {
	var b strings.Builder
	window := after.TakenAt.Sub(before.TakenAt).Seconds()
	if window <= 0 {
		window = 1
	}
	pct := func(secs float64) float64 { return 100 * secs / window }
	fmt.Fprintf(&b, "agentd profile summary\n")
	fmt.Fprintf(&b, "  window:            %.1fs (daemon uptime %s)\n", window,
		(time.Duration(after.UptimeSeconds) * time.Second).String())
	if after.CPU.Available {
		selfUser := after.CPU.SelfUserSeconds - before.CPU.SelfUserSeconds
		selfSys := after.CPU.SelfSystemSeconds - before.CPU.SelfSystemSeconds
		childUser := after.CPU.ChildrenUserSeconds - before.CPU.ChildrenUserSeconds
		childSys := after.CPU.ChildrenSysSeconds - before.CPU.ChildrenSysSeconds
		fmt.Fprintf(&b, "  agentd CPU:        %.1f%% of one core (user %.2fs, sys %.2fs)\n",
			pct(selfUser+selfSys), selfUser, selfSys)
		fmt.Fprintf(&b, "  child process CPU: %.1f%% of one core (user %.2fs, sys %.2fs; reaped children only)\n",
			pct(childUser+childSys), childUser, childSys)
	}
	fmt.Fprintf(&b, "  goroutines:        %d -> %d\n", before.Goroutines, after.Goroutines)
	fmt.Fprintf(&b, "  GC cycles:         %d (%.1f/s), allocated %.1f MiB (%.2f MiB/s)\n",
		after.NumGC-before.NumGC, float64(after.NumGC-before.NumGC)/window,
		float64(after.TotalAllocBytes-before.TotalAllocBytes)/(1<<20),
		float64(after.TotalAllocBytes-before.TotalAllocBytes)/(1<<20)/window)
	fmt.Fprintf(&b, "  heap in use:       %.1f MiB\n", float64(after.HeapAllocBytes)/(1<<20))
	total := after.SubprocessTotal - before.SubprocessTotal
	fmt.Fprintf(&b, "  subprocesses:      %d started (%.2f/s, instrumented sites only)\n", total, float64(total)/window)
	for _, r := range perfstats.SubprocessDelta(before, after) {
		fmt.Fprintf(&b, "    %7d  %6.2f/s  %s\n", r.Count, float64(r.Count)/window, r.Command)
	}
	return b.String()
}

// pprofTop renders `go tool pprof -top` for the CPU profile when a Go
// toolchain is on PATH. Best-effort: the raw profile is the real artifact.
func pprofTop(profile string) string {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	outBytes, err := exec.CommandContext(ctx, goBin, "tool", "pprof", "-top", "-nodecount=40", profile).Output()
	if err != nil {
		return ""
	}
	return "CPU profile top 40 (go tool pprof -top):\n" + string(outBytes)
}
