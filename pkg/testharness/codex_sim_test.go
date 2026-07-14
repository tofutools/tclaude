package testharness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexEnvelope is the {timestamp, type, payload} line shape every Codex
// rollout line takes. Tests unmarshal into this to assert structure.
type codexEnvelope struct {
	Timestamp string         `json:"timestamp"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	raw       map[string]json.RawMessage
}

// readRollout parses every non-empty line of a rollout into envelopes.
func readRollout(t *testing.T, path string) []codexEnvelope {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rollout: %v", err)
	}
	var out []codexEnvelope
	for i, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("line %d not valid JSON object: %v\n%s", i+1, err, line)
		}
		var env codexEnvelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("line %d not a valid envelope: %v", i+1, err)
		}
		env.raw = raw
		out = append(out, env)
	}
	return out
}

// knownTopLevelTypes / knownEventTypes are the type sets observed in the
// real v0.139 rollout. The sim must stay within them.
var knownTopLevelTypes = map[string]bool{
	"session_meta": true, "event_msg": true,
	"response_item": true, "turn_context": true,
}

var knownEventTypes = map[string]bool{
	"task_started": true, "user_message": true, "agent_message": true,
	"token_count": true, "task_complete": true,
}

func payloadType(env codexEnvelope) string {
	if env.Payload == nil {
		return ""
	}
	s, _ := env.Payload["type"].(string)
	return s
}

func TestCodexSim_EmitsValidEnvelopes(t *testing.T) {
	home := t.TempDir()
	cx := NewCodexSim(t, home, "/home/gigur/git/testcodex")
	if err := cx.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cx.WriteExchange("hello", "Hello. How can I help?"); err != nil {
		t.Fatalf("WriteExchange: %v", err)
	}

	envs := readRollout(t, cx.RolloutPath)
	if len(envs) == 0 {
		t.Fatal("no lines written")
	}
	for i, env := range envs {
		// Every line carries exactly the three envelope keys.
		if len(env.raw) != 3 {
			t.Errorf("line %d: want 3 envelope keys, got %d (%v)", i+1, len(env.raw), keysOf(env.raw))
		}
		if env.Timestamp == "" {
			t.Errorf("line %d: empty timestamp", i+1)
		}
		if _, err := time.Parse("2006-01-02T15:04:05.000Z", env.Timestamp); err != nil {
			t.Errorf("line %d: timestamp %q not Codex-shaped: %v", i+1, env.Timestamp, err)
		}
		if !knownTopLevelTypes[env.Type] {
			t.Errorf("line %d: unknown top-level type %q", i+1, env.Type)
		}
		if env.Type == "event_msg" && !knownEventTypes[payloadType(env)] {
			t.Errorf("line %d: unknown event_msg payload.type %q", i+1, payloadType(env))
		}
	}
}

func TestCodexSim_SessionMetaShape(t *testing.T) {
	home := t.TempDir()
	cwd := "/home/gigur/git/testcodex"
	cx := NewCodexSim(t, home, cwd)
	if err := cx.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	envs := readRollout(t, cx.RolloutPath)
	first := envs[0]
	if first.Type != "session_meta" {
		t.Fatalf("first line type = %q, want session_meta", first.Type)
	}
	p := first.Payload
	checkStr(t, p, "id", cx.ConvID)
	checkStr(t, p, "cwd", cwd)
	checkStr(t, p, "cli_version", "0.139.0")
	checkStr(t, p, "model_provider", "openai")
	checkStr(t, p, "originator", "codex-tui")
	checkStr(t, p, "source", "cli")
	// base_instructions is an object with a text field (shape, not value).
	bi, ok := p["base_instructions"].(map[string]any)
	if !ok {
		t.Fatalf("base_instructions not an object: %T", p["base_instructions"])
	}
	if _, ok := bi["text"].(string); !ok {
		t.Errorf("base_instructions.text not a string")
	}
}

func TestCodexSim_NextEventTimeConfiguredBeforeStartSkipsSessionMeta(t *testing.T) {
	home := t.TempDir()
	cx := NewCodexSim(t, home, "/work")
	next := time.Date(2026, time.July, 14, 12, 0, 0, 321_000_000, time.UTC)
	cx.SetNextEventTime(next)

	if err := cx.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cx.WriteTokenCount(CodexTokenUsage{}, CodexTokenUsage{}); err != nil {
		t.Fatalf("WriteTokenCount: %v", err)
	}

	envs := readRollout(t, cx.RolloutPath)
	if len(envs) != 2 {
		t.Fatalf("rollout lines = %d, want session metadata plus one event", len(envs))
	}
	metaTime, err := time.Parse("2006-01-02T15:04:05.000Z", envs[0].Timestamp)
	if err != nil {
		t.Fatalf("parse session metadata timestamp: %v", err)
	}
	if metaTime.Unix() != cx.CreatedUnix() {
		t.Errorf("session metadata unix time = %d, want CreatedUnix %d", metaTime.Unix(), cx.CreatedUnix())
	}
	if got, _ := envs[0].Payload["timestamp"].(string); got != envs[0].Timestamp {
		t.Errorf("session metadata payload timestamp = %q, want envelope timestamp %q", got, envs[0].Timestamp)
	}
	if got, want := envs[1].Timestamp, formatCodexTime(next); got != want {
		t.Errorf("first event timestamp = %q, want %q", got, want)
	}
}

func TestCodexSim_UserTurnLines(t *testing.T) {
	home := t.TempDir()
	cx := NewCodexSim(t, home, "/work")
	if err := cx.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := cx.WriteUserInput("ship it"); err != nil {
		t.Fatalf("WriteUserInput: %v", err)
	}
	envs := readRollout(t, cx.RolloutPath)

	// Expect, after session_meta: task_started, turn_context, user
	// response_item, user_message event.
	types := lineSignature(envs)
	want := []string{
		"session_meta",
		"event_msg/task_started",
		"turn_context",
		"response_item/message",
		"event_msg/user_message",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("turn signature:\n got %v\nwant %v", types, want)
	}

	// turn_context carries the cwd and model the read path needs.
	tc := findByType(envs, "turn_context")
	checkStr(t, tc.Payload, "cwd", "/work")
	checkStr(t, tc.Payload, "model", "gpt-5.5")

	// The user response_item content carries the input verbatim.
	ri := findByType(envs, "response_item")
	if got := messageText(t, ri); got != "ship it" {
		t.Errorf("user message text = %q, want %q", got, "ship it")
	}
}

func TestCodexSim_FullExchangeShape(t *testing.T) {
	home := t.TempDir()
	cx := NewCodexSim(t, home, "/work")
	_ = cx.Start()
	if err := cx.WriteExchange("hi", "yo"); err != nil {
		t.Fatalf("WriteExchange: %v", err)
	}
	envs := readRollout(t, cx.RolloutPath)
	sig := strings.Join(lineSignature(envs), ",")

	for _, must := range []string{
		"event_msg/agent_message",
		"event_msg/token_count",
		"event_msg/task_complete",
	} {
		if !strings.Contains(sig, must) {
			t.Errorf("full exchange missing %q; signature=%s", must, sig)
		}
	}

	// token_count carries the nested usage telemetry (JOH-170 shape).
	tc := findByEventType(envs, "token_count")
	info, ok := tc.Payload["info"].(map[string]any)
	if !ok {
		t.Fatal("token_count.info missing")
	}
	for _, k := range []string{"total_token_usage", "last_token_usage", "model_context_window"} {
		if _, ok := info[k]; !ok {
			t.Errorf("token_count.info missing %q", k)
		}
	}
	total, ok := info["total_token_usage"].(map[string]any)
	if !ok {
		t.Fatal("total_token_usage not an object")
	}
	for _, k := range []string{"input_tokens", "cached_input_tokens", "output_tokens", "total_tokens"} {
		if _, ok := total[k]; !ok {
			t.Errorf("total_token_usage missing %q", k)
		}
	}
}

// TestCodexSim_RoundTripAgainstRealFixture proves the sim's output is
// structurally interchangeable with a real captured v0.139 rollout: same
// envelope keys, same top-level type set, same event_msg payload.type
// set, same session_meta key set.
func TestCodexSim_RoundTripAgainstRealFixture(t *testing.T) {
	real := readRollout(t, filepath.Join("testdata", "codex_rollout_v0139.jsonl"))

	home := t.TempDir()
	cx := NewCodexSim(t, home, "/home/gigur/git/testcodex")
	_ = cx.Start()
	_ = cx.WriteExchange("hello", "Hello. How can I help?")
	sim := readRollout(t, cx.RolloutPath)

	// Envelope discipline: every real and sim line has the same 3 keys.
	for label, set := range map[string][]codexEnvelope{"real": real, "sim": sim} {
		for i, e := range set {
			if len(e.raw) != 3 {
				t.Errorf("%s line %d: %d keys %v, want timestamp/type/payload", label, i+1, len(e.raw), keysOf(e.raw))
			}
		}
	}

	// Every top-level type the sim emits occurs in the real file.
	realTop := typeSet(real, func(e codexEnvelope) string { return e.Type })
	for ty := range typeSet(sim, func(e codexEnvelope) string { return e.Type }) {
		if !realTop[ty] {
			t.Errorf("sim emits top-level type %q not present in real rollout", ty)
		}
	}

	// Every event_msg payload.type the sim emits occurs in the real file.
	realEvt := eventTypeSet(real)
	for ty := range eventTypeSet(sim) {
		if !realEvt[ty] {
			t.Errorf("sim emits event_msg type %q not present in real rollout", ty)
		}
	}

	// session_meta key set: the sim must carry every key the real
	// session_meta has (so the read path finds what it expects).
	realMeta := findByType(real, "session_meta")
	simMeta := findByType(sim, "session_meta")
	for k := range realMeta.Payload {
		if _, ok := simMeta.Payload[k]; !ok {
			t.Errorf("sim session_meta missing key %q present in real rollout", k)
		}
	}
}

func TestCodexSim_ReceiveBuffersUntilEnter(t *testing.T) {
	home := t.TempDir()
	cx := NewCodexSim(t, home, "/work")
	_ = cx.Start()

	cx.Receive("hel")
	cx.Receive("lo")
	// Nothing flushed yet — only session_meta on disk.
	if envs := readRollout(t, cx.RolloutPath); len(envs) != 1 {
		t.Fatalf("before Enter: %d lines, want 1 (session_meta)", len(envs))
	}
	cx.Receive("Enter")
	envs := readRollout(t, cx.RolloutPath)
	if got := messageText(t, findByType(envs, "response_item")); got != "hello" {
		t.Errorf("flushed message = %q, want %q", got, "hello")
	}
}

func TestCodexSim_TitleFromFirstUserMessage(t *testing.T) {
	home := t.TempDir()
	cx := NewCodexSim(t, home, "/work")
	_ = cx.Start()
	_ = cx.WriteUserInput("implement the parser")
	if got := cx.Title(); got != "implement the parser" {
		t.Errorf("Title() = %q, want derived from first message", got)
	}
	// A second message does not overwrite the derived title.
	_ = cx.WriteUserInput("second prompt")
	if got := cx.Title(); got != "implement the parser" {
		t.Errorf("Title() changed on second message: %q", got)
	}
}

func TestCodexSim_OnInputOverride(t *testing.T) {
	home := t.TempDir()
	cx := NewCodexSim(t, home, "/work")
	_ = cx.Start()

	cx.OnInput("/quit", func(c *CodexSim, _ string) bool {
		c.MarkDead()
		return true
	})
	cx.Receive("/quit")
	cx.Receive("Enter")
	if cx.IsAlive() {
		t.Error("sim still alive after /quit handler")
	}
}

func TestCodexSim_CommandDelayIsAsync(t *testing.T) {
	home := t.TempDir()
	cx := NewCodexSim(t, home, "/work")
	_ = cx.Start()
	started, release, done := cx.SetCommandBarrier("")
	t.Cleanup(release)

	// Empty submissions are discarded and must not consume the next-command
	// barrier, even though an empty prefix otherwise matches every command.
	cx.Receive("Enter")
	cx.Receive("slow input")
	returned := make(chan struct{})
	go func() {
		cx.Receive("Enter")
		close(returned)
	}()
	// The handler reaches its barrier, and Receive itself must return while the
	// handler remains causally held immediately before dispatch.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("delayed command did not reach its dispatch barrier")
	}
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("Receive did not return while the delayed handler was blocked")
	}
	if envs := readRollout(t, cx.RolloutPath); len(envs) != 1 {
		t.Fatalf("turn landed synchronously despite delay: %d lines", len(envs))
	}
	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("delayed command did not finish after barrier release")
	}
	if envs := readRollout(t, cx.RolloutPath); len(envs) <= 1 {
		t.Fatalf("delayed turn did not land after release: %d lines", len(envs))
	}
}

func TestCodexSim_RolloutPathIsDateIndexed(t *testing.T) {
	home := t.TempDir()
	cx := NewCodexSim(t, home, "/work")
	rel, err := filepath.Rel(filepath.Join(home, ".codex", "sessions"), cx.RolloutPath)
	if err != nil {
		t.Fatalf("rollout path not under sessions tree: %v", err)
	}
	// YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 4 {
		t.Fatalf("path layout = %v, want YYYY/MM/DD/file", parts)
	}
	if !strings.HasPrefix(parts[3], "rollout-") || !strings.HasSuffix(parts[3], cx.ConvID+".jsonl") {
		t.Errorf("filename %q not rollout-<ts>-<id>.jsonl", parts[3])
	}
}

func TestCodexSim_HydrateResumesExistingRollout(t *testing.T) {
	home := t.TempDir()
	id := generateConvID()
	cx := NewCodexSimWithID(t, home, id, "/work")
	_ = cx.Start()
	_ = cx.WriteUserInput("original prompt")
	cx.Shutdown()

	// Resume: a fresh CodexSim that locates the rollout by id.
	resumed := HydrateCodexSim(t, home, id, "/work")
	if resumed.RolloutPath != cx.RolloutPath {
		t.Errorf("resumed path %q != original %q", resumed.RolloutPath, cx.RolloutPath)
	}
	if got := resumed.Title(); got != "original prompt" {
		t.Errorf("resumed Title() = %q, want derived from first message", got)
	}
	// Resume re-arms alive and appends to the SAME file.
	if err := resumed.Start(); err != nil {
		t.Fatalf("resume Start: %v", err)
	}
	_ = resumed.WriteUserInput("follow-up")
	envs := readRollout(t, resumed.RolloutPath)
	// session_meta written once; both user turns present.
	if n := countType(envs, "session_meta"); n != 1 {
		t.Errorf("session_meta count = %d, want 1 (not rewritten on resume)", n)
	}
	if n := countEventType(envs, "user_message"); n != 2 {
		t.Errorf("user_message count = %d, want 2", n)
	}
}

// --- helpers ---

func keysOf(m map[string]json.RawMessage) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func lineSignature(envs []codexEnvelope) []string {
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		if pt := payloadType(e); pt != "" {
			out = append(out, e.Type+"/"+pt)
		} else {
			out = append(out, e.Type)
		}
	}
	return out
}

func findByType(envs []codexEnvelope, typ string) codexEnvelope {
	for _, e := range envs {
		if e.Type == typ {
			return e
		}
	}
	return codexEnvelope{}
}

func findByEventType(envs []codexEnvelope, evt string) codexEnvelope {
	for _, e := range envs {
		if e.Type == "event_msg" && payloadType(e) == evt {
			return e
		}
	}
	return codexEnvelope{}
}

func countType(envs []codexEnvelope, typ string) int {
	n := 0
	for _, e := range envs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func countEventType(envs []codexEnvelope, evt string) int {
	n := 0
	for _, e := range envs {
		if e.Type == "event_msg" && payloadType(e) == evt {
			n++
		}
	}
	return n
}

func typeSet(envs []codexEnvelope, key func(codexEnvelope) string) map[string]bool {
	s := map[string]bool{}
	for _, e := range envs {
		s[key(e)] = true
	}
	return s
}

func eventTypeSet(envs []codexEnvelope) map[string]bool {
	s := map[string]bool{}
	for _, e := range envs {
		if e.Type == "event_msg" {
			s[payloadType(e)] = true
		}
	}
	return s
}

func messageText(t *testing.T, env codexEnvelope) string {
	t.Helper()
	content, ok := env.Payload["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("message has no content array: %v", env.Payload)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] not an object")
	}
	s, _ := first["text"].(string)
	return s
}

func checkStr(t *testing.T, m map[string]any, key, want string) {
	t.Helper()
	got, _ := m[key].(string)
	if got != want {
		t.Errorf("payload[%q] = %q, want %q", key, got, want)
	}
}
