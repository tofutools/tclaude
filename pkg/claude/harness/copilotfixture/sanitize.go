package copilotfixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// This file is the gate between "what a run produced" and "what may be
// committed". Everything written to testdata passes through it.
//
// Three classes of content are handled differently:
//
//   - Volatile identity (UUIDs, event ids, timestamps, ports, absolute paths)
//     is REPLACED with a stable placeholder. It differs every run, so leaving
//     it in would make goldens meaningless; it also encodes private paths.
//   - Bulk model input (the ~26 kB system prompt, the tool schemas, the
//     wrapped user content) is REDUCED to shape plus a digest. Committing it
//     would be a large version-coupled blob that churns on every CLI bump
//     while proving nothing tclaude relies on, and it is precisely the
//     "large raw prompt" TCL-970 forbids.
//   - Structure tclaude actually depends on (endpoint path, body key set,
//     message roles, tool-name set, stream flags, the x-initiator
//     discriminator) is KEPT verbatim. That is the compatibility evidence.

var (
	uuidRE = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// RFC3339-ish, with or without fractional seconds and offset.
	timestampRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	loopbackRE  = regexp.MustCompile(`http://127\.0\.0\.1:\d+`)

	// Copilot injects a host-probed block into its system prompt:
	//
	//	<environment_context>
	//	* Current working directory: ...
	//	* Git repository root: Not a git repository
	//	* Operating System: Linux
	//	* Available tools: git, curl
	//	</environment_context>
	//
	// "Operating System" comes from os.type() and "Available tools" from a
	// live PATH probe, so this block differs between hosts. Left in, it would
	// poison PromptDigest: a container without curl, or any macOS developer,
	// would see four goldens fail and read it as Copilot contract drift. Worse,
	// re-recording with -update on macOS would bake Darwin digests in and break
	// Linux CI.
	//
	// Removing the whole block keeps the digest sensitive to what it is meant
	// to detect — a change in the CLI's actual prompting — while dropping
	// host-derived noise it can never distinguish from that.
	environmentContextRE = regexp.MustCompile(`(?s)<environment_context>.*?</environment_context>`)
)

// Placeholders written into goldens.
const (
	uuidPlaceholder       = "<uuid>"
	timestampPlaceholder  = "<timestamp>"
	baseURLPlaceholder    = "http://127.0.0.1:<port>"
	pathPlaceholder       = "<tmp>"
	envContextPlaceholder = "<environment_context/>"
	pinnedSessionIDToken  = "<pinned-session-id>"
)

// Sanitizer normalizes one run's artifacts. Paths are supplied because they
// are per-run temporary directories that must never reach a golden.
type Sanitizer struct {
	replacements []replacement

	// pinnedSessionID is a session id the SCENARIO chose rather than one
	// Copilot minted. It maps to its own placeholder so a golden can show that
	// enrollment was honoured; without it the generic UUID rule would erase
	// the distinction and the resume fixture would look identical whether or
	// not --session-id worked.
	pinnedSessionID string
}

type replacement struct{ from, to string }

// NewSanitizer builds a sanitizer that maps this run's disposable directories
// onto stable placeholders. Longest path first so nested dirs (cache inside
// root) cannot be partially rewritten by their parent.
func NewSanitizer(home, cache, workDir string) *Sanitizer {
	pairs := []replacement{
		{home, pathPlaceholder + "/home"},
		{cache, pathPlaceholder + "/cache"},
		{workDir, pathPlaceholder + "/work"},
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return len(pairs[i].from) > len(pairs[j].from)
	})
	return &Sanitizer{replacements: pairs}
}

// WithPinnedSessionID marks id as caller-chosen for this sanitizer.
func (s *Sanitizer) WithPinnedSessionID(id string) *Sanitizer {
	s.pinnedSessionID = id
	return s
}

