package testharness

import (
	"fmt"
	"regexp"
	"strings"
)

// The Copilot CLI launch grammar, modelled as a parser.
//
// Every other pane simulator in this package is handed structured spawn
// arguments and never sees a command line. That is fine for Claude Code and
// Codex, whose spawners tclaude has been shipping for a while, and it is not
// fine for Copilot: the whole reason a detached Copilot agent is hard is that
// its launch string decides whether the pane runs or parks forever, and the
// exact spelling of several flags is load-bearing (`--resume=` binds only with
// an `=`; `--session-id` takes a space; `-i` must come last or another option
// swallows the prompt). A simulator that accepted structured arguments would
// be blind to every one of those.
//
// So the Copilot branch of the simulated spawner renders the REAL
// harness.copilotSpawner.BuildCommand output and feeds it through here. A
// spawner regression then fails inside a daemon flow test, at the boundary
// where a real launch would have failed, instead of silently producing a pane
// that behaves nothing like the one production would have got.
//
// # What this parser is, and what it deliberately is not
//
// It is a pin on WHAT TCLAUDE MAY EMIT, not a reimplementation of Copilot's
// argument parser. Where 1.0.77's real behaviour for a spelling is unmeasured,
// this rejects the spelling with an error saying so, rather than guessing an
// acceptance that would then be baked into every test that passes through it.
// Rejecting an unmeasured form costs one clear failure the day tclaude starts
// emitting it; accepting one costs a test suite that agrees with production
// about a behaviour neither of them has ever observed.
//
// Grammar sources, in the order they are trusted:
//
//  1. The measured 1.0.77 permission contract committed under
//     pkg/claude/harness/copilotfixture/testdata (PR #1936). Everything about
//     rule-pattern acceptance below comes from there, including the finding
//     that `--deny-tool 'url()'` is a HARD PARSE ERROR — the flag TCL-973's
//     own plan proposed as the daemon default, which would have killed every
//     Copilot pane at launch. That is exactly the class of mistake this parser
//     exists to catch, so it is modelled as a launch failure and not softened.
//  2. Copilot CLI's documented command-line options table, for the `=`-vs-space
//     spellings copilot_spawner.go already cites flag by flag.

// copilotUUIDRE is the shape `--session-id` must carry.
//
// Copilot creates a session for an UNMATCHED `--session-id` only when the
// value is a valid UUID; a name or an id prefix never creates one, and
// `--resume` additionally accepts both of those. So a launch that presets a
// non-UUID id does not fail loudly — it quietly attaches to, or fails to
// create, something other than the conversation the daemon just enrolled. That
// is a silent identity bug, which is why it is a parse error here.
var copilotUUIDRE = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// CopilotAllowAllEnvVar is the ambient promotion measured in the 1.0.77
// contract: strictly stronger than the `--allow-all-tools` flag it documents,
// since it also clears the folder-trust gate no flag can clear.
const CopilotAllowAllEnvVar = "COPILOT_ALLOW_ALL"

// copilotAllowAllEnvValue is the ONLY value that promotes. The contract
// measured parsing as strict, case-sensitive equality against this literal:
// "TRUE", "1", "false" and "" all behave exactly as unset.
const copilotAllowAllEnvValue = "true"

