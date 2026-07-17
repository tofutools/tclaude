package model

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// FuzzCanonicalYAMLRoundTrip pins the editor's lossless Go round-trip
// contract across every parseable template shape the fuzzer discovers.
// Comments are intentionally canonicalized away; modeled content, including
// layout and all name/description/doc fields, must survive exactly while the
// semantic identity remains stable.
func FuzzCanonicalYAMLRoundTrip(f *testing.F) {
	f.Add([]byte(validTemplateYAML))
	f.Add([]byte(canonicalYAMLDuplicateScalarKeyRegression))
	f.Add([]byte(`
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: documented
name: Documented template
description: Template description
doc: |
  Template documentation.
params:
  issue:
    type: string
    name: Issue
    description: Issue description
    doc: Issue documentation
start: begin
nodes:
  begin:
    type: start
    name: Begin
    description: Start description
    doc: Start documentation
    next: { pass: done }
  done:
    type: end
    name: Done
    description: End description
    doc: End documentation
    result: success
layout:
  nodes:
    begin: { x: 10.5, y: -4 }
    done: { x: 220, y: 30 }
`))
	f.Fuzz(func(t *testing.T, source []byte) {
		parsed, err := Parse(source)
		if err != nil || parsed.Template == nil {
			return
		}
		canonical, err := CanonicalYAML(parsed.Template)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		roundTrip, err := Parse(canonical)
		if err != nil {
			t.Fatalf("parse canonical output: %v\n%s", err, canonical)
		}
		if parsed.SemanticHash != roundTrip.SemanticHash {
			t.Fatalf("semantic hash changed: %s != %s", parsed.SemanticHash, roundTrip.SemanticHash)
		}
		if !reflect.DeepEqual(parsed.Template, roundTrip.Template) {
			t.Fatalf("modeled template changed\nbefore: %#v\nafter:  %#v", parsed.Template, roundTrip.Template)
		}
	})
}

const canonicalYAMLDuplicateScalarKeyRegression = `0: 0
0: 000
00: 0000000000
0000: 0000000000000000000
00000000000: 000000000000000000000
doc: |

 0
`

func TestCanonicalYAMLRoundTripDuplicateScalarKeys(t *testing.T) {
	parsed, err := Parse([]byte(canonicalYAMLDuplicateScalarKeyRegression))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Diagnostics) == 0 || parsed.Diagnostics[0].Code != "duplicate_key" ||
		parsed.Diagnostics[0].Path != "0" || parsed.Diagnostics[0].Line != 2 || parsed.Diagnostics[0].Col != 1 {
		t.Fatalf("first diagnostic = %#v, want duplicate_key at 0 (2:1)", parsed.Diagnostics)
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Parse(canonical)
	if err != nil {
		t.Fatalf("parse canonical output: %v\n%s", err, canonical)
	}
	if !reflect.DeepEqual(parsed.Template, roundTrip.Template) {
		t.Fatalf("modeled template changed\nbefore: %#v\nafter:  %#v\n%s", parsed.Template, roundTrip.Template, canonical)
	}
	if parsed.SemanticHash != roundTrip.SemanticHash {
		t.Fatalf("semantic hash changed: %s != %s\n%s", parsed.SemanticHash, roundTrip.SemanticHash, canonical)
	}
}

