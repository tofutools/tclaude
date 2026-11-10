package selfupdate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/launcher"
)

// CheckArgsFlag is the argv[1] of a tclaude binary's non-starting argument
// check: `<binary> --tclaude-check-args <command words and flags...>`
// resolves the subcommand and parses its flags, then exits 0 (accepted) or
// CheckArgsRejected without running anything.
const CheckArgsFlag = "--tclaude-check-args"

// CheckArgsRejected is the exit status of a rejected argument check.
const CheckArgsRejected = 2

// LaunchPlan is how the running daemon was launched, handed to an update
// job. Snapshot is durable (in the job journal, values hashed); Binary and
// Args stay in memory for the pre-flight check of the staged binary.
type LaunchPlan struct {
	Snapshot LaunchSnapshot
	// Binary is the name of the binary that runs the daemon (tclaude or
	// tclaude-agentd); empty skips the pre-flight check.
	Binary string
	// Args are the command lines the daemon may restart with, each without
	// argv[0] (`agentd serve --flag ...`).
	Args [][]string
}

// LaunchSnapshot is the running daemon's argv and environment before an
// update. Values are never kept in clear: Argv is the redacted display form,
// and every argv element and environment value is a salted SHA-256.
type LaunchSnapshot struct {
	TakenAt time.Time `json:"taken_at"`
	Salt    string    `json:"salt"`
	// Argv is the redacted command line, for display.
	Argv       []string          `json:"argv"`
	ArgvHashes []string          `json:"argv_hashes"`
	Env        map[string]string `json:"env"`
	Launcher   string            `json:"launcher,omitempty"`
	Unit       string            `json:"unit,omitempty"`
	// RestartMethod is how the restart was expected to happen.
	RestartMethod string `json:"restart_method,omitempty"`
}

// LaunchCheck is what an update job reports about the daemon's flags and
// environment: the pre-flight check of the new binary, and the comparison
// the restarted daemon made with the snapshot. It never carries values.
type LaunchCheck struct {
	Launcher      string   `json:"launcher,omitempty"`
	Unit          string   `json:"unit,omitempty"`
	RestartMethod string   `json:"restart_method,omitempty"`
	Argv          []string `json:"argv"`
	EnvKeys       int      `json:"env_keys"`
	// ArgsCheck is the pre-flight result: passed, passed_help (an older
	// binary without the check, its --help listed every flag), skipped,
	// forced or failed. ArgsCheckDetail says why.
	ArgsCheck       string `json:"args_check,omitempty"`
	ArgsCheckDetail string `json:"args_check_detail,omitempty"`
	// State is recorded (snapshot taken, restart pending), identical,
	// differs, or unavailable (the snapshot could not be read).
	State       string             `json:"state"`
	Differences []LaunchDifference `json:"differences,omitempty"`
	ComparedAt  *time.Time         `json:"compared_at,omitempty"`
	// Expected compares the restarted daemon with the launch it is
	// configured to run with (the launch profile, or a unit tclaude did not
	// write), when there is one. Keys only, never values.
	Expected *ExpectedLaunch `json:"expected,omitempty"`
}

// ExpectedLaunch is the restarted daemon against its configured launch.
// Baseline is profile or unit; State identical or differs.
type ExpectedLaunch struct {
	Baseline    string             `json:"baseline"`
	State       string             `json:"state"`
	Differences []LaunchDifference `json:"differences,omitempty"`
}

