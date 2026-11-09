package launcher

import (
	"bufio"
	"bytes"
	"context"
	"path"
	"strconv"
	"strings"
	"time"
)

const probeTimeout = 5 * time.Second

// detectSystemd recognises a process that is the main PID of a systemd
// service. The cgroup names the candidate unit; `systemctl show` confirms
// MainPID is us, because a shell inside a terminal service (and anything
// else that inherits INVOCATION_ID) is not the unit's main process.
func detectSystemd(ctx context.Context, env Env, run Runner, info *Info) bool {
	if env.Getenv("INVOCATION_ID") == "" {
		return false
	}
	raw, err := env.ReadFile("/proc/" + strconv.Itoa(env.PID) + "/cgroup")
	if err != nil {
		return false
	}
	unit, user := unitFromCgroup(string(raw))
	if unit == "" {
		return false
	}
	args := []string{"show", unit, "--no-pager",
		"-p", "Id", "-p", "MainPID", "-p", "ExecStart", "-p", "Environment",
		"-p", "EnvironmentFiles", "-p", "Restart", "-p", "KillMode", "-p", "FragmentPath"}
	if user {
		args = append([]string{"--user"}, args...)
	}
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := run(cctx, "systemctl", args...)
	if err != nil {
		return false
	}
	props := parseShow(out)
	if props["MainPID"] != strconv.Itoa(env.PID) {
		return false
	}
	info.Kind = KindSystemdSystem
	if user {
		info.Kind = KindSystemdUser
	}
	info.Unit = unit
	if id := firstWord(props["Id"]); unitNameRe.MatchString(id) {
		info.Unit = id
	}
	info.UnitPath = props["FragmentPath"]
	info.Argv = parseExecStartArgv(props["ExecStart"])
	info.RestartPolicy = props["Restart"]
	info.KillMode = props["KillMode"]
	for _, kv := range splitQuoted(props["Environment"]) {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			info.Env = append(info.Env, redactEnv(k, v))
		}
	}
	for _, f := range parseEnvironmentFiles(props["EnvironmentFiles"]) {
		info.EnvironmentFiles = append(info.EnvironmentFiles, f)
		if data, err := env.ReadFile(f); err == nil {
			info.Env = append(info.Env, parseEnvFile(data)...)
		}
	}
	if info.UnitPath != "" {
		if data, err := env.ReadFile(info.UnitPath); err == nil {
			info.ManagedByTclaude = bytes.Contains(data, []byte(ManagedMarker))
		}
	}
	return true
}

// unitFromCgroup returns the .service leaf of the unified (or name=systemd)
// cgroup path, and whether it lives under a user manager.
func unitFromCgroup(raw string) (unit string, user bool) {
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || (parts[0] != "0" && parts[1] != "name=systemd") {
			continue
		}
		p := parts[2]
		leaf := path.Base(p)
		if !strings.HasSuffix(leaf, ".service") || !unitNameRe.MatchString(leaf) {
			continue
		}
		user = false
		for _, seg := range strings.Split(p, "/") {
			if strings.HasPrefix(seg, "user@") && strings.HasSuffix(seg, ".service") && seg != leaf {
				user = true
			}
		}
		return leaf, user
	}
	return "", false
}

// parseShow parses `systemctl show` KEY=VALUE output. Repeated keys
// (EnvironmentFiles) are joined with newlines.
func parseShow(out []byte) map[string]string {
	props := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		if old, dup := props[k]; dup && old != "" {
			v = old + "\n" + v
		}
		props[k] = v
	}
	return props
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// parseExecStartArgv extracts argv[] from systemctl's ExecStart rendering:
// "{ path=/x ; argv[]=/x serve --flag ; ignore_errors=no ; ... }".
// systemd joins argv with single spaces, so an argument containing a space
// cannot be told apart; this is best-effort observation.
func parseExecStartArgv(s string) []string {
	_, rest, ok := strings.Cut(s, "argv[]=")
	if !ok {
		return nil
	}
	if i := strings.Index(rest, " ;"); i >= 0 {
		rest = rest[:i]
	}
	return strings.Fields(rest)
}