// CopilotLaunch is one parsed `copilot` invocation: the environment the pane
// exports, the identity flags, and the permission posture the argv resolves
// to. It is the input the simulator's gate model runs on.
type CopilotLaunch struct {
	// Binary is argv[0] as written (a bare `copilot` or a quoted path).
	Binary string

	// Env is the `export K=V; ` prefix the spawner emits, decoded. Only the
	// variables the simulator reasons about need to be meaningful, but every
	// export is captured so a test can assert a scrub.
	Env map[string]string

	// Identity.
	SessionID string // --session-id <uuid>, fresh launches
	ResumeID  string // --resume=<id>, relaunches
	Name      string // --name=<name>, fresh launches only
	Model     string // --model=<model>
	Effort    string // --effort=<level>

	// InitialPrompt is the `-i <prompt>` first turn.
	InitialPrompt string

	// Permission posture. Only the flags the 1.0.77 contract measured the
	// EFFECT of are represented; the rest are rejected at parse (see the
	// unmodelled arms in parseArgs), because a permission flag that parses and
	// is then ignored is precisely the regression this seam exists to catch.
	AllowAllTools   bool
	AllowAllPaths   bool
	AllowAllURLs    bool
	NoAskUser       bool
	DisallowTempDir bool
	AddDirs         []string
	DenyTools       []string

	// BlanketAllow records `--allow-all` / `--yolo`. It is kept SEPARATE from
	// AllowAllTools on purpose: the contract measured these two flags only
	// against the folder-trust gate (where they do nothing), and no committed
	// scenario measured their effect on tool approval, paths or URLs. Folding
	// them into AllowAllTools would have the simulator assert an
	// auto-approval nobody observed — so instead the gate model fails the test
	// when a decision would depend on them. See CopilotSim.blockReasonLocked.
	BlanketAllow bool

	// Argv is everything after the binary, in order, so a test can assert on
	// the raw spelling (flag order, the `=` forms) without re-tokenizing.
	Argv []string
}

// AmbientAllowAll reports whether the exported environment carries the
// measured promotion. See copilotAllowAllEnvValue for why this is an exact
// string comparison and not a truthiness test.
func (l CopilotLaunch) AmbientAllowAll() bool {
	return l.Env[CopilotAllowAllEnvVar] == copilotAllowAllEnvValue
}

// ToolsAutoApproved reports whether TOOL CALLS run without a confirmation
// prompt.
//
// Exactly two inputs, and both were measured against a tool call rather than
// inferred from a flag's name: `--allow-all-tools` completed an unsafe command
// that blocked without it (contract entry `default-interactive-blocking`), and
// COPILOT_ALLOW_ALL=true executed an unsafe tool call with no flags at all
// (entry `ambient-allow-all-env`).
//
// `--allow-all` / `--yolo` are deliberately absent — see BlanketAllow. This
// method answers only the tool axis; the path and URL axes are separate gates
// with separate evidence, and reusing this for them was a real bug the cold
// review of the first revision caught.
func (l CopilotLaunch) ToolsAutoApproved() bool {
	return l.AllowAllTools || l.AmbientAllowAll()
}

// ParseCopilotLaunch decodes a spawner-produced command string.
//
// An error models a launch the real CLI would reject: the process exits
// non-zero and the pane dies, which is the outcome the simulator reproduces
// rather than papering over.
func ParseCopilotLaunch(cmd string) (CopilotLaunch, error) {
	statements, err := copilotSplitStatements(cmd)
	if err != nil {
		return CopilotLaunch{}, err
	}
	launch := CopilotLaunch{Env: map[string]string{}}
	var argv []string
	for i, stmt := range statements {
		if len(stmt) == 0 {
			continue
		}
		if stmt[0] == "export" {
			if len(stmt) != 2 {
				return CopilotLaunch{}, fmt.Errorf(
					"copilot launch: malformed env export %q", strings.Join(stmt, " "))
			}
			key, value, found := strings.Cut(stmt[1], "=")
			if !found {
				return CopilotLaunch{}, fmt.Errorf(
					"copilot launch: malformed env export %q", stmt[1])
			}
			launch.Env[key] = value
			continue
		}
		if stmt[0] == "unset" {
			// A launch may SCRUB an inherited variable, and the model has to
			// follow that or it would report an ambient promotion the pane
			// never sees. The spawner unsets COPILOT_ALLOW_ALL for exactly this
			// reason: the operator's own environment must not be able to
			// silently widen a recorded posture.
			//
			// Applied in statement order, so a later export legitimately wins.
			// The names are dropped rather than recorded as empty — an empty
			// COPILOT_ALLOW_ALL and an absent one are the same thing to the
			// CLI, and AmbientAllowAll compares against the exact measured
			// value either way.
			if len(stmt) < 2 {
				return CopilotLaunch{}, fmt.Errorf(
					"copilot launch: malformed unset %q", strings.Join(stmt, " "))
			}
			for _, name := range stmt[1:] {
				delete(launch.Env, name)
			}
			continue
		}
		if i != len(statements)-1 {
			return CopilotLaunch{}, fmt.Errorf(
				"copilot launch: unexpected statement before the harness command: %q",
				strings.Join(stmt, " "))
		}
		argv = stmt
	}
	if len(argv) == 0 {
		return CopilotLaunch{}, fmt.Errorf("copilot launch: no command in %q", cmd)
	}
	launch.Binary = argv[0]
	launch.Argv = argv[1:]
	if err := launch.parseArgs(); err != nil {
		return CopilotLaunch{}, err
	}
	return launch, nil
}

