package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

const (
	processSnippetEnvelopeKind       = "tclaude/process-selection"
	processSnippetEnvelopeVersion    = 1
	processSnippetEnvelopePrefix     = "tclaude-process-selection:v1\n"
	processSnippetMaxNodeIDBytes     = 128
	processSnippetMaxOutcomeUnits    = 512
	processSnippetMaxCoordinate      = 1_000_000
	processSnippetMaxJSONDepth       = 32
	processSnippetMaxJSONItems       = 32_768
	processSnippetRequestOverhead    = 4 << 10
	processSnippetUnavailableMessage = "This custom snippet is unavailable because its stored format is invalid or unsupported."
)

var processSnippetNodeID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type processSnippetEnvelope struct {
	Kind    string                       `json:"kind"`
	Version json.Number                  `json:"version"`
	Nodes   []processSnippetEnvelopeNode `json:"nodes"`
	Edges   []model.Edge                 `json:"edges"`
}

type processSnippetEnvelopeNode struct {
	ID              string          `json:"id"`
	Node            json.RawMessage `json:"node"`
	Position        json.RawMessage `json:"position"`
	decoded         model.Node
	decodedPosition processSnippetPosition
}

type processSnippetPosition struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type canonicalProcessSnippetEnvelope struct {
	Kind    string                        `json:"kind"`
	Version int                           `json:"version"`
	Nodes   []canonicalProcessSnippetNode `json:"nodes"`
	Edges   []model.Edge                  `json:"edges"`
}

type canonicalProcessSnippetNode struct {
	ID       string                 `json:"id"`
	Node     model.Node             `json:"node"`
	Position processSnippetPosition `json:"position"`
}

type processSnippetMutationBody struct {
	Name     string          `json:"name"`
	Envelope json.RawMessage `json:"envelope,omitempty"`
	Revision int64           `json:"revision,omitempty"`
}

type processSnippetView struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Revision    int64           `json:"revision"`
	CreatedAt   string          `json:"createdAt,omitempty"`
	UpdatedAt   string          `json:"updatedAt,omitempty"`
	Available   bool            `json:"available"`
	Envelope    json.RawMessage `json:"envelope,omitempty"`
	Unavailable string          `json:"unavailableReason,omitempty"`
}

func registerDashboardProcessSnippetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/process/snippets", dashboardProcessSnippetRoute(handleProcessSnippetList))
	mux.HandleFunc("POST /api/process/snippets", dashboardProcessSnippetRoute(handleProcessSnippetCreate))
	mux.HandleFunc("PATCH /api/process/snippets/{id}", dashboardProcessSnippetRoute(handleProcessSnippetRename))
	mux.HandleFunc("DELETE /api/process/snippets/{id}", dashboardProcessSnippetRoute(handleProcessSnippetDelete))
}

func dashboardProcessSnippetRoute(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		if !processRoutesEnabled() {
			http.NotFound(w, r)
			return
		}
		next(w, r)
	}
}

func strictJSON(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func decodeProcessSnippetBody(w http.ResponseWriter, r *http.Request) (processSnippetMutationBody, error) {
	limit := int64(db.MaxProcessSnippetEnvelopeBytes + processSnippetRequestOverhead)
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		return processSnippetMutationBody{}, fmt.Errorf("request exceeds the custom snippet size limit")
	}
	var body processSnippetMutationBody
	if err := strictJSON(raw, &body); err != nil {
		return processSnippetMutationBody{}, fmt.Errorf("request is not valid custom snippet JSON")
	}
	return body, nil
}

func normalizeProcessSnippetName(value string) (name, key string, err error) {
	name = strings.TrimSpace(value)
	if name == "" {
		return "", "", errors.New("custom snippet name is required")
	}
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > db.MaxProcessSnippetNameRunes || len(name) > db.MaxProcessSnippetNameBytes {
		return "", "", fmt.Errorf("custom snippet name must be at most %d characters and %d UTF-8 bytes", db.MaxProcessSnippetNameRunes, db.MaxProcessSnippetNameBytes)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", "", errors.New("custom snippet name cannot contain control characters")
		}
	}
	// The explicit uniqueness rule is trim + Unicode lower-case. Internal
	// whitespace is preserved and surfaced exactly as named.
	return name, strings.ToLower(name), nil
}