// LaunchDifference is one change between the snapshot and the restarted
// daemon. Kind is binary, command, flag or env; Change added, removed or
// changed. Before and After are display forms (redacted flag values; never
// an environment value).
type LaunchDifference struct {
	Kind   string `json:"kind"`
	Change string `json:"change"`
	Name   string `json:"name"`
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

func (d LaunchDifference) String() string {
	what := d.Kind + " " + d.Name
	if d.Kind == "binary" || d.Kind == "command" {
		what = d.Kind
	}
	switch {
	case d.Kind == "env":
		return fmt.Sprintf("environment %s %s", d.Name, d.Change)
	case d.Change == "changed":
		return fmt.Sprintf("%s changed: %s → %s", what, d.Before, d.After)
	case d.Change == "added":
		return fmt.Sprintf("%s added: %s", what, d.After)
	}
	return fmt.Sprintf("%s removed: %s", what, d.Before)
}

// volatileEnv are set afresh by the supervisor or shell for every start;
// they differ across any restart and say nothing about the configuration.
var volatileEnv = map[string]bool{
	"INVOCATION_ID": true, "JOURNAL_STREAM": true, "SYSTEMD_EXEC_PID": true, "MANAGERPID": true,
	"MAINPID": true, "LISTEN_PID": true, "LISTEN_FDS": true, "LISTEN_FDNAMES": true,
	"NOTIFY_SOCKET": true, "WATCHDOG_PID": true, "WATCHDOG_USEC": true, "SERVICE_RESULT": true,
	"EXIT_CODE": true, "EXIT_STATUS": true, "MONITOR_SERVICE_RESULT": true, "TRIGGER_UNIT": true,
	"_": true, "SHLVL": true, "OLDPWD": true,
	// launchd: a fresh UUID for every launch of the job.
	"LaunchInstanceID": true,
}

// ignoredEnv reports a key the comparison skips: volatile per-start values
// and the markers the updater itself sets for the commands it runs.
func ignoredEnv(key string) bool {
	return volatileEnv[key] || strings.HasPrefix(key, "TCLAUDE_UPDATE_")
}

func saltedHash(salt, value string) string {
	h := sha256.Sum256([]byte(salt + "\x00" + value))
	return hex.EncodeToString(h[:])
}

// CaptureLaunch snapshots argv and environ (KEY=VALUE entries) with a fresh
// salt.
func CaptureLaunch(argv, environ []string, kind, unit, restartMethod string) (LaunchSnapshot, error) {
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return LaunchSnapshot{}, err
	}
	return captureLaunch(hex.EncodeToString(salt[:]), argv, environ, kind, unit, restartMethod), nil
}

func captureLaunch(salt string, argv, environ []string, kind, unit, restartMethod string) LaunchSnapshot {
	snap := LaunchSnapshot{TakenAt: time.Now().UTC(), Salt: salt, Argv: launcher.RedactArgv(argv), Env: map[string]string{}, Launcher: kind, Unit: unit, RestartMethod: restartMethod}
	for _, a := range argv {
		snap.ArgvHashes = append(snap.ArgvHashes, saltedHash(salt, a))
	}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" && !ignoredEnv(k) {
			snap.Env[k] = saltedHash(salt, v)
		}
	}
	return snap
}

// Check is the job's report of the snapshot before the restart.
func (s LaunchSnapshot) Check() *LaunchCheck {
	return &LaunchCheck{Launcher: s.Launcher, Unit: s.Unit, RestartMethod: s.RestartMethod, Argv: append([]string{}, s.Argv...), EnvKeys: len(s.Env), State: "recorded"}
}

// argvShape splits a command line into its binary, its command words (the
// subcommand path before the first flag) and its flags. A flag's value is
// its =value or, failing that, the next token when that is not a flag.
type argvShape struct {
	binary, command string
	flags           map[string]argvFlag
}
type argvFlag struct{ display, hash string }

func shapeOf(display, hashes []string) argvShape {
	out := argvShape{flags: map[string]argvFlag{}}
	if len(display) == 0 {
		return out
	}
	out.binary = display[0]
	i := 1
	words := []string{}
	for ; i < len(display) && !strings.HasPrefix(display[i], "-"); i++ {
		words = append(words, display[i])
	}
	out.command = strings.Join(words, " ")
	for ; i < len(display); i++ {
		tok := display[i]
		if !strings.HasPrefix(tok, "-") {
			// A stray positional: keyed by itself.
			out.flags[tok] = add(out.flags[tok], tok, hashes[i])
			continue
		}
		name, value, hasValue := strings.Cut(tok, "=")
		hash := hashes[i]
		if !hasValue && i+1 < len(display) && !strings.HasPrefix(display[i+1], "-") {
			i++
			value, hasValue = display[i], true
			hash += hashes[i]
		}
		shown := name
		if hasValue {
			shown += "=" + value
		}
		out.flags[name] = add(out.flags[name], shown, hash)
	}
	return out
}

func add(f argvFlag, shown, hash string) argvFlag {
	if f.display != "" {
		shown = f.display + " " + shown
		hash = f.hash + "," + hash
	}
	return argvFlag{display: shown, hash: hash}
}