// parseArgs walks the argv, enforcing the spellings and the exclusions.
//
//nolint:gocyclo // One flag per arm; splitting it would only hide the table.
func (l *CopilotLaunch) parseArgs() error {
	args := l.Argv
	for i := 0; i < len(args); i++ {
		arg := args[i]
		// `-i PROMPT` starts an interactive session and submits the prompt.
		// Copilot's own option table puts no restriction on its position, but
		// tclaude's spawner emits it LAST on purpose — an option that takes a
		// value would otherwise swallow the prompt — and that ordering is the
		// thing worth pinning, so anything after it is an error here.
		if arg == "-i" || arg == "--interactive" {
			value, err := copilotFlagValue(args, &i, arg)
			if err != nil {
				return err
			}
			if i != len(args)-1 {
				return fmt.Errorf(
					"copilot launch: %s must be last; %q follows its prompt and would be "+
						"parsed as a stray argument", arg, args[i+1])
			}
			l.InitialPrompt = value
			continue
		}
		switch {
		case arg == "-p" || arg == "--prompt":
			// The headless form: it runs the prompt and EXITS. A pane launched
			// with it dies the moment the turn completes, taking the agent with
			// it, so it must never reach a tmux pane.
			return fmt.Errorf(
				"copilot launch: %s is the headless form and exits after the turn; "+
					"an interactive pane must use -i", arg)

		case arg == "--session-id":
			value, err := copilotFlagValue(args, &i, arg)
			if err != nil {
				return err
			}
			if !copilotUUIDRE.MatchString(value) {
				return fmt.Errorf(
					"copilot launch: --session-id %q is not a UUID; Copilot creates a "+
						"session for an unmatched id only when it is one, so this would "+
						"attach the launch to something other than the enrolled conversation",
					value)
			}
			l.SessionID = value

		case strings.HasPrefix(arg, "--session-id="):
			return fmt.Errorf(
				"copilot launch: --session-id is documented with a SPACE (%q used the "+
					"= form); whether 1.0.77 accepts the = form is unmeasured, so the "+
					"simulator refuses to vouch for it", arg)

		case arg == "--resume" || arg == "-r":
			// The value is OPTIONAL, so a bare `--resume` does not consume the
			// next word: it opens the interactive session picker, and where the
			// picker cannot be drawn it exits. Either way a detached pane is
			// lost, and the id tclaude meant to resume becomes a positional.
			return fmt.Errorf(
				"copilot launch: bare %s opens the session picker (its value is "+
					"optional, so only the =<id> form binds an id)", arg)

		case strings.HasPrefix(arg, "--resume=") || strings.HasPrefix(arg, "-r="):
			_, value, _ := strings.Cut(arg, "=")
			if value == "" {
				return fmt.Errorf("copilot launch: %q binds an empty resume id", arg)
			}
			l.ResumeID = value

		case arg == "--continue" || arg == "-c" || arg == "--connect":
			// Recognized only so the refusal can name the flag: these select a
			// conversation implicitly, they are documented as incompatible with
			// --resume, and tclaude never has a reason to emit one.
			return fmt.Errorf(
				"copilot launch: %s selects a conversation implicitly; tclaude always "+
					"knows the id it wants", arg)

		case strings.HasPrefix(arg, "--name="):
			_, l.Name, _ = strings.Cut(arg, "=")
		case strings.HasPrefix(arg, "--model="):
			_, l.Model, _ = strings.Cut(arg, "=")
		case strings.HasPrefix(arg, "--effort="):
			_, l.Effort, _ = strings.Cut(arg, "=")

		case arg == "--name" || arg == "--model" || arg == "--effort":
			return fmt.Errorf(
				"copilot launch: %s is documented with an = (a space-separated value "+
					"is unmeasured)", arg)

		// Permission axis. Every flag here is one the 1.0.77 contract measured
		// or, for the blanket allows, one it measured the effect of.
		case arg == "--allow-all-tools":
			l.AllowAllTools = true
		case arg == "--allow-all-paths":
			l.AllowAllPaths = true
		case arg == "--allow-all" || arg == "--yolo":
			l.BlanketAllow = true
		case arg == "--no-ask-user":
			l.NoAskUser = true
		case arg == "--disallow-temp-dir":
			l.DisallowTempDir = true

		case arg == "--add-dir":
			value, err := copilotFlagValue(args, &i, arg)
			if err != nil {
				return err
			}
			l.AddDirs = append(l.AddDirs, value)

		case arg == "--deny-tool":
			value, err := copilotFlagValue(args, &i, arg)
			if err != nil {
				return err
			}
			if err := l.addDenyTool(arg, value); err != nil {
				return err
			}

		case strings.HasPrefix(arg, "--deny-tool="):
			flag, value, _ := strings.Cut(arg, "=")
			if err := l.addDenyTool(flag, value); err != nil {
				return err
			}

		// The permission flags whose EFFECT no committed scenario measured.
		// They are named individually rather than left to the catch-all so the
		// refusal can say what is missing, because each needs a different
		// fixture before it can be modelled — and because parsing one and then
		// ignoring it is worse than refusing it. `--deny-url` is the sharpest:
		// the contract's corroborating notes report domain-scoped denies as
		// ENFORCED with no prompt, so a launch carrying one and a simulator
		// that ignored it would model as "allowed" exactly where reality
		// denies.
		case arg == "--allow-tool", strings.HasPrefix(arg, "--allow-tool="):
			return copilotUnmodelledFlag("--allow-tool",
				"no scenario measured whether an allow rule closes the approval prompt")
		case arg == "--allow-all-urls":
			// Measured to close the web-fetch URL prompt on its own, which is
			// what proves that prompt is a URL decision rather than ordinary
			// tool approval (entry web-fetch-url-access, result 2).
			l.AllowAllURLs = true
		case arg == "--allow-url":
			return copilotUnmodelledFlag(arg,
				"no scenario measured a URL ALLOW rule; only the deny side was")
		case arg == "--deny-url":
			value, valErr := copilotFlagValue(args, &i, arg)
			if valErr != nil {
				return valErr
			}
			return copilotUnimplementedFlag(arg+" "+value,
				"host-scoped --deny-url is MEASURED as enforced and the wildcard forms "+
					"as inert (entry web-fetch-url-access), but this simulator implements "+
					"URL denies only through --deny-tool's bare kind")
		case arg == "--excluded-tools":
			value, valErr := copilotFlagValue(args, &i, arg)
			if valErr != nil {
				return valErr
			}
			return copilotUnimplementedFlag(arg+" "+value,
				"tool REMOVAL is measured — `--excluded-tools web_fetch` drops the tool "+
					"from the catalog and a call to it answers \"Tool 'web_fetch' does not "+
					"exist\" without deadlocking — but this simulator models a fixed tool "+
					"catalog and cannot express a removed tool")
		case arg == "--mode", arg == "--plan", arg == "--autopilot":
			return copilotUnmodelledFlag(arg,
				"the agent-mode axis is a separate autonomy contract with its own "+
					"forced-continuation semantics and no committed measurement")

		default:
			return fmt.Errorf(
				"copilot launch: unmodelled argument %q. The simulator refuses arguments "+
					"whose 1.0.77 behaviour it cannot vouch for rather than ignoring them, "+
					"since an ignored permission flag is exactly the regression this seam "+
					"exists to catch", arg)
		}
	}
	// The documented exclusions. `--resume` may not be combined with
	// `--session-id`, `--continue`, `--connect` or the worktree flags; the
	// latter three already returned above, so only this pair can reach here.
	if l.ResumeID != "" && l.SessionID != "" {
		return fmt.Errorf(
			"copilot launch: --resume cannot be combined with --session-id")
	}
	// `--name` is documented as naming a NEW session. Its behaviour on a
	// resume is unverified, which is why copilot_spawner.go renames a resumed
	// conversation in-pane instead.
	if l.ResumeID != "" && l.Name != "" {
		return fmt.Errorf(
			"copilot launch: --name on a --resume is unverified; a resumed " +
				"conversation is renamed in-pane instead")
	}
	return nil
}