// parseEnvironmentFiles parses "/path (ignore_errors=no)" entries.
func parseEnvironmentFiles(s string) []string {
	var files []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if i := strings.LastIndex(line, " (ignore_errors="); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimPrefix(line, "-")
		if strings.HasPrefix(line, "/") {
			files = append(files, line)
		}
	}
	return files
}

// parseEnvFile reads KEY=VALUE lines of a systemd EnvironmentFile.
func parseEnvFile(data []byte) []EnvVar {
	var vars []EnvVar
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		vars = append(vars, redactEnv(k, v))
	}
	return vars
}

// splitQuoted splits systemd's Environment= rendering, honouring double
// quotes and backslash escapes.
func splitQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			have = true
		case c == '"':
			inQuote = !inQuote
			have = true
		case (c == ' ' || c == '\t') && !inQuote:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// detectLaunchd recognises a launchd job by XPC_SERVICE_NAME and confirms
// with `launchctl print` that the job's pid is us.
func detectLaunchd(ctx context.Context, env Env, run Runner, info *Info) bool {
	label := env.Getenv("XPC_SERVICE_NAME")
	if label == "" || label == "0" || strings.HasPrefix(label, "application.") || !labelRe.MatchString(label) {
		return false
	}
	uid := strconv.Itoa(env.UID)
	domains := []string{"gui/" + uid, "user/" + uid}
	if env.UID == 0 {
		domains = append([]string{"system"}, domains...)
	}
	for _, d := range domains {
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		out, err := run(cctx, "launchctl", "print", d+"/"+label)
		cancel()
		if err != nil {
			continue
		}
		p := parseLaunchctlPrint(out)
		if p.pid != strconv.Itoa(env.PID) {
			continue
		}
		info.Kind = KindLaunchd
		info.Label = label
		info.Domain = d
		info.UnitPath = p.path
		info.Argv = p.args
		if len(info.Argv) == 0 && p.program != "" {
			info.Argv = []string{p.program}
		}
		for _, kv := range p.env {
			info.Env = append(info.Env, redactEnv(kv.Key, kv.Value))
		}
		if info.UnitPath != "" {
			if data, err := env.ReadFile(info.UnitPath); err == nil {
				info.ManagedByTclaude = bytes.Contains(data, []byte(ManagedMarker))
			}
		}
		return true
	}
	return false
}

type launchctlJob struct {
	pid, path, program string
	args               []string
	env                []EnvVar
}

// parseLaunchctlPrint reads the job's top-level fields (one tab deep) from
// `launchctl print`. Its format is not a stable interface; anything not
// recognised is ignored.
func parseLaunchctlPrint(out []byte) launchctlJob {
	var job launchctlJob
	block := ""
	for _, raw := range strings.Split(string(out), "\n") {
		depth := len(raw) - len(strings.TrimLeft(raw, "\t"))
		line := strings.TrimSpace(raw)
		if block != "" {
			if depth <= 1 && line == "}" {
				block = ""
				continue
			}
			if depth != 2 {
				continue
			}
			switch block {
			case "arguments":
				job.args = append(job.args, line)
			case "environment":
				if k, v, ok := strings.Cut(line, " => "); ok {
					job.env = append(job.env, EnvVar{Key: k, Value: v})
				}
			}
			continue
		}
		if depth != 1 {
			continue
		}
		if name, ok := strings.CutSuffix(line, " = {"); ok {
			if name == "arguments" || name == "environment" {
				block = name
			} else {
				block = "skip"
			}
			continue
		}
		k, v, ok := strings.Cut(line, " = ")
		if !ok {
			continue
		}
		switch k {
		case "pid":
			job.pid = v
		case "path":
			job.path = v
		case "program":
			job.program = v
		}
	}
	return job
}
