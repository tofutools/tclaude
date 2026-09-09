package claude

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// StatusLineCommand is a provider-owned child mode of either shipped binary.
// It only reads native stdin/environment and writes the attempt's observation
// spool. It never opens application state or authenticates a daemon request.
const StatusLineCommand = "internal-claude-status-line"

type observedCompactionWindow struct {
	Known  bool  `json:"known"`
	Tokens int64 `json:"tokens"`
}

type nativeStatusLine struct {
	nativeContextUsage
	Model struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Workspace struct {
		CurrentDir string `json:"current_dir"`
	} `json:"workspace"`
	Cost struct {
		Total float64 `json:"total_cost_usd"`
	} `json:"cost"`
	Effort struct {
		Level string `json:"level"`
	} `json:"effort"`
	Limits map[string]struct {
		Used   float64 `json:"used_percentage"`
		Resets int64   `json:"resets_at"`
	} `json:"rate_limits"`
}

// RunStatusLine renders the native facts and publishes the same observation,
// including the window from this native process's environment. Inherit stays
// inherit: collecting that value does not set a launch environment override.
func RunStatusLine(input io.Reader, output io.Writer, spoolDirectory, nativeWindow string) error {
	raw, err := io.ReadAll(io.LimitReader(input, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > (1<<20)-1024 {
		return fmt.Errorf("claude status payload exceeds observation limit")
	}
	var fields map[string]json.RawMessage
	var status nativeStatusLine
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if fields == nil {
		return fmt.Errorf("claude status payload must be an object")
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return err
	}
	window, err := model.ParseAutoCompactWindow(nativeWindow)
	observed := observedCompactionWindow{Known: err == nil}
	if err == nil {
		observed.Tokens = model.AutoCompactWindowTokens(string(window))
	}
	encoded, err := json.Marshal(observed)
	if err != nil {
		return err
	}
	fields["tclaude_compaction_window"] = encoded
	payload, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	// A failed observation publication must not erase the operator's status.
	if _, err := io.WriteString(output, renderStatusLine(status, observed, time.Now())); err != nil {
		return err
	}
	return publishStatusObservation(spoolDirectory, payload)
}

func publishStatusObservation(directory string, payload []byte) error {
	if !filepath.IsAbs(directory) {
		return fmt.Errorf("claude status spool must be absolute")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("claude status spool must be a protected directory")
	}
	file, err := os.CreateTemp(directory, ".status-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(directory, "event-"+strings.TrimPrefix(filepath.Base(name), ".status-")))
}

func statusText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}
func finitePercent(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100
}
func statusBar(percent float64) string {
	fill := min(10, max(0, int(math.Round(percent/10))))
	return "[" + strings.Repeat("█", fill) + strings.Repeat("░", 10-fill) + "]"
}
func statusReset(at int64, now time.Time) string {
	if at <= 0 {
		return ""
	}
	remaining := time.Unix(at, 0).Sub(now)
	if remaining <= 0 {
		return "resetting"
	}
	minutes := int(math.Ceil(remaining.Minutes()))
	if minutes >= 24*60 {
		return fmt.Sprintf("%dd %dh", minutes/(24*60), minutes/60%24)
	}
	return fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
}
func renderStatusLine(status nativeStatusLine, window observedCompactionWindow, now time.Time) string {
	label := statusText(status.Model.DisplayName)
	if label == "" {
		label = statusText(status.Model.ID)
	}
	if label == "" {
		label = "Claude"
	}
	parts := []string{label}
	if native := status.Window; native != nil && native.Size > 0 && native.Percent != nil && finitePercent(*native.Percent) && window.Known {
		effective := model.EffectiveContextWindow(native.Size, window.Tokens)
		percent := model.RebaseContextPercentage(*native.Percent, native.Size, effective)
		parts[0] += fmt.Sprintf(" (%s) %s %.0f%%", model.FormatAutoCompactWindow(strconv.FormatInt(effective, 10)), statusBar(percent), percent)
	} else {
		parts[0] += " · context unknown"
	}
	hasLimits := false
	for _, pair := range [][2]string{{"five_hour", "5h"}, {"seven_day", "7d"}, {"seven_day_sonnet", "sonnet"}} {
		bucket, ok := status.Limits[pair[0]]
		if !ok || !finitePercent(bucket.Used) {
			continue
		}
		hasLimits = true
		parts = append(parts, strings.TrimSpace(fmt.Sprintf("%s %s %.0f%% %s", pair[1], statusBar(bucket.Used), bucket.Used, statusReset(bucket.Resets, now))))
	}
	if !hasLimits && status.Cost.Total > 0 && !math.IsInf(status.Cost.Total, 0) && !math.IsNaN(status.Cost.Total) {
		parts = append(parts, fmt.Sprintf("$%.2f", status.Cost.Total))
	}
	if effort := statusText(status.Effort.Level); effort != "" {
		parts = append(parts, "🧠 "+effort)
	}
	out := strings.Join(parts, " | ") + "\n"
	if cwd := statusText(status.Workspace.CurrentDir); cwd != "" {
		out += cwd + "\n"
	}
	return out
}

func claudeStatusLineCommand() string {
	executable, err := os.Executable()
	if err != nil {
		return "false"
	}
	// The path is supplied by the running binary, but still quote it as one shell
	// argument: installations may contain spaces or shell metacharacters.
	return "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "' " + StatusLineCommand
}