// addDenyTool validates a deny rule and keeps it, or refuses a rule whose
// ENFORCEMENT this simulator cannot reproduce.
//
// The split matters because parse acceptance and runtime enforcement are two
// different questions, and the contract records them coming apart. A rule that
// parses is not a rule that does anything, and — the direction that actually
// hurts — a rule that parses and IS enforced would otherwise be carried here
// and then silently ignored by the gate model, which reads as "allowed" where
// reality denies.
//
// Only tool-kind rules the gate model can evaluate are kept. `url(...)` and
// `write(...)` are refused on the same reasoning that already refuses
// `--deny-url`: the URL layer's enforcement is reported by an independent rig
// but no committed scenario establishes it, and this simulator models no write
// tool at all, so a write rule could never match anything it produces.
func (l *CopilotLaunch) addDenyTool(flag, value string) error {
	if err := copilotCheckRulePattern(flag, value); err != nil {
		return err
	}
	kind, pattern, scoped := strings.Cut(strings.TrimSuffix(value, ")"), "(")
	switch {
	case kind == "url" && !scoped:
		// The BARE KIND is a working blanket URL deny, measured per spelling by
		// entry `web-fetch-url-access` result 4: it denies every URL at the
		// permission layer, with no prompt and before name resolution, while
		// paired with --allow-all-tools — so a launch-time deny beats a
		// launch-time blanket allow on the URL axis. Modelled, because it is
		// the one URL rule tclaude could actually render as a default.
	case kind == "url" && copilotWildcardURLPattern(pattern):
		// Measured INERT, by the same entry: `url(*)` parses and then matches
		// nothing, falling through to the network layer. Kept and modelled as
		// matching nothing, which is the faithful behaviour and the useful one
		// — a tclaude default that believed this was a deny should show the URL
		// going through, not a simulator that quietly makes it work.
	case kind == "url":
		// Host-scoped rules ARE enforced (measured), so ignoring one would
		// model a real deny as an allow. This simulator does not implement
		// host matching, so it refuses rather than pretending either way.
		return copilotUnimplementedFlag(flag+" "+value,
			"host-scoped URL denies are MEASURED as enforced at the permission layer "+
				"(entry web-fetch-url-access), but this simulator implements only the "+
				"bare-kind blanket deny — it does not match hosts")
	case kind == "write":
		return copilotUnimplementedFlag(flag+" "+value,
			"this simulator models no write tool, so a write rule could never match a "+
				"call it produces")
	}
	l.DenyTools = append(l.DenyTools, value)
	return nil
}