// Text normalizes a free-text blob: host-probed prompt scaffolding and
// run-specific paths first (the most specific), then a pinned session id
// before the generic UUID rule could swallow it, then loopback ports, the
// remaining UUIDs and timestamps.
func (s *Sanitizer) Text(in string) string {
	out := environmentContextRE.ReplaceAllString(in, envContextPlaceholder)
	for _, r := range s.replacements {
		if r.from != "" {
			out = strings.ReplaceAll(out, r.from, r.to)
		}
	}
	if s.pinnedSessionID != "" {
		out = strings.ReplaceAll(out, s.pinnedSessionID, pinnedSessionIDToken)
	}
	out = loopbackRE.ReplaceAllString(out, baseURLPlaceholder)
	out = uuidRE.ReplaceAllString(out, uuidPlaceholder)
	out = timestampRE.ReplaceAllString(out, timestampPlaceholder)
	return out
}

// RequestObservation is the committable projection of one provider request.
// It answers "what shape did the CLI send", never "what text did it send".
type RequestObservation struct {
	// Path is the endpoint. On the completions wire this is the only route the
	// CLI ever contacts.
	Path string `json:"path"`

	// Initiator is the x-initiator header: "user" on the first call of a turn,
	// "agent" on a tool follow-up. A stable, cheap discriminator.
	Initiator string `json:"initiator"`

	// InteractionType is x-interaction-type, present only on the
	// user-initiated call.
	InteractionType string `json:"interactionType,omitempty"`

	// AuthorizationPresent records that the header exists; AuthorizationEmpty
	// records that it carries no credential. Both true is the credential-free
	// signature: the SDK always sends the header, but with an empty bearer.
	AuthorizationPresent bool `json:"authorizationPresent"`
	AuthorizationEmpty   bool `json:"authorizationEmpty"`

	// BodyKeys is the sorted top-level key set — the wire contract itself.
	BodyKeys []string `json:"bodyKeys"`

	// Model is the wire model, which is what --model / COPILOT_MODEL
	// precedence is asserted through.
	Model string `json:"model"`

	Stream             bool `json:"stream"`
	StreamIncludeUsage bool `json:"streamIncludeUsage"`

	// MessageRoles is the conversation shape. A resume shows its history here
	// as extra assistant/user entries; a tool follow-up shows assistant+tool.
	MessageRoles []string `json:"messageRoles"`

	// ToolNames is the sorted tool catalog offered to the model. The SET is
	// stable enough to be evidence; the schemas behind it are not, so only
	// names are kept.
	ToolNames []string `json:"toolNames"`

	// ReasoningEffort is populated only on the responses wire; the completions
	// wire carries no effort key at all.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`

	// PromptDigest fingerprints the bulk model input (system prompt plus the
	// wrapped user content) after normalization. It changes when the CLI
	// changes its prompting, which is exactly the drift signal wanted, without
	// committing the prompt itself.
	PromptDigest string `json:"promptDigest"`
}