// CompareLaunch compares the restarted daemon's argv and environ with the
// snapshot. A binary path the update relocated is expected, not a
// difference.
func CompareLaunch(before LaunchSnapshot, argv, environ []string, kind, unit, restartMethod string, relocations []Relocation) LaunchCheck {
	after := captureLaunch(before.Salt, argv, environ, kind, unit, restartMethod)
	check := *before.Check()
	now := time.Now().UTC()
	check.ComparedAt = &now
	diffs := []LaunchDifference{}
	b, a := shapeOf(before.Argv, before.ArgvHashes), shapeOf(after.Argv, after.ArgvHashes)
	if len(before.ArgvHashes) > 0 && len(after.ArgvHashes) > 0 && before.ArgvHashes[0] != after.ArgvHashes[0] && !relocated(b.binary, a.binary, relocations) {
		diffs = append(diffs, LaunchDifference{Kind: "binary", Change: "changed", Name: "argv[0]", Before: b.binary, After: a.binary})
	}
	if b.command != a.command {
		diffs = append(diffs, LaunchDifference{Kind: "command", Change: "changed", Name: "command", Before: b.command, After: a.command})
	}
	for _, name := range unionKeys(b.flags, a.flags) {
		bf, inB := b.flags[name]
		af, inA := a.flags[name]
		switch {
		case !inA:
			diffs = append(diffs, LaunchDifference{Kind: "flag", Change: "removed", Name: name, Before: bf.display})
		case !inB:
			diffs = append(diffs, LaunchDifference{Kind: "flag", Change: "added", Name: name, After: af.display})
		case bf.hash != af.hash:
			diffs = append(diffs, LaunchDifference{Kind: "flag", Change: "changed", Name: name, Before: bf.display, After: af.display})
		}
	}
	for _, key := range unionKeys(before.Env, after.Env) {
		bv, inB := before.Env[key]
		av, inA := after.Env[key]
		switch {
		case !inA:
			diffs = append(diffs, LaunchDifference{Kind: "env", Change: "removed", Name: key})
		case !inB:
			diffs = append(diffs, LaunchDifference{Kind: "env", Change: "added", Name: key})
		case bv != av:
			diffs = append(diffs, LaunchDifference{Kind: "env", Change: "changed", Name: key})
		}
	}
	check.State = "identical"
	if len(diffs) > 0 {
		check.State, check.Differences = "differs", diffs
	}
	return check
}

func relocated(from, to string, relocations []Relocation) bool {
	for _, r := range relocations {
		if (from == r.From || filepath.Clean(from) == r.From) && (to == r.To || filepath.Clean(to) == r.To) {
			return true
		}
	}
	return false
}

func unionKeys[V any](a, b map[string]V) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, m := range []map[string]V{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Summary is one line for a notification or the CLI.
func (c *LaunchCheck) Summary() string {
	if c == nil {
		return ""
	}
	out := ""
	switch c.State {
	case "identical":
		out = "Kept flags and environment."
	case "differs":
		parts := make([]string, 0, len(c.Differences))
		for _, d := range c.Differences {
			parts = append(parts, d.String())
		}
		out = fmt.Sprintf("Flags and environment: %d difference(s) after the restart: %s.", len(c.Differences), strings.Join(parts, "; "))
	case "unavailable":
		out = "Flags and environment were not compared: the snapshot could not be read."
	}
	if e := c.Expected; e != nil && e.State == "differs" {
		if out != "" {
			out += " "
		}
		out += fmt.Sprintf("agentd does not run as its %s says (%d difference(s)).", baselineName(e.Baseline), len(e.Differences))
	}
	return out
}

func (s *Service) launchPath(id string) string {
	return filepath.Join(s.dir, "jobs", id+".launch.json")
}

// argsChecker runs the pre-flight check of a staged binary.
type argsChecker func(ctx context.Context, binary string, args []string) (string, error)

// preflight verifies the binary at path, which replaces the daemon's
// binary, accepts every command line the daemon may restart with. It
// returns the bare rejection (callers word what it means for them), nil when
// it passed or was forced.
func (s *Service) preflight(ctx context.Context, job *Job, name, path string, opts ApplyOptions) error {
	plan := opts.Launch
	if plan == nil || plan.Binary == "" || plan.Binary != name || job.Launch == nil {
		return nil
	}
	// Copy on write: the saved status shares the pointer, and Status()
	// reads it under s.mu.
	launch := *job.Launch
	defer func() { job.Launch = &launch }()
	if len(plan.Args) == 0 {
		launch.ArgsCheck, launch.ArgsCheckDetail = "skipped", "the daemon's command line is not `agentd serve`"
		return nil
	}
	result := "passed"
	for _, args := range plan.Args {
		mode, err := s.checkArgs(ctx, path, args)
		if err != nil {
			if opts.Force {
				launch.ArgsCheck, launch.ArgsCheckDetail = "forced", err.Error()
				job.Warnings = append(append([]string{}, job.Warnings...), "Forced past the flag check: "+err.Error())
				return nil
			}
			launch.ArgsCheck, launch.ArgsCheckDetail = "failed", err.Error()
			return fmt.Errorf("the new %s would not start with the daemon's current flags: %w", name, err)
		}
		if mode == "passed_help" {
			result = mode
		}
	}
	launch.ArgsCheck = result
	if result == "passed_help" {
		launch.ArgsCheckDetail = "the new binary has no argument check; every flag is listed in its --help"
	}
	return nil
}

// endLaunchCheck closes the launch report of a job that ends without a
// restart: no comparison will follow, so the hashes are dropped now.
func (s *Service) endLaunchCheck(job *Job) {
	_ = os.Remove(s.launchPath(job.ID))
	if job.Launch != nil && job.Launch.State == "recorded" {
		l := *job.Launch
		l.State = "not_restarted"
		job.Launch = &l
	}
}

// checkLaunchArgs asks binary whether it accepts args without starting
// anything. A binary without the check (older than it) answers with an
// unknown-flag error; then every flag must appear in its --help.
func checkLaunchArgs(ctx context.Context, binary string, args []string) (string, error) {
	out, err := runProbe(ctx, binary, append([]string{CheckArgsFlag}, args...))
	if err == nil {
		return "passed", nil
	}
	if !bytes.Contains(out, []byte("unknown flag: "+CheckArgsFlag)) {
		return "", errors.New(rejection(out, err))
	}
	words := []string{}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
	}
	help, herr := runProbe(ctx, binary, append(words, "--help"))
	if herr != nil {
		return "", fmt.Errorf("could not read its --help: %s", rejection(help, herr))
	}
	known := map[string]bool{}
	for _, m := range helpFlagRe.FindAllSubmatch(help, -1) {
		known[string(m[1])] = true
	}
	for _, a := range args[len(words):] {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, _ := strings.Cut(a, "=")
		if !known[name] {
			return "", fmt.Errorf("unknown flag: %s", name)
		}
	}
	return "passed_help", nil
}