// copilotWildcardURLPattern reports whether a URL rule pattern is one of the
// spellings MEASURED to match nothing at runtime.
//
// Exactly the measured set, no near-misses. An earlier revision also listed
// `http://*` by analogy with `https://*`, which no scenario covers — and that
// inference lands on the permissive side, since modelling a rule as inert when
// it might be enforced turns a real deny into an allow. A spelling nobody
// measured therefore falls through to the host-scoped arm and is refused,
// which is the same doctrine every other unmeasured case here follows.
func copilotWildcardURLPattern(pattern string) bool {
	switch pattern {
	case "*", "https://*", "*.*":
		return true
	}
	return false
}

// copilotUnimplementedFlag is the refusal for behaviour the fixtures DO
// establish but this simulator does not reproduce.
//
// It is a different sentence from copilotUnmodelledFlag on purpose, and the
// distinction is the whole reason both exist: "nobody has measured this" tells
// the reader to go and measure, while "this is measured and we chose not to
// implement it" tells them the evidence is already committed and the work is
// here. Collapsing the two would leave a stale to-do pointing at a fixture that
// already exists.
func copilotUnimplementedFlag(flag, why string) error {
	return fmt.Errorf(
		"copilot launch: %s is MEASURED BUT NOT IMPLEMENTED by this simulator — %s. "+
			"Refused rather than accepted-and-ignored, since ignoring a rule the real "+
			"CLI enforces models a deny as an allow", flag, why)
}

