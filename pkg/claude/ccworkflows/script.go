package ccworkflows

// AgentCall is one statically-recovered `agent(...)` call site in a workflow
// script body, with the label/phase the run will tag it with. It is the glue
// used to label in-flight runs (whose journal carries neither): the journal's
// `started` events appear in the same lexical order as these calls.
type AgentCall struct {
	Label string
	Phase string
}

// scriptToken is a coarse lexical token used only by the spawn-order scanner.
type scriptToken struct {
	kind byte // 'i' identifier, 's' string, 'p' punctuation
	text string
	off  int // source offset of the token start
}

// tokenizeScript produces a coarse token stream, collapsing whitespace,
// comments, strings, and numbers so the spawn scanner can match `agent(` /
// `phase(` call sites without tripping over `agent` appearing inside a prompt
// string. It stops cleanly at the first malformed string (truncated source).
func tokenizeScript(src string) []scriptToken {
	var toks []scriptToken
	l := &jsLexer{s: src, i: 0}
	for {
		l.skipTrivia()
		if l.i >= len(src) {
			break
		}
		start := l.i
		c := src[l.i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			v, err := l.string()
			if err != nil {
				return toks
			}
			s, _ := asString(v)
			toks = append(toks, scriptToken{kind: 's', text: s, off: start})
		case isIdentStart(c):
			for l.i < len(src) && isIdentChar(src[l.i]) {
				l.i++
			}
			toks = append(toks, scriptToken{kind: 'i', text: src[start:l.i], off: start})
		default:
			toks = append(toks, scriptToken{kind: 'p', text: string(c), off: start})
			l.i++
		}
	}
	return toks
}

// ParseSpawnOrder statically recovers the ordered list of `agent(...)` calls in
// a workflow script, each tagged with the label/phase it will run under. It
// tracks the most recent `phase('Title')` call so agent() calls that omit an
// explicit `phase:` opt inherit the active phase, mirroring the runtime.
//
// It never executes JS. For data-dependent fan-out (loops, .map, pipeline) the
// static call list may not match the runtime agent count one-to-one — callers
// should consult ScriptHasDynamicSpawns to gauge confidence.
func ParseSpawnOrder(src string) []AgentCall {
	toks := tokenizeScript(src)
	var calls []AgentCall
	currentPhase := ""
	for i := range len(toks) {
		t := toks[i]
		if t.kind != 'i' || i+1 >= len(toks) || toks[i+1].kind != 'p' || toks[i+1].text != "(" {
			continue
		}
		switch t.text {
		case "phase":
			if i+2 < len(toks) && toks[i+2].kind == 's' {
				currentPhase = toks[i+2].text
			}
		case "agent":
			calls = append(calls, AgentCall{
				Label: scanCallOpt(src, toks, i+1, "label", ""),
				Phase: scanCallOpt(src, toks, i+1, "phase", currentPhase),
			})
		}
	}
	return calls
}

// scanCallOpt finds the options object of a call whose opening paren is at
// token index parenIdx, parses it, and returns the requested string field —
// or fallback if the call has no options object or no such field.
func scanCallOpt(src string, toks []scriptToken, parenIdx int, field, fallback string) string {
	depth := 0
	for j := parenIdx; j < len(toks); j++ {
		tj := toks[j]
		if tj.kind != 'p' {
			continue
		}
		switch tj.text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return fallback // reached end of call, no opts object
			}
		case "{":
			if depth == 1 {
				if v, _, err := parseJSValue(src, tj.off); err == nil {
					if om, ok := v.(map[string]any); ok {
						if s, ok := asString(om[field]); ok {
							return s
						}
					}
				}
				return fallback
			}
		}
	}
	return fallback
}

// ScriptHasDynamicSpawns reports whether the script body uses constructs that
// generate agents in a data-dependent way (for/while loops, .map/.forEach,
// Array.from, pipeline) — so a static spawn-order list cannot be trusted to
// align one-to-one with the runtime agent sequence. When true, label/phase
// correlation for in-flight runs is best-effort only.
//
// It scans the token stream (not raw text) so that these words appearing inside
// a prompt string or a comment do NOT falsely demote confidence.
func ScriptHasDynamicSpawns(src string) bool {
	toks := tokenizeScript(src)
	followedByParen := func(i int) bool {
		return i+1 < len(toks) && toks[i+1].kind == 'p' && toks[i+1].text == "("
	}
	for i, t := range toks {
		if t.kind != 'i' {
			continue
		}
		switch t.text {
		case "for", "while", "pipeline":
			if followedByParen(i) {
				return true
			}
		case "map", "forEach":
			// member call: `.map(` / `.forEach(`
			if i > 0 && toks[i-1].kind == 'p' && toks[i-1].text == "." && followedByParen(i) {
				return true
			}
		case "from":
			// `Array.from(`
			if i >= 2 && toks[i-1].kind == 'p' && toks[i-1].text == "." &&
				toks[i-2].kind == 'i' && toks[i-2].text == "Array" && followedByParen(i) {
				return true
			}
		}
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