func TestFreeformDecodedScalarKeyCollisionsAreDeterministic(t *testing.T) {
	const source = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: freeform-key-collisions
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    metadata:
      nested:
        0: first
        "0": second
        00: last
      ordered:
        1: first
        01: last
      sequence:
        - false: first
          "false": last
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	want := []Diagnostic{
		{Severity: SeverityError, Code: "duplicate_key", Path: "nodes.a.metadata.nested.0", Message: "duplicate YAML mapping key", Line: 12, Col: 9},
		{Severity: SeverityError, Code: "duplicate_key", Path: "nodes.a.metadata.nested.0", Message: "duplicate YAML mapping key", Line: 13, Col: 9},
		{Severity: SeverityError, Code: "duplicate_key", Path: "nodes.a.metadata.ordered.1", Message: "duplicate YAML mapping key", Line: 16, Col: 9},
		{Severity: SeverityError, Code: "duplicate_key", Path: "nodes.a.metadata.sequence[0].false", Message: "duplicate YAML mapping key", Line: 19, Col: 11},
	}
	if len(parsed.Diagnostics) < len(want) || !reflect.DeepEqual(parsed.Diagnostics[:len(want)], Diagnostics(want)) {
		t.Fatalf("duplicate diagnostic prefix\n got: %#v\nwant: %#v", parsed.Diagnostics, want)
	}
	metadata := parsed.Template.Nodes["a"].Metadata
	if got := metadata["nested"]; !reflect.DeepEqual(got, map[string]any{"0": "last"}) {
		t.Fatalf("nested last-wins value = %#v", got)
	}
	if got := metadata["ordered"]; !reflect.DeepEqual(got, map[string]any{"1": "last"}) {
		t.Fatalf("ordered last-wins value = %#v", got)
	}
	if got := metadata["sequence"]; !reflect.DeepEqual(got, []any{map[string]any{"false": "last"}}) {
		t.Fatalf("sequence last-wins value = %#v", got)
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	for _, quotedKey := range []string{`"0": last`, `"1": last`, `"false": last`} {
		if !strings.Contains(string(canonical), quotedKey) {
			t.Fatalf("canonical output does not preserve ambiguous string key %q:\n%s", quotedKey, canonical)
		}
	}
	roundTrip, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Template, roundTrip.Template) {
		t.Fatalf("modeled template changed\nbefore: %#v\nafter:  %#v\n%s", parsed.Template, roundTrip.Template, canonical)
	}
	if parsed.SemanticHash != roundTrip.SemanticHash {
		t.Fatalf("semantic hash changed: %s != %s\n%s", parsed.SemanticHash, roundTrip.SemanticHash, canonical)
	}
}

func TestParamDefaultDecodedScalarKeyCollisionLastWins(t *testing.T) {
	const source = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: param-default-key-collision
params:
  settings:
    type: object
    default:
      2: first
      02: last
start: done
nodes:
  done: { type: end }
`
	parsed, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Diagnostics) == 0 || parsed.Diagnostics[0].Code != "duplicate_key" ||
		parsed.Diagnostics[0].Path != "params.settings.default.2" || parsed.Diagnostics[0].Line != 9 {
		t.Fatalf("first diagnostic = %#v, want duplicate_key at params.settings.default.2 line 9", parsed.Diagnostics)
	}
	if got := parsed.Template.Params["settings"].Default; !reflect.DeepEqual(got, map[string]any{"2": "last"}) {
		t.Fatalf("default last-wins value = %#v", got)
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"2": last`) {
		t.Fatalf("canonical output did not quote normalized string key:\n%s", canonical)
	}
	roundTrip, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Template, roundTrip.Template) || parsed.SemanticHash != roundTrip.SemanticHash {
		t.Fatalf("param default changed across canonical round trip\nbefore: %#v\nafter:  %#v\nhashes: %s != %s\n%s",
			parsed.Template, roundTrip.Template, parsed.SemanticHash, roundTrip.SemanticHash, canonical)
	}
}

