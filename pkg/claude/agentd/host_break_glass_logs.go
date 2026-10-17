package agentd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

var breakGlassLogName = regexp.MustCompile(`^run-[0-9]+\.jsonl$`)

func handleBreakGlassRuns(w http.ResponseWriter, r *http.Request) {
	if peerActionFromRequest(r) != nil {
		writeError(w, 403, "local_only", "break-glass is local only")
		return
	}
	if !requireHuman(w, r, "read break-glass run logs") {
		return
	}
	dir := filepath.Join(config.DataDir(), "break-glass")
	name := r.PathValue("name")
	if name != "" {
		if !breakGlassLogName.MatchString(name) {
			writeError(w, 400, "invalid_arg", "invalid log name")
			return
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			writeError(w, 404, "not_found", "run log not found")
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		decoder := json.NewDecoder(f)
		var header struct {
			Started time.Time `json:"started"`
			Command []string  `json:"command"`
			Cwd     string    `json:"cwd"`
			Reason  string    `json:"reason"`
		}
		if err := decoder.Decode(&header); err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "Started: %s\nCommand: %q\nCwd: %s\nReason: %s\n\n", header.Started.Format(time.RFC3339), header.Command, header.Cwd, header.Reason)
		for {
			var frame breakGlassFrame
			if decoder.Decode(&frame) != nil {
				return
			}
			if len(frame.Data) > 0 {
				if _, err := w.Write(frame.Data); err != nil {
					return
				}
			}
			if frame.Error != "" {
				_, _ = fmt.Fprintf(w, "\nError: %s\n", frame.Error)
			}
			if frame.ExitCode != nil {
				_, _ = fmt.Fprintf(w, "\nExit code: %d\n", *frame.ExitCode)
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, 500, "io", err.Error())
		return
	}
	type run struct {
		Name     string    `json:"name"`
		Command  string    `json:"command"`
		Modified time.Time `json:"modified"`
	}
	runs := []run{}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !breakGlassLogName.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		runs = append(runs, run{Name: entry.Name(), Modified: info.ModTime()})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Modified.After(runs[j].Modified) })
	if len(runs) > 200 {
		runs = runs[:200]
	}
	for i := range runs {
		f, err := os.Open(filepath.Join(dir, runs[i].Name))
		if err != nil {
			continue
		}
		var header struct {
			Command []string `json:"command"`
		}
		err = json.NewDecoder(io.LimitReader(f, 65536)).Decode(&header)
		_ = f.Close()
		if err == nil {
			runs[i].Command = clipBreakGlassText(strings.Join(header.Command, " "), 512)
		}
	}
	writeJSON(w, 200, runs)
}
