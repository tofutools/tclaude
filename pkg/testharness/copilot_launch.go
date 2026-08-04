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

	// Mode is --mode's value, or "" when unset. `--plan` / `--autopilot` are
	// folded into it.
	Mode string

	// Permission posture.
	AllowAllTools   bool
	AllowAllPaths   bool
	AllowAllURLs    bool
	NoAskUser       bool
	DisallowTempDir bool
	AddDirs         []string
	AllowTools      []string
	DenyTools       []string
	AllowURLs       []string
	DenyURLs        []string

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

// ToolsAutoApproved reports whether tool calls run without a confirmation
// prompt. `--allow-all-tools` is the documented flag; the ambient variable is
// strictly stronger and reaches the same place.
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
		case arg == "--allow-all-urls":
			l.AllowAllURLs = true
		case arg == "--allow-all" || arg == "--yolo":
			l.AllowAllTools, l.AllowAllPaths, l.AllowAllURLs = true, true, true
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

		case arg == "--allow-tool", arg == "--deny-tool":
			value, err := copilotFlagValue(args, &i, arg)
			if err != nil {
				return err
			}
			if err := copilotCheckRulePattern(arg, value); err != nil {
				return err
			}
			l.appendRule(arg, value)

		case strings.HasPrefix(arg, "--allow-tool="), strings.HasPrefix(arg, "--deny-tool="):
			flag, value, _ := strings.Cut(arg, "=")
			if err := copilotCheckRulePattern(flag, value); err != nil {
				return err
			}
			l.appendRule(flag, value)

		case arg == "--allow-url", arg == "--deny-url":
			value, err := copilotFlagValue(args, &i, arg)
			if err != nil {
				return err
			}
			l.appendURL(arg, value)

		case arg == "--mode":
			value, err := copilotFlagValue(args, &i, arg)
			if err != nil {
				return err
			}
			switch value {
			case "interactive", "plan", "autopilot":
				l.Mode = value
			default:
				return fmt.Errorf("copilot launch: --mode %q is not one of "+
					"interactive|plan|autopilot", value)
			}
		case arg == "--plan":
			l.Mode = "plan"
		case arg == "--autopilot":
			l.Mode = "autopilot"

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

func (l *CopilotLaunch) appendRule(flag, value string) {
	if flag == "--allow-tool" {
		l.AllowTools = append(l.AllowTools, value)
		return
	}
	l.DenyTools = append(l.DenyTools, value)
}

func (l *CopilotLaunch) appendURL(flag, value string) {
	if flag == "--allow-url" {
		l.AllowURLs = append(l.AllowURLs, value)
		return
	}
	l.DenyURLs = append(l.DenyURLs, value)
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