func TestFreeformSignedZeroKeyCollisionLastWins(t *testing.T) {
	tests := []struct {
		name        string
		keys        string
		wantKey     string
		wantValue   string
		wantDupLine int
	}{
		{name: "positive then negative", keys: "      0.0: positive\n      -0.0: negative", wantKey: "-0", wantValue: "negative", wantDupLine: 9},
		{name: "negative then positive", keys: "      -0.0: negative\n      0.0: positive", wantKey: "0", wantValue: "positive", wantDupLine: 9},
	}
	for _, test := range tests {
		t.Run("param default "+test.name, func(t *testing.T) {
			source := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: signed-zero-key-collision
params:
  settings:
    type: object
    default:
` + test.keys + `
start: done
nodes:
  done: { type: end }
`
			parsed, err := Parse([]byte(source))
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.Diagnostics) == 0 || parsed.Diagnostics[0].Code != "duplicate_key" ||
				parsed.Diagnostics[0].Path != "params.settings.default."+test.wantKey || parsed.Diagnostics[0].Line != test.wantDupLine {
				t.Fatalf("first diagnostic = %#v, want signed-zero duplicate_key", parsed.Diagnostics)
			}
			want := map[string]any{test.wantKey: test.wantValue}
			if got := parsed.Template.Params["settings"].Default; !reflect.DeepEqual(got, want) {
				t.Fatalf("signed-zero last-wins value = %#v, want %#v", got, want)
			}
			canonical, err := CanonicalYAML(parsed.Template)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := Parse(canonical)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(parsed.Template, roundTrip.Template) || parsed.SemanticHash != roundTrip.SemanticHash {
				t.Fatalf("signed-zero key changed across canonical round trip\nbefore: %#v\nafter:  %#v\nhashes: %s != %s\n%s",
					parsed.Template, roundTrip.Template, parsed.SemanticHash, roundTrip.SemanticHash, canonical)
			}
		})
		t.Run("nested metadata "+test.name, func(t *testing.T) {
			source := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: signed-zero-key-collision
start: a
nodes:
  a:
    type: end
    metadata:
      nested:
` + strings.ReplaceAll(test.keys, "      ", "        ") + `
`
			parsed, err := Parse([]byte(source))
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.Diagnostics) == 0 || parsed.Diagnostics[0].Code != "duplicate_key" ||
				parsed.Diagnostics[0].Path != "nodes.a.metadata.nested."+test.wantKey {
				t.Fatalf("first diagnostic = %#v, want nested signed-zero duplicate_key", parsed.Diagnostics)
			}
			want := map[string]any{test.wantKey: test.wantValue}
			if got := parsed.Template.Nodes["a"].Metadata["nested"]; !reflect.DeepEqual(got, want) {
				t.Fatalf("nested signed-zero last-wins value = %#v, want %#v", got, want)
			}
			canonical, err := CanonicalYAML(parsed.Template)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := Parse(canonical)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(parsed.Template, roundTrip.Template) || parsed.SemanticHash != roundTrip.SemanticHash {
				t.Fatalf("nested signed-zero key changed across canonical round trip\nbefore: %#v\nafter:  %#v\nhashes: %s != %s\n%s",
					parsed.Template, roundTrip.Template, parsed.SemanticHash, roundTrip.SemanticHash, canonical)
			}
		})
	}
}

func TestFreeformMixedSignedZeroKeyIdentity(t *testing.T) {
	tests := []struct {
		name           string
		keys           string
		want           map[string]any
		wantDuplicates int
	}{
		{
			name: "integer then negative float stay distinct",
			keys: "0: integer\n-0.0: negative",
			want: map[string]any{"0": "integer", "-0": "negative"},
		},
		{
			name: "negative float then integer stay distinct",
			keys: "-0.0: negative\n0: integer",
			want: map[string]any{"0": "integer", "-0": "negative"},
		},
		{
			name:           "string then negative float collide",
			keys:           `"-0": string` + "\n-0.0: negative",
			want:           map[string]any{"-0": "negative"},
			wantDuplicates: 2,
		},
		{
			name:           "negative float then string collide",
			keys:           `-0.0: negative` + "\n\"-0\": string",
			want:           map[string]any{"-0": "string"},
			wantDuplicates: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paramKeys := strings.ReplaceAll(test.keys, "\n", "\n      ")
			metadataKeys := strings.ReplaceAll(test.keys, "\n", "\n        ")
			source := `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: mixed-signed-zero-key-identity
params:
  settings:
    type: object
    default:
      ` + paramKeys + `
start: a
nodes:
  a:
    type: end
    metadata:
      nested:
        ` + metadataKeys + `
`
			parsed, err := Parse([]byte(source))
			if err != nil {
				t.Fatal(err)
			}
			duplicates := 0
			for _, diagnostic := range parsed.Diagnostics {
				if diagnostic.Code == "duplicate_key" {
					duplicates++
				}
			}
			if duplicates != test.wantDuplicates {
				t.Fatalf("duplicate diagnostics = %d, want %d: %#v", duplicates, test.wantDuplicates, parsed.Diagnostics)
			}
			if got := parsed.Template.Params["settings"].Default; !reflect.DeepEqual(got, test.want) {
				t.Fatalf("param default = %#v, want %#v", got, test.want)
			}
			if got := parsed.Template.Nodes["a"].Metadata["nested"]; !reflect.DeepEqual(got, test.want) {
				t.Fatalf("nested metadata = %#v, want %#v", got, test.want)
			}
			canonical, err := CanonicalYAML(parsed.Template)
			if err != nil {
				t.Fatal(err)
			}
			roundTrip, err := Parse(canonical)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(parsed.Template, roundTrip.Template) || parsed.SemanticHash != roundTrip.SemanticHash {
				t.Fatalf("mixed signed-zero keys changed across canonical round trip\nbefore: %#v\nafter:  %#v\nhashes: %s != %s\n%s",
					parsed.Template, roundTrip.Template, parsed.SemanticHash, roundTrip.SemanticHash, canonical)
			}
		})
	}
}