// Request projects a recorded request into its committable observation.
func (s *Sanitizer) Request(r RecordedRequest) RequestObservation {
	obs := RequestObservation{
		Path:            r.Path,
		Initiator:       r.Header.Get("X-Initiator"),
		InteractionType: r.Header.Get("X-Interaction-Type"),
	}
	if auth, ok := r.Header["Authorization"]; ok {
		obs.AuthorizationPresent = true
		// The CLI sends "Bearer" or "Bearer " with no token when no provider
		// credential is configured. Anything longer means a credential leaked in.
		obs.AuthorizationEmpty = strings.TrimSpace(strings.TrimPrefix(
			strings.TrimSpace(strings.Join(auth, "")), "Bearer")) == ""
	}

	for k := range r.Body {
		obs.BodyKeys = append(obs.BodyKeys, k)
	}
	sort.Strings(obs.BodyKeys)

	obs.Model, _ = r.Body["model"].(string)
	obs.Stream, _ = r.Body["stream"].(bool)
	if so, ok := r.Body["stream_options"].(map[string]any); ok {
		obs.StreamIncludeUsage, _ = so["include_usage"].(bool)
	}
	if reasoning, ok := r.Body["reasoning"].(map[string]any); ok {
		obs.ReasoningEffort, _ = reasoning["effort"].(string)
	}

	var bulk strings.Builder
	appendBulk := func(role, content string) {
		bulk.WriteString(role)
		bulk.WriteByte('\x00')
		// Normalized before digesting so the injected <current_datetime>, the
		// host-probed environment block and any absolute path cannot make the
		// digest differ between two otherwise identical runs.
		bulk.WriteString(s.Text(content))
		bulk.WriteByte('\x00')
	}

	// The two wires carry the conversation differently: completions sends
	// messages[] with the system prompt as its first entry, responses sends
	// input[] plus a separate top-level `instructions` string. Both are
	// projected onto the same MessageRoles/PromptDigest pair so a fixture reads
	// the same way regardless of wire.
	if instructions, ok := r.Body["instructions"].(string); ok {
		obs.MessageRoles = append(obs.MessageRoles, "instructions")
		appendBulk("instructions", instructions)
	}
	for _, key := range []string{"messages", "input"} {
		entries, ok := r.Body[key].([]any)
		if !ok {
			continue
		}
		for _, raw := range entries {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			obs.MessageRoles = append(obs.MessageRoles, role)
			if content, ok := m["content"].(string); ok {
				appendBulk(role, content)
			}
		}
	}
	obs.PromptDigest = digest(bulk.String())

	if tools, ok := r.Body["tools"].([]any); ok {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// Completions nests the name under `function`; responses puts it
			// at the top level of the tool entry.
			if fn, ok := tool["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					obs.ToolNames = append(obs.ToolNames, name)
				}
				continue
			}
			if name, ok := tool["name"].(string); ok {
				obs.ToolNames = append(obs.ToolNames, name)
			}
		}
		sort.Strings(obs.ToolNames)
	}
	return obs
}

// Requests projects a whole run's traffic.
func (s *Sanitizer) Requests(rs []RecordedRequest) []RequestObservation {
	out := make([]RequestObservation, 0, len(rs))
	for _, r := range rs {
		out = append(out, s.Request(r))
	}
	return out
}

// EventObservation is the committable projection of the CLI's event stream.
type EventObservation struct {
	// Types is the ordered event-type sequence, the stable part of the stream.
	Types []string `json:"types"`
	// ResultExitCode and ResultSessionID come from the terminal result line.
	ResultExitCode  *int   `json:"resultExitCode,omitempty"`
	ResultSessionID string `json:"resultSessionId,omitempty"`
	// AssistantText is the assembled assistant message. Safe to commit because
	// the mock authored it, so it is fixture content rather than model output.
	AssistantText string `json:"assistantText,omitempty"`
}

// Events projects a run's event stream.
func (s *Sanitizer) Events(r RunResult) EventObservation {
	obs := EventObservation{Types: r.EventTypes()}
	if result, ok := r.Result(); ok {
		obs.ResultExitCode = result.ExitCode
		// A minted id normalizes to <uuid>; a scenario-pinned id normalizes to
		// its own token, so the golden records that enrollment was honoured
		// rather than erasing the difference.
		obs.ResultSessionID = s.Text(result.SessionID)
	}
	var text strings.Builder
	for _, e := range r.Events {
		if e.Type != "assistant.message" {
			continue
		}
		var data struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(e.Data, &data); err == nil {
			text.WriteString(data.Content)
		}
	}
	obs.AssistantText = s.Text(text.String())
	return obs
}

// SessionLayout is the committable projection of ONE directory tree after a
// run — the evidence TCL-972/TCL-975 need about what Copilot creates and
// where.
//
// It was written for COPILOT_HOME and is now the per-root element of
// BaselineLayout, which projects COPILOT_HOME, the package cache, the XDG
// cache and the working directory alike. Nothing about the shape is
// COPILOT_HOME-specific; the root is whatever the caller walked.
type SessionLayout struct {
	// Entries are paths relative to the walked root, with volatile segments
	// normalized, sorted. Contents are never captured: session.db and
	// events.jsonl hold conversation content.
	Entries []string `json:"entries"`
}

// Digest of arbitrary text, truncated to a readable prefix. Full SHA-256 would
// add noise without adding drift sensitivity.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// Marshal renders an observation as the stable indented JSON committed to
// testdata.
func Marshal(v any) ([]byte, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("copilotfixture: marshal observation: %w", err)
	}
	return append(out, '\n'), nil
}