// copilotUnmodelledFlag is the refusal for a flag the CLI accepts but this
// simulator has no measurement for. It names the flag and what is missing, so
// the reader's next step is "commit a fixture", not "guess".
func copilotUnmodelledFlag(flag, why string) error {
	return fmt.Errorf(
		"copilot launch: %s is UNMODELLED — %s. The simulator refuses it rather "+
			"than parsing it and silently ignoring it, because an ignored "+
			"permission flag is the regression this seam exists to catch", flag, why)
}

// copilotRulePatternRE is the accepted spelling of a tool rule: a bare kind,
// or a kind with a NON-EMPTY parenthesised pattern.
//
// Measured on 1.0.77: `url`, `url(*)`, `url(example.com)`, `shell(*)` and
// `write(/tmp)` all parse, while `url()`, `shell()`, `write()` and a bare `*`
// are rejected at argument parse with exit 1 and no provider contact.
var copilotRulePatternRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*(\([^)]+\))?$`)

// copilotCheckRulePattern reproduces the measured parse acceptance.
//
// Acceptance only. Whether a pattern is ENFORCED at runtime is a different
// question, and for URL rules the contract records that the two demonstrably
// come apart — `url(*)` parses and then matches nothing. The simulator's gate
// model deliberately does not read those spellings as working denies; see
// copilot_sim.go.
func copilotCheckRulePattern(flag, value string) error {
	if copilotRulePatternRE.MatchString(value) {
		return nil
	}
	return fmt.Errorf(
		"copilot launch: Invalid %s value. Error: Invalid rule format: %s "+
			"(1.0.77 rejects empty parentheses and a bare *, exits 1, and never "+
			"contacts the provider)", flag, value)
}

// copilotFlagValue consumes a space-separated flag value, advancing i.
func copilotFlagValue(args []string, i *int, flag string) (string, error) {
	if *i+1 >= len(args) {
		return "", fmt.Errorf("copilot launch: %s expects a value", flag)
	}
	*i++
	return args[*i], nil
}

// copilotSplitStatements tokenizes a `sh -c` string into `;`-separated
// statements of words, honouring the single-quoting clcommon.ShellQuoteArg
// produces (including its `'\''` escape) and backslash escapes outside quotes.
//
// It is a deliberately small lexer rather than a shell: the spawner emits
// exactly `export K=V; … ; copilot …`, so anything with pipes, substitutions
// or redirections is a change worth failing on rather than interpreting.
func copilotSplitStatements(cmd string) ([][]string, error) {
	var (
		statements [][]string
		words      []string
		word       strings.Builder
		haveWord   bool
		inQuote    bool
	)
	flushWord := func() {
		if haveWord {
			words = append(words, word.String())
			word.Reset()
			haveWord = false
		}
	}
	flushStatement := func() {
		flushWord()
		if len(words) > 0 {
			statements = append(statements, words)
			words = nil
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if inQuote {
			if c == '\'' {
				inQuote = false
				continue
			}
			word.WriteByte(c)
			haveWord = true
			continue
		}
		switch c {
		case '\'':
			inQuote = true
			haveWord = true
		case '\\':
			if i+1 >= len(cmd) {
				return nil, fmt.Errorf("copilot launch: trailing backslash in %q", cmd)
			}
			i++
			word.WriteByte(cmd[i])
			haveWord = true
		case ' ', '\t':
			flushWord()
		case ';':
			flushStatement()
		case '|', '&', '<', '>', '(', ')', '`', '$':
			return nil, fmt.Errorf(
				"copilot launch: unquoted shell metacharacter %q in %q; the simulator "+
					"models a plain command line, not a shell", string(c), cmd)
		default:
			word.WriteByte(c)
			haveWord = true
		}
	}
	if inQuote {
		return nil, fmt.Errorf("copilot launch: unterminated quote in %q", cmd)
	}
	flushStatement()
	return statements, nil
}