func TestStringKeyedScalarKeysPreserveLexicalIdentity(t *testing.T) {
	const source = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: lexical-string-keys
start: "0"
nodes:
  0: { type: end }
  00: { type: end }
`
	parsed, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range parsed.Diagnostics {
		if diagnostic.Code == "duplicate_key" {
			t.Fatalf("lexically distinct keys in a string-keyed graph map collided: %#v", parsed.Diagnostics)
		}
	}
	if _, ok := parsed.Template.Nodes["0"]; !ok {
		t.Fatalf("plain numeric-looking node key 0 was not preserved: %#v", parsed.Template.Nodes)
	}
	if _, ok := parsed.Template.Nodes["00"]; !ok {
		t.Fatalf("plain numeric-looking node key 00 was not preserved: %#v", parsed.Template.Nodes)
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	for _, quotedKey := range []string{`"0":`, `"00":`} {
		if !strings.Contains(string(canonical), quotedKey) {
			t.Fatalf("canonical output did not quote graph string key %q:\n%s", quotedKey, canonical)
		}
	}
	roundTrip, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Template, roundTrip.Template) || parsed.SemanticHash != roundTrip.SemanticHash {
		t.Fatalf("string-keyed graph map changed across canonical round trip\nbefore: %#v\nafter:  %#v\nhashes: %s != %s\n%s",
			parsed.Template, roundTrip.Template, parsed.SemanticHash, roundTrip.SemanticHash, canonical)
	}
}

func TestAliasedMappingUsesOccurrenceKeyContext(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{
			name: "freeform anchor before typed alias",
			source: `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: aliased-key-context
params:
  settings:
    type: object
    default: &shared
      0: { type: end }
      00: { type: end }
start: "0"
nodes: *shared
`,
		},
		{
			name: "typed anchor before freeform alias",
			source: `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: aliased-key-context
start: "0"
nodes: &shared
  0: { type: end }
  00: { type: end }
params:
  settings:
    type: object
    default: *shared
`,
		},
	}
	var baseline *ParsedTemplate
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := Parse([]byte(test.source))
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.Diagnostics) == 0 || parsed.Diagnostics[0].Code != "duplicate_key" ||
				parsed.Diagnostics[0].Path != "params.settings.default.0" {
				t.Fatalf("first diagnostic = %#v, want freeform duplicate_key", parsed.Diagnostics)
			}
			if _, ok := parsed.Template.Nodes["0"]; !ok {
				t.Fatalf("typed alias lost lexical key 0: %#v", parsed.Template.Nodes)
			}
			if _, ok := parsed.Template.Nodes["00"]; !ok {
				t.Fatalf("typed alias lost lexical key 00: %#v", parsed.Template.Nodes)
			}
			wantDefault := map[string]any{"0": map[string]any{"type": "end"}}
			if got := parsed.Template.Params["settings"].Default; !reflect.DeepEqual(got, wantDefault) {
				t.Fatalf("freeform alias did not prune last-wins: %#v", got)
			}
			if baseline == nil {
				baseline = parsed
				return
			}
			if !reflect.DeepEqual(baseline.Template, parsed.Template) || baseline.SemanticHash != parsed.SemanticHash {
				t.Fatalf("alias declaration order changed modeled identity\nbefore: %#v\nafter:  %#v\nhashes: %s != %s",
					baseline.Template, parsed.Template, baseline.SemanticHash, parsed.SemanticHash)
			}
		})
	}
}

func TestCanonicalYAMLPreservesLeadingNewlineMapKey(t *testing.T) {
	const source = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: leading-newline-key
start: a
nodes:
  a:
    type: end
    metadata:
      "\n0": leading
      "0": plain
`
	parsed, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Template, roundTrip.Template) || parsed.SemanticHash != roundTrip.SemanticHash {
		t.Fatalf("leading-newline map key changed across canonical round trip\nbefore: %#v\nafter:  %#v\nhashes: %s != %s\n%s",
			parsed.Template, roundTrip.Template, parsed.SemanticHash, roundTrip.SemanticHash, canonical)
	}
}