func validateSnippetJSONValue(value any) error {
	items := 0
	var walk func(any, int) error
	walk = func(candidate any, depth int) error {
		items++
		if items > processSnippetMaxJSONItems || depth > processSnippetMaxJSONDepth {
			return errors.New("custom snippet node data exceeds the supported structure limits")
		}
		switch value := candidate.(type) {
		case nil, string, bool, json.Number:
			return nil
		case float64:
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return errors.New("custom snippet node data contains an invalid number")
			}
			return nil
		case []any:
			for _, item := range value {
				if err := walk(item, depth+1); err != nil {
					return err
				}
			}
			return nil
		case map[string]any:
			for key, item := range value {
				if strings.IndexFunc(key, unicode.IsControl) >= 0 {
					return errors.New("custom snippet node data has an invalid field name")
				}
				if err := walk(item, depth+1); err != nil {
					return err
				}
			}
			return nil
		default:
			return errors.New("custom snippet node data has an unsupported value")
		}
	}
	return walk(value, 0)
}

func snippetWireRecord(value any, fields ...string) (map[string]any, error) {
	record, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("custom snippet contains incompatible process node data")
	}
	for field := range record {
		if !slices.Contains(fields, field) {
			return nil, errors.New("custom snippet contains incompatible process node data")
		}
	}
	return record, nil
}

func snippetOptionalStrings(record map[string]any, fields ...string) error {
	for _, field := range fields {
		if value, present := record[field]; present {
			if _, ok := value.(string); !ok {
				return errors.New("custom snippet contains incompatible process node data")
			}
		}
	}
	return nil
}

func snippetStringList(value any) error {
	items, ok := value.([]any)
	if !ok {
		return errors.New("custom snippet contains incompatible process node data")
	}
	for _, item := range items {
		if _, ok := item.(string); !ok {
			return errors.New("custom snippet contains incompatible process node data")
		}
	}
	return nil
}

func snippetStringMap(value any) error {
	record, ok := value.(map[string]any)
	if !ok {
		return errors.New("custom snippet contains incompatible process node data")
	}
	for key, item := range record {
		if key == "__proto__" || key == "prototype" || key == "constructor" {
			return errors.New("custom snippet contains incompatible process node data")
		}
		if _, ok := item.(string); !ok {
			return errors.New("custom snippet contains incompatible process node data")
		}
	}
	return nil
}

func snippetSafeInteger(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.Trunc(parsed) != parsed || math.Abs(parsed) > 9_007_199_254_740_991 {
		return 0, false
	}
	return int64(parsed), true
}

func normalizeSnippetSafeInteger(record map[string]any, field string) error {
	value, present := record[field]
	if !present {
		return nil
	}
	normalized, ok := snippetSafeInteger(value)
	if !ok {
		return errors.New("custom snippet contains incompatible process node data")
	}
	// JSON.parse erases exponent, decimal, and negative-zero spellings before
	// the browser authority applies Number.isSafeInteger. Mirror that semantic
	// value before decoding into the model's typed int fields.
	record[field] = json.Number(strconv.FormatInt(normalized, 10))
	return nil
}

func validateSnippetRetryWire(value any) error {
	record, err := snippetWireRecord(value, "maxAttempts", "backoff", "onFail")
	if err != nil {
		return err
	}
	if err := normalizeSnippetSafeInteger(record, "maxAttempts"); err != nil {
		return err
	}
	return snippetOptionalStrings(record, "backoff", "onFail")
}

func validateSnippetPerformerWire(value any) error {
	record, err := snippetWireRecord(value,
		"kind", "profile", "prompt", "ask", "choices", "choiceOutcomes", "assignee",
		"model", "effort", "run", "args", "timeout", "contact")
	if err != nil {
		return err
	}
	kind, ok := record["kind"].(string)
	if !ok || (kind != string(model.PerformerHuman) && kind != string(model.PerformerAgent) && kind != string(model.PerformerProgram)) {
		return errors.New("custom snippet contains incompatible process node data")
	}
	if err := snippetOptionalStrings(record, "profile", "prompt", "ask", "assignee", "model", "effort", "run", "timeout"); err != nil {
		return err
	}
	for _, field := range []string{"choices", "args"} {
		if item, present := record[field]; present {
			if err := snippetStringList(item); err != nil {
				return err
			}
		}
	}
	if item, present := record["choiceOutcomes"]; present {
		if err := snippetStringMap(item); err != nil {
			return err
		}
	}
	if item, present := record["contact"]; present {
		contact, err := snippetWireRecord(item, "cadence", "budget", "escalationTarget")
		if err != nil {
			return err
		}
		if err := normalizeSnippetSafeInteger(contact, "budget"); err != nil {
			return err
		}
		if err := snippetOptionalStrings(contact, "cadence", "escalationTarget"); err != nil {
			return err
		}
	}
	return nil
}