var helpFlagRe = regexp.MustCompile(`(?m)(?:^|[\s,])(--?[A-Za-z0-9][A-Za-z0-9-]*)`)

// rejection is the first error line of a probe's output.
func rejection(out []byte, err error) string {
	for _, line := range strings.Split(string(out), "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "Error:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "Error:"))
		}
	}
	if t := strings.TrimSpace(tail(out, 300)); t != "" {
		return t
	}
	return err.Error()
}

func runProbe(ctx context.Context, binary string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 100 * time.Millisecond
	var out limitedBuffer
	out.limit = 64 << 10
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.data, err
}

// CompareLaunch records, on the job the restart finished, how the restarted
// daemon's argv and environ compare with the snapshot taken before it.
// Differences become job warnings. It returns the updated job, or nil when
// there is nothing to compare (no snapshot, already compared, or the job
// did not restart this daemon).
func (s *Service) CompareLaunch(argv, environ []string, kind, unit, restartMethod string) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Job == nil || s.status.Job.Launch == nil || s.status.Job.Launch.State != "recorded" || s.status.Job.Phase != "restarted" {
		return nil, nil
	}
	job := *s.status.Job
	var snap LaunchSnapshot
	var check LaunchCheck
	if err := readJSON(s.launchPath(job.ID), &snap); err != nil {
		check = *job.Launch
		check.State = "unavailable"
	} else {
		check = CompareLaunch(snap, argv, environ, kind, unit, restartMethod, job.Relocations)
		check.ArgsCheck, check.ArgsCheckDetail = job.Launch.ArgsCheck, job.Launch.ArgsCheckDetail
	}
	job.Launch = &check
	if check.State == "differs" {
		job.Warnings = append(job.Warnings, check.Summary())
	}
	if err := s.saveJob(job); err != nil {
		return nil, err
	}
	// The hashes are no longer needed once compared.
	_ = os.Remove(s.launchPath(job.ID))
	return &job, nil
}

// RecordExpectedLaunch adds the comparison with the configured launch to
// the latest job's launch report, once it has been compared with its
// snapshot. A difference becomes a job warning.
func (s *Service) RecordExpectedLaunch(jobID string, exp ExpectedLaunch) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Job == nil || s.status.Job.ID != jobID || s.status.Job.Launch == nil || s.status.Job.Launch.ComparedAt == nil || s.status.Job.Launch.Expected != nil {
		return nil, nil
	}
	job := *s.status.Job
	l := *job.Launch
	for i := range exp.Differences {
		if exp.Differences[i].Kind == "env" {
			exp.Differences[i].Before, exp.Differences[i].After = "", ""
		}
	}
	l.Expected = &exp
	job.Launch = &l
	if exp.State == "differs" {
		parts := make([]string, 0, len(exp.Differences))
		for _, d := range exp.Differences {
			parts = append(parts, d.String())
		}
		job.Warnings = append(append([]string{}, job.Warnings...), fmt.Sprintf("agentd does not run as its %s says: %s.", baselineName(exp.Baseline), strings.Join(parts, "; ")))
	}
	if err := s.saveJob(job); err != nil {
		return nil, err
	}
	return &job, nil
}

func baselineName(b string) string {
	if b == "unit" {
		return "service unit"
	}
	return "launch profile"
}
