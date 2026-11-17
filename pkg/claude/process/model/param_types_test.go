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
		{"{type: Boolean}", "invalid_param_type"},
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