func TestCanonicalYAMLPreservesCRLFModeledStrings(t *testing.T) {
	const source = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: crlf-modeled-strings
doc: "first\r\nsecond"
start: a
nodes:
  a:
    type: end
    metadata:
      crlfValue: "first\r\nsecond"
      "key\r\nline": keyed
`
	parsed, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Template, roundTrip.Template) || parsed.SemanticHash != roundTrip.SemanticHash {
		t.Fatalf("CRLF modeled string changed across canonical round trip\nbefore: %#v\nafter:  %#v\nhashes: %s != %s\n%s",
			parsed.Template, roundTrip.Template, parsed.SemanticHash, roundTrip.SemanticHash, canonical)
	}
}

func TestRestoreCanonicalYAMLStringsQuotesChangedScalar(t *testing.T) {
	node := &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   "!!str",
		Value: "first\nsecond",
		Style: yaml.LiteralStyle,
	}
	modeled := "first\r\nsecond"
	if err := restoreCanonicalYAMLStrings(node, reflect.ValueOf(modeled)); err != nil {
		t.Fatal(err)
	}
	if node.Value != modeled || node.Style != yaml.DoubleQuotedStyle {
		t.Fatalf("restored node = value %q style %v, want exact CRLF value in double quotes", node.Value, node.Style)
	}
}

func TestCanonicalYAMLPreservesBinaryModeledStrings(t *testing.T) {
	const source = `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: binary-modeled-strings
doc: !!binary /w==
start: a
nodes:
  a:
    type: end
    metadata:
      binaryValue: !!binary /g==
      ? !!binary /w==
      : ffKey
      ? !!binary /g==
      : feKey
      "\uFFFD": replacementKey
`
	parsed, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Template, roundTrip.Template) || parsed.SemanticHash != roundTrip.SemanticHash {
		t.Fatalf("binary modeled string changed across canonical round trip\nbefore: %#v\nafter:  %#v\nhashes: %s != %s\n%s",
			parsed.Template, roundTrip.Template, parsed.SemanticHash, roundTrip.SemanticHash, canonical)
	}
}

const validTemplateYAML = `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: code-change-with-review
name: Code change with review
description: Implement an issue, check it, and review it.
params:
  issue:
    type: string
start: implement
nodes:
  implement:
    type: task
    performer:
      kind: agent
      profile: dev
      prompt: "Implement {{ params.issue }}"
      contact:
        cadence: 5m
        budget: 3
        escalationTarget: human:operator
    checks:
      - id: unit-tests
        performer:
          kind: program
          run: go test ./...
    review:
      id: review
      performer:
        kind: agent
        profile: reviewer
        prompt: Review the implementation.
    retry:
      maxAttempts: 3
      backoff: 10m
      onFail: feedback-same-session
    next:
      pass: done
      fail: escalate
  escalate:
    type: decision
    performer:
      kind: human
      ask: "Retries exhausted. Continue?"
    next:
      retry: implement
      cancel: canceled
  done:
    type: end
    result: success
  canceled:
    type: end
    result: canceled
layout:
  nodes:
    implement: { x: 120, y: 80 }
    escalate: { x: 320, y: 200 }
`

func TestParseValidTemplate(t *testing.T) {
	parsed, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	if len(parsed.Diagnostics.Warnings()) != 0 {
		t.Fatalf("unexpected warnings: %#v", parsed.Diagnostics.Warnings())
	}
	if parsed.SemanticHash == "" {
		t.Fatal("semantic hash is empty")
	}
	if parsed.SourceHash == "" {
		t.Fatal("source hash is empty")
	}
	wantRef := "code-change-with-review@sha256:" + parsed.SemanticHash
	if parsed.Ref != wantRef {
		t.Fatalf("ref = %q, want %q", parsed.Ref, wantRef)
	}

	assertEdge(t, parsed.Edges, Edge{From: "", Outcome: "start", To: "implement"})
	assertEdge(t, parsed.Edges, Edge{From: "implement", Outcome: "pass", To: "done"})
	assertEdge(t, parsed.Edges, Edge{From: "implement", Outcome: "fail", To: "escalate"})
	contact := parsed.Template.Nodes["implement"].Performer.Contact
	if contact == nil || contact.Cadence != "5m" || contact.Budget != 3 || contact.EscalationTarget != "human:operator" {
		t.Fatalf("contact = %#v", contact)
	}
}

func TestParseAllowsPoisonEscalationRetryCycle(t *testing.T) {
	parsed, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("poison escalation retry diagnostics: %#v", parsed.Diagnostics.Errors())
	}
}

func TestParseRejectsUnsupportedPoisonEscalationChoices(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
	}{
		{name: "extra choice", data: strings.Replace(validTemplateYAML, "      cancel: canceled", "      cancel: canceled\n      ship-anyway: done", 1)},
		{name: "retry targets other node", data: strings.Replace(validTemplateYAML, "      retry: implement", "      retry: done", 1)},
		{name: "cancel targets successful end", data: strings.Replace(validTemplateYAML, "      cancel: canceled", "      cancel: done", 1)},
		{name: "non-reserved incoming edge", data: strings.Replace(validTemplateYAML, "  done:\n", "  intruder:\n    type: task\n    performer: { kind: agent, prompt: intrude }\n    next: { pass: done, fail: escalate }\n  done:\n", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := Parse([]byte(test.data))
			if err != nil {
				t.Fatal(err)
			}
			if !hasDiagnostic(parsed.Diagnostics, SeverityError, "invalid_poison_escalation") {
				t.Fatalf("diagnostics = %#v", parsed.Diagnostics)
			}
		})
	}
}

func TestCanonicalYAMLRoundTripPreservesSemantics(t *testing.T) {
	parsed, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	data, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Diagnostics.HasErrors() {
		t.Fatalf("round-trip errors: %#v", roundTrip.Diagnostics.Errors())
	}
	if roundTrip.SemanticHash != parsed.SemanticHash {
		t.Fatalf("semantic hash changed after round trip: %s != %s", roundTrip.SemanticHash, parsed.SemanticHash)
	}
	before, err := CanonicalSemanticJSON(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	after, err := CanonicalSemanticJSON(roundTrip.Template)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("semantic JSON changed\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestCanonicalYAMLIsIdempotent(t *testing.T) {
	parsed, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	first, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	secondParsed, err := Parse(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CanonicalYAML(secondParsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("canonical YAML is not byte-stable\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestSemanticHashIgnoresLayoutAndComments(t *testing.T) {
	left, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}

	rightYAML := strings.ReplaceAll(validTemplateYAML, "implement: { x: 120, y: 80 }", "implement: { x: 999, y: 80 }")
	rightYAML = "# editor-only comment\n" + rightYAML
	right, err := Parse([]byte(rightYAML))
	if err != nil {
		t.Fatal(err)
	}

	if left.SemanticHash != right.SemanticHash {
		t.Fatalf("semantic hash should ignore layout/comments: %s != %s", left.SemanticHash, right.SemanticHash)
	}
	if left.SourceHash == right.SourceHash {
		t.Fatal("source hash should include raw bytes and differ")
	}
}

func TestSemanticHashChangesForSemanticChanges(t *testing.T) {
	left, err := Parse([]byte(validTemplateYAML))
	if err != nil {
		t.Fatal(err)
	}
	rightYAML := strings.ReplaceAll(validTemplateYAML, "profile: dev", "profile: senior-dev")
	right, err := Parse([]byte(rightYAML))
	if err != nil {
		t.Fatal(err)
	}
	if left.SemanticHash == right.SemanticHash {
		t.Fatal("semantic hash should change when performer profile changes")
	}
}

func TestScalarNextRoundTrip(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: scalar-next
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	assertEdge(t, parsed.Edges, Edge{From: "a", Outcome: DefaultOutcome, To: "done"})
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), "next: done") {
		t.Fatalf("canonical scalar next did not stay scalar:\n%s", canonical)
	}
}

func TestFreeformRoundTrip(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: freeform
params:
  settings:
    type: object
    default:
      enabled: true
      threshold: 1.5
      tags: [a, b]
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    metadata:
      nested:
        count: 2
        none: null
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	canonical, err := CanonicalYAML(parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.SemanticHash != parsed.SemanticHash {
		t.Fatalf("semantic hash changed after freeform round trip: %s != %s", roundTrip.SemanticHash, parsed.SemanticHash)
	}
}

func TestDiagnosticsOrderIsStable(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: stable-diagnostics
start: a
nodes:
  a:
    type: task
    performer:
      kind: agent
      prompt: "{{ params.zed }} {{ params.alpha }}"
      args: ["{{ params.mid }}"]
    next: done
  done: { type: end }
`
	var want []string
	for i := 0; i < 50; i++ {
		parsed, err := Parse([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, diag := range parsed.Diagnostics {
			if diag.Code == "undeclared_param_ref" {
				got = append(got, diag.Path+":"+diag.Message)
			}
		}
		if i == 0 {
			want = got
			continue
		}
		if !slices.Equal(want, got) {
			t.Fatalf("diagnostic order changed\nwant: %#v\ngot:  %#v", want, got)
		}
	}
}

func TestLayoutWarningsDoNotMakeTemplateInvalid(t *testing.T) {
	data := strings.ReplaceAll(validTemplateYAML, "escalate: { x: 320, y: 200 }", "removed-node: { x: 320, y: 200 }")
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	if !hasDiagnostic(parsed.Diagnostics, SeverityWarning, "stale_layout_node") {
		t.Fatalf("expected stale_layout_node warning, got %#v", parsed.Diagnostics)
	}
}

func TestInvalidTemplates(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		code string
	}{
		{
			name: "unknown edge target",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: unknown-target
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    next: missing
`,
			code: "unknown_target",
		},
		{
			name: "unreachable node",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: unreachable
start: a
nodes:
  a: { type: end }
  b: { type: end }
`,
			code: "unreachable_node",
		},
		{
			name: "graph cycle",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: cycle
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    next: b
  b:
    type: wait
    wait: { duration: 1m }
    next: a
`,
			code: "graph_cycle",
		},
		{
			name: "undeclared param",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: undeclared-param
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Use {{ params.issue }}" }
    next: done
  done: { type: end }
`,
			code: "undeclared_param_ref",
		},
		{
			name: "retry without budget",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: retry-without-budget
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    retry: { onFail: feedback-same-session }
    next: done
  done: { type: end }
`,
			code: "invalid_retry_budget",
		},
		{
			name: "duplicate node id",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: duplicate
start: a
nodes:
  a: { type: end }
  a: { type: task, performer: { kind: agent, prompt: "Do it" }, next: done }
  done: { type: end }
`,
			code: "duplicate_key",
		},
		{
			name: "unknown field",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: typo
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    nxet: done
`,
			code: "unknown_field",
		},
		{
			name: "non-string freeform key",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: non-string-freeform-key
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    metadata:
      nested:
        1: one
    next: done
  done: { type: end }
`,
			code: "non_string_freeform_key",
		},
		{
			name: "invalid template id",
			yaml: strings.Replace(validTemplateYAML, "id: code-change-with-review", "id: bad id", 1),
			code: "invalid_id",
		},
		{
			name: "invalid node id",
			yaml: strings.Replace(validTemplateYAML, "  implement:", "  \"\":", 1),
			code: "invalid_id",
		},
		{
			name: "invalid end result",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: invalid-end-result
start: done
nodes:
  done:
    type: end
    result: failled
`,
			code: "invalid_end_result",
		},
		{
			name: "result on non-end node",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: non-end-result
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    result: failed
    next: done
  done: { type: end }
`,
			code: "result_on_non_end_node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if !hasDiagnostic(parsed.Diagnostics, SeverityError, tt.code) {
				t.Fatalf("expected error code %q, got %#v", tt.code, parsed.Diagnostics)
			}
		})
	}
}

func TestHeaderErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		code string
	}{
		{
			name: "missing id",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
start: a
nodes:
  a: { type: end }
`,
			code: "missing_id",
		},
		{
			name: "unknown start",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: unknown-start
start: missing
nodes:
  a: { type: end }
`,
			code: "unknown_start",
		},
		{
			name: "invalid api version",
			yaml: `
apiVersion: v0
kind: ProcessTemplate
id: invalid-api
start: a
nodes:
  a: { type: end }
`,
			code: "invalid_api_version",
		},
		{
			name: "invalid kind",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: SomethingElse
id: invalid-kind
start: a
nodes:
  a: { type: end }
`,
			code: "invalid_kind",
		},
		{
			name: "missing nodes",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-nodes
start: a
`,
			code: "missing_nodes",
		},
		{
			name: "empty input",
			yaml: ``,
			code: "missing_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if !hasDiagnostic(parsed.Diagnostics, SeverityError, tt.code) {
				t.Fatalf("expected error code %q, got %#v", tt.code, parsed.Diagnostics)
			}
		})
	}
}

func TestNodeShapeErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		code string
	}{
		{
			name: "missing performer",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-performer
start: a
nodes:
  a: { type: task, next: done }
  done: { type: end }
`,
			code: "missing_performer",
		},
		{
			name: "missing next",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-next
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
`,
			code: "missing_next",
		},
		{
			name: "missing wait",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-wait
start: a
nodes:
  a: { type: wait, next: done }
  done: { type: end }
`,
			code: "missing_wait",
		},
		{
			name: "blank wait",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: blank-wait
start: a
nodes:
  a:
    type: wait
    wait: { duration: "   " }
    next: done
  done: { type: end }
`,
			code: "missing_wait",
		},
		{
			name: "end has next",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: end-has-next
start: a
nodes:
  a: { type: end, next: done }
  done: { type: end }
`,
			code: "end_has_next",
		},
		{
			name: "multiple start nodes",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: multiple-start
start: a
nodes:
  a: { type: start, next: done }
  b: { type: start, next: done }
  done: { type: end }
`,
			code: "multiple_start_nodes",
		},
		{
			name: "invalid performer kind",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: invalid-performer
start: a
nodes:
  a:
    type: task
    performer: { kind: robot, prompt: "Do it" }
    next: done
  done: { type: end }
`,
			code: "invalid_performer_kind",
		},
		{
			name: "blank agent prompt",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: blank-agent-prompt
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "   " }
    next: done
  done: { type: end }
`,
			code: "missing_prompt",
		},
		{
			name: "blank human ask",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: blank-human-ask
start: a
nodes:
  a:
    type: decision
    performer: { kind: human, ask: "   " }
    next: { approve: done }
  done: { type: end }
`,
			code: "missing_prompt",
		},
		{
			name: "blank program run",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: blank-program-run
start: a
nodes:
  a:
    type: task
    performer: { kind: program, run: "   " }
    next: done
  done: { type: end }
`,
			code: "missing_run",
		},
		{
			name: "missing step id",
			yaml: `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-step-id
start: a
nodes:
  a:
    type: task
    performer: { kind: agent, prompt: "Do it" }
    checks:
      - performer: { kind: program, run: go test ./... }
    next: done
  done: { type: end }
`,
			code: "missing_step_id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if !hasDiagnostic(parsed.Diagnostics, SeverityError, tt.code) {
				t.Fatalf("expected error code %q, got %#v", tt.code, parsed.Diagnostics)
			}
		})
	}
}

func TestProseParamRefsAreWarnings(t *testing.T) {
	data := `
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: prose-param-ref
description: "Use {{ params.example }} in prompts."
start: a
nodes:
  a:
    type: wait
    wait: { duration: 1m }
    next: done
  done: { type: end }
`
	parsed, err := Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %#v", parsed.Diagnostics.Errors())
	}
	if !hasDiagnostic(parsed.Diagnostics, SeverityWarning, "undeclared_param_ref") {
		t.Fatalf("expected prose undeclared_param_ref warning, got %#v", parsed.Diagnostics)
	}
}

func assertEdge(t *testing.T, edges []Edge, want Edge) {
	t.Helper()
	for _, edge := range edges {
		if edge == want {
			return
		}
	}
	t.Fatalf("missing edge %#v in %#v", want, edges)
}

func hasDiagnostic(diagnostics Diagnostics, severity Severity, code string) bool {
	for _, diag := range diagnostics {
		if diag.Severity == severity && diag.Code == code {
			return true
		}
	}
	return false
}