func validateSnippetStepWire(value any) error {
	record, err := snippetWireRecord(value,
		"id", "name", "description", "doc", "performer", "approval", "approvalRetry", "retry")
	if err != nil {
		return err
	}
	if err := snippetOptionalStrings(record, "id", "name", "description", "doc", "approval"); err != nil {
		return err
	}
	if err := validateSnippetPerformerWire(record["performer"]); err != nil {
		return err
	}
	for _, field := range []string{"approvalRetry", "retry"} {
		if retry, present := record[field]; present {
			if err := validateSnippetRetryWire(retry); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSnippetNodeWire(value any) error {
	record, err := snippetWireRecord(value,
		"type", "join", "name", "description", "doc", "performer", "plan", "checks",
		"review", "retry", "wait", "next", "result", "captures", "metadata")
	if err != nil {
		return err
	}
	typeName, ok := record["type"].(string)
	if !ok || !slices.Contains([]string{
		string(model.NodeTypeTask), string(model.NodeTypeDecision), string(model.NodeTypeParallel),
		string(model.NodeTypeWait), string(model.NodeTypeStart), string(model.NodeTypeEnd),
	}, typeName) {
		return errors.New("custom snippet contains incompatible process node data")
	}
	if err := snippetOptionalStrings(record, "join", "name", "description", "doc", "result"); err != nil {
		return err
	}
	if performer, present := record["performer"]; present {
		if err := validateSnippetPerformerWire(performer); err != nil {
			return err
		}
	}
	for _, field := range []string{"plan", "review"} {
		if step, present := record[field]; present {
			if err := validateSnippetStepWire(step); err != nil {
				return err
			}
		}
	}
	if checks, present := record["checks"]; present {
		items, ok := checks.([]any)
		if !ok {
			return errors.New("custom snippet contains incompatible process node data")
		}
		for _, step := range items {
			if err := validateSnippetStepWire(step); err != nil {
				return err
			}
		}
	}
	if retry, present := record["retry"]; present {
		if err := validateSnippetRetryWire(retry); err != nil {
			return err
		}
	}
	if wait, present := record["wait"]; present {
		config, err := snippetWireRecord(wait, "duration", "until", "signal")
		if err != nil {
			return err
		}
		if err := snippetOptionalStrings(config, "duration", "until", "signal"); err != nil {
			return err
		}
	}
	if captures, present := record["captures"]; present {
		if err := snippetStringList(captures); err != nil {
			return err
		}
	}
	if metadata, present := record["metadata"]; present {
		if _, ok := metadata.(map[string]any); !ok {
			return errors.New("custom snippet contains incompatible process node data")
		}
	}
	return nil
}

func marshalProcessSnippetEnvelope(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'})
	// encoding/json always escapes the two JavaScript line separators for
	// JSONP safety. JSON.stringify emits them as UTF-8, so unescape only real
	// JSON escapes (an even number of preceding backslashes), not a literal
	// caller string such as "\\u2028".
	result := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); {
		if index+6 <= len(encoded) && encoded[index] == '\\' &&
			(string(encoded[index:index+6]) == `\u2028` || string(encoded[index:index+6]) == `\u2029`) {
			preceding := 0
			for cursor := index - 1; cursor >= 0 && encoded[cursor] == '\\'; cursor-- {
				preceding++
			}
			if preceding%2 == 0 {
				if encoded[index+5] == '8' {
					result = append(result, "\u2028"...)
				} else {
					result = append(result, "\u2029"...)
				}
				index += 6
				continue
			}
		}
		result = append(result, encoded[index])
		index++
	}
	return result, nil
}

func validSnippetNodeID(value string) bool {
	return value != "" && len(value) <= processSnippetMaxNodeIDBytes && processSnippetNodeID.MatchString(value)
}

func validSnippetOutcome(value string) bool {
	units := 0
	for _, r := range value {
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
		if r <= 0x1f || r == 0x7f {
			return false
		}
	}
	return units > 0 && units <= processSnippetMaxOutcomeUnits
}

func snippetCompoundTask(node model.Node) bool {
	return node.Type == model.NodeTypeTask && (node.Plan != nil || len(node.Checks) > 0 || node.Review != nil)
}

func snippetSanctionedRetry(edge model.Edge, nodes map[string]model.Node, bySource map[string]map[string]string) bool {
	decision, ok := nodes[edge.From]
	if !ok || edge.Outcome != "retry" || decision.Type != model.NodeTypeDecision ||
		decision.Performer == nil || decision.Performer.Kind != model.PerformerHuman {
		return false
	}
	target, ok := nodes[edge.To]
	if !ok || !snippetCompoundTask(target) {
		return false
	}
	for _, outcome := range []string{"fail", "failed", "failure", "error"} {
		if bySource[edge.To][outcome] == edge.From {
			return true
		}
	}
	return false
}

func validateSnippetTopology(nodes map[string]model.Node, edges []model.Edge) error {
	bySource := make(map[string]map[string]string, len(nodes))
	for id := range nodes {
		bySource[id] = map[string]string{}
	}
	for _, edge := range edges {
		bySource[edge.From][edge.Outcome] = edge.To
	}
	adjacency := make(map[string][]string, len(nodes))
	indegree := make(map[string]int, len(nodes))
	for id := range nodes {
		indegree[id] = 0
	}
	for _, edge := range edges {
		if snippetSanctionedRetry(edge, nodes, bySource) {
			continue
		}
		adjacency[edge.From] = append(adjacency[edge.From], edge.To)
		indegree[edge.To]++
	}
	ready := make([]string, 0, len(nodes))
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	visited := 0
	for len(ready) > 0 {
		id := ready[len(ready)-1]
		ready = ready[:len(ready)-1]
		visited++
		for _, target := range adjacency[id] {
			indegree[target]--
			if indegree[target] == 0 {
				ready = append(ready, target)
			}
		}
	}
	if visited != len(nodes) {
		return errors.New("custom snippet contains an unsupported process graph cycle")
	}
	return nil
}

// canonicalizeProcessSnippetEnvelope is the server persistence boundary for
// the v1 JS selection envelope. It rejects unknown edit-wire fields, validates
// the same shape/topology/resource contract, sorts identities stably, and
// marshals typed model.Node values so caller-owned bytes are never stored.
func canonicalizeProcessSnippetEnvelope(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("custom snippet selection is required")
	}
	if len(raw)+len(processSnippetEnvelopePrefix) > db.MaxProcessSnippetEnvelopeBytes {
		return nil, errors.New("custom snippet exceeds the 256 KiB editor limit")
	}
	var envelope processSnippetEnvelope
	if err := strictJSON(raw, &envelope); err != nil {
		return nil, errors.New("custom snippet has an invalid selection envelope")
	}
	version, versionOK := snippetSafeInteger(envelope.Version)
	if envelope.Kind != processSnippetEnvelopeKind || !versionOK || version != processSnippetEnvelopeVersion {
		return nil, errors.New("custom snippet uses an unsupported selection format version")
	}
	if len(envelope.Nodes) == 0 {
		return nil, errors.New("custom snippet selection does not contain any nodes")
	}
	if envelope.Edges == nil {
		return nil, errors.New("custom snippet has an invalid selection envelope")
	}
	if len(envelope.Nodes) > model.MaxNormalizedNodes || len(envelope.Edges) > model.MaxNormalizedEdges {
		return nil, errors.New("custom snippet exceeds the process graph limits")
	}

	nodes := make(map[string]model.Node, len(envelope.Nodes))
	canonicalNodes := make([]canonicalProcessSnippetNode, 0, len(envelope.Nodes))
	for index := range envelope.Nodes {
		entry := &envelope.Nodes[index]
		if !validSnippetNodeID(entry.ID) {
			return nil, errors.New("custom snippet contains an invalid node record")
		}
		if _, exists := nodes[entry.ID]; exists {
			return nil, errors.New("custom snippet contains duplicate node IDs")
		}
		var rawNode map[string]json.RawMessage
		if err := json.Unmarshal(entry.Node, &rawNode); err != nil || rawNode == nil {
			return nil, errors.New("custom snippet contains incompatible process node data")
		}
		if _, hasTopology := rawNode["next"]; hasTopology {
			return nil, errors.New("custom snippet contains unsupported nested topology data")
		}
		var generic any
		decoder := json.NewDecoder(bytes.NewReader(entry.Node))
		decoder.UseNumber()
		if err := decoder.Decode(&generic); err != nil || validateSnippetJSONValue(generic) != nil || validateSnippetNodeWire(generic) != nil {
			return nil, errors.New("custom snippet contains incompatible process node data")
		}
		normalizedNode, err := json.Marshal(generic)
		if err != nil || strictJSON(normalizedNode, &entry.decoded) != nil {
			return nil, errors.New("custom snippet contains incompatible process node data")
		}
		var position struct {
			X *float64 `json:"x"`
			Y *float64 `json:"y"`
		}
		if err := strictJSON(entry.Position, &position); err != nil || position.X == nil || position.Y == nil {
			return nil, errors.New("custom snippet contains an invalid node position")
		}
		entry.decodedPosition = processSnippetPosition{X: *position.X, Y: *position.Y}
		switch entry.decoded.Type {
		case model.NodeTypeTask, model.NodeTypeDecision, model.NodeTypeParallel,
			model.NodeTypeWait, model.NodeTypeStart, model.NodeTypeEnd:
		default:
			return nil, errors.New("custom snippet contains an unsupported node type")
		}
		if math.IsNaN(entry.decodedPosition.X) || math.IsInf(entry.decodedPosition.X, 0) ||
			math.IsNaN(entry.decodedPosition.Y) || math.IsInf(entry.decodedPosition.Y, 0) ||
			math.Abs(entry.decodedPosition.X) > processSnippetMaxCoordinate ||
			math.Abs(entry.decodedPosition.Y) > processSnippetMaxCoordinate {
			return nil, errors.New("custom snippet contains an invalid node position")
		}
		nodes[entry.ID] = entry.decoded
		canonicalNodes = append(canonicalNodes, canonicalProcessSnippetNode{
			ID: entry.ID, Node: entry.decoded, Position: entry.decodedPosition,
		})
	}
	slices.SortFunc(canonicalNodes, func(a, b canonicalProcessSnippetNode) int {
		return strings.Compare(a.ID, b.ID)
	})

	edgeKeys := make(map[string]struct{}, len(envelope.Edges))
	for _, edge := range envelope.Edges {
		if !validSnippetNodeID(edge.From) || !validSnippetNodeID(edge.To) || !validSnippetOutcome(edge.Outcome) {
			return nil, errors.New("custom snippet contains an invalid edge record")
		}
		if _, ok := nodes[edge.From]; !ok {
			return nil, errors.New("custom snippet contains an edge with a missing endpoint")
		}
		if _, ok := nodes[edge.To]; !ok {
			return nil, errors.New("custom snippet contains an edge with a missing endpoint")
		}
		key := edge.From + "\x00" + edge.Outcome
		if _, duplicate := edgeKeys[key]; duplicate {
			return nil, errors.New("custom snippet contains duplicate edge outcomes")
		}
		edgeKeys[key] = struct{}{}
	}
	slices.SortFunc(envelope.Edges, func(a, b model.Edge) int {
		return strings.Compare(a.From+"\x00"+a.Outcome+"\x00"+a.To, b.From+"\x00"+b.Outcome+"\x00"+b.To)
	})
	if err := validateSnippetTopology(nodes, envelope.Edges); err != nil {
		return nil, err
	}
	canonical, err := marshalProcessSnippetEnvelope(canonicalProcessSnippetEnvelope{
		Kind: processSnippetEnvelopeKind, Version: processSnippetEnvelopeVersion,
		Nodes: canonicalNodes, Edges: envelope.Edges,
	})
	if err != nil || len(canonical)+len(processSnippetEnvelopePrefix) > db.MaxProcessSnippetEnvelopeBytes {
		return nil, errors.New("custom snippet exceeds the 256 KiB editor limit")
	}
	return canonical, nil
}

func processSnippetViewFromRow(snippet db.ProcessSnippet) processSnippetView {
	view := processSnippetView{
		ID: snippet.ID, Name: snippet.Name, Revision: snippet.Revision,
		Available: false, Unavailable: processSnippetUnavailableMessage,
	}
	if !snippet.CreatedAt.IsZero() {
		view.CreatedAt = snippet.CreatedAt.Format(timeRFC3339Nano)
	}
	if !snippet.UpdatedAt.IsZero() {
		view.UpdatedAt = snippet.UpdatedAt.Format(timeRFC3339Nano)
	}
	if snippet.Corrupt {
		return view
	}
	canonical, err := canonicalizeProcessSnippetEnvelope([]byte(snippet.EnvelopeJSON))
	if err != nil || string(canonical) != snippet.EnvelopeJSON {
		return view
	}
	view.Available = true
	view.Unavailable = ""
	view.Envelope = canonical
	return view
}

const timeRFC3339Nano = "2006-01-02T15:04:05.999999999Z07:00"

func writeProcessSnippetDBError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrProcessSnippetNotFound):
		writeError(w, http.StatusNotFound, "snippet_not_found", "custom snippet was not found")
	case errors.Is(err, db.ErrProcessSnippetConflict):
		writeError(w, http.StatusConflict, "snippet_conflict", "custom snippet changed; reload and try again")
	case errors.Is(err, db.ErrProcessSnippetNameExists):
		writeError(w, http.StatusConflict, "snippet_name_exists", "a custom snippet with that name already exists")
	case errors.Is(err, db.ErrProcessSnippetCountLimit):
		writeError(w, http.StatusUnprocessableEntity, "snippet_count_limit", "the custom snippet library has reached its 128 item limit")
	case errors.Is(err, db.ErrProcessSnippetByteLimit):
		writeError(w, http.StatusUnprocessableEntity, "snippet_byte_limit", "the custom snippet library has reached its 4 MiB payload limit")
	default:
		writeError(w, http.StatusInternalServerError, "process_snippets", "custom snippet storage failed")
	}
}

