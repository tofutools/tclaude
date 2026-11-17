package model

import (
	"fmt"
	"testing"
)

// A param's type must be one the process understands, and its default a
// value of that type: `type: bogus` or `type: boolean` with `default: "no"`
// is refused when it is authored, not discovered when a run binds it.
func TestParamTypesAndDefaultsAreValidated(t *testing.T) {
	cases := []struct {
		param string
		code  string // "" when it is valid
	}{
		{"{type: string}", ""},
		{"{type: string, default: hello}", ""},
		{"{default: hello}", ""}, // a blank type is a string
		{"{type: number, default: 1.5}", ""},
		{"{type: number, default: 2}", ""},
		{"{type: integer, default: 3}", ""},
		{"{type: int, default: 3}", ""},
		{"{type: boolean, default: false}", ""},
		{"{type: bool, default: true}", ""},
		{"{type: object, default: {a: 1}}", ""},
		{"{type: array, default: [a, b]}", ""},
		{"{type: boolean}", ""},
		{"{type: bogus}", "invalid_param_type"},
		{"{type: Boolean, default: true}", ""}, // case-blind, as subprocess binding reads it
		{"{type: boolean, default: \"false\"}", "invalid_param_default"},
		{"{type: boolean, default: 1}", "invalid_param_default"},
		{"{type: number, default: \"1\"}", "invalid_param_default"},
		{"{type: integer, default: 1.5}", "invalid_param_default"},
		{"{type: string, default: 7}", "invalid_param_default"},
		{"{type: object, default: [a]}", "invalid_param_default"},
		{"{type: array, default: {a: 1}}", "invalid_param_default"},
	}
	for _, tc := range cases {
		t.Run(tc.param, func(t *testing.T) {
			data := fmt.Sprintf(`
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: typed
params:
  p: %s
start: done
nodes:
  done: {type: end}
`, tc.param)
			parsed, err := Parse([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, d := range parsed.Diagnostics.Errors() {
				codes = append(codes, d.Code)
			}
			if tc.code == "" && len(codes) > 0 {
				t.Fatalf("valid param refused: %v", parsed.Diagnostics.Errors())
			}
			if tc.code != "" && (len(codes) != 1 || codes[0] != tc.code) {
				t.Fatalf("want one %s, got %v", tc.code, parsed.Diagnostics.Errors())
			}
		})
	}
}

// A run's pinned snapshot was saved before types were checked: it must still
// load (ParseExactSource, and Validate under it), or the run could no longer
// be advanced after the daemon restarts.
func TestPinnedSourceWithAnUncheckedParamTypeStillLoads(t *testing.T) {
	data := []byte(`
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: old
params:
  p: {type: text, default: 5}
start: done
nodes:
  done: {type: end}
`)
	pinned, err := ParseExactSource(data)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Diagnostics.HasErrors() {
		t.Fatalf("a pinned snapshot is refused: %v", pinned.Diagnostics.Errors())
	}
	if diagnostics := Validate(pinned.Template, pinned.Edges); diagnostics.HasErrors() {
		t.Fatalf("Validate refuses a pinned snapshot: %v", diagnostics.Errors())
	}
	authored, err := ParseAuthoring(data)
	if err != nil {
		t.Fatal(err)
	}
	if !authored.Diagnostics.HasErrors() {
		t.Fatal("authoring accepts an unknown param type")
	}
}

func TestInlineSubprocessParamTypesAreChecked(t *testing.T) {
	parsed, err := Parse([]byte(`
apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: outer
start: sub
nodes:
  sub:
    type: subprocess
    subprocess:
      params:
        n: {type: integer, default: "5"}
      start: done
      nodes:
        done: {type: end}
    next: {next: done}
  done: {type: end}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range parsed.Diagnostics.Errors() {
		if d.Code == "invalid_param_default" {
			return
		}
	}
	t.Fatalf("want invalid_param_default in the body, got %v", parsed.Diagnostics.Errors())
}