func handleProcessSnippetList(w http.ResponseWriter, _ *http.Request) {
	library, err := db.ListProcessSnippets()
	if err != nil {
		writeProcessSnippetDBError(w, err)
		return
	}
	views := make([]processSnippetView, 0, len(library.Snippets))
	for _, snippet := range library.Snippets {
		views = append(views, processSnippetViewFromRow(snippet))
	}
	writeJSON(w, http.StatusOK, map[string]any{"generation": library.Generation, "snippets": views})
}

func handleProcessSnippetCreate(w http.ResponseWriter, r *http.Request) {
	body, err := decodeProcessSnippetBody(w, r)
	if err != nil || body.Revision != 0 {
		if err == nil {
			err = errors.New("revision is not accepted when creating a custom snippet")
		}
		writeError(w, http.StatusBadRequest, "snippet_request", err.Error())
		return
	}
	name, nameKey, err := normalizeProcessSnippetName(body.Name)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "snippet_name", err.Error())
		return
	}
	canonical, err := canonicalizeProcessSnippetEnvelope(body.Envelope)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "snippet_envelope", err.Error())
		return
	}
	snippet, generation, err := db.CreateProcessSnippet(name, nameKey, string(canonical))
	if err != nil {
		writeProcessSnippetDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"generation": generation, "snippet": processSnippetViewFromRow(snippet)})
}

func validProcessSnippetPathID(id string) bool {
	if !strings.HasPrefix(id, db.ProcessSnippetIDPrefix) || len(id) != len(db.ProcessSnippetIDPrefix)+32 {
		return false
	}
	for _, r := range id[len(db.ProcessSnippetIDPrefix):] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func handleProcessSnippetRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validProcessSnippetPathID(id) {
		writeError(w, http.StatusNotFound, "snippet_not_found", "custom snippet was not found")
		return
	}
	body, err := decodeProcessSnippetBody(w, r)
	if err != nil || len(body.Envelope) != 0 || body.Revision <= 0 {
		writeError(w, http.StatusBadRequest, "snippet_request", "name and a positive revision are required")
		return
	}
	name, nameKey, err := normalizeProcessSnippetName(body.Name)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "snippet_name", err.Error())
		return
	}
	snippet, generation, err := db.RenameProcessSnippet(id, name, nameKey, body.Revision)
	if err != nil {
		writeProcessSnippetDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"generation": generation, "snippet": processSnippetViewFromRow(snippet)})
}

func handleProcessSnippetDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validProcessSnippetPathID(id) {
		writeError(w, http.StatusNotFound, "snippet_not_found", "custom snippet was not found")
		return
	}
	body, err := decodeProcessSnippetBody(w, r)
	if err != nil || body.Revision <= 0 || body.Name != "" || len(body.Envelope) != 0 {
		writeError(w, http.StatusBadRequest, "snippet_request", "a positive revision is required")
		return
	}
	generation, err := db.DeleteProcessSnippet(id, body.Revision)
	if err != nil {
		writeProcessSnippetDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "generation": generation})
}
