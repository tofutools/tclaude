package workflow

import (
	"reflect"
	"testing"
)

func TestScope_Resolve(t *testing.T) {
	s := Scope{
		"service_name": "billing",
		"plan":         map[string]any{"output": "the plan text", "steps": []any{"a", "b"}},
		"count":        float64(3),
	}
	cases := []struct {
		ref    string
		want   any
		wantOK bool
	}{
		{"service_name", "billing", true},
		{"plan", map[string]any{"output": "the plan text", "steps": []any{"a", "b"}}, true},
		{"plan.output", "the plan text", true},
		{"plan.steps", []any{"a", "b"}, true},
		{"count", float64(3), true},
		{"missing", nil, false},
		{"plan.nope", nil, false},
		{"service_name.x", nil, false}, // descend into a non-map
	}
	for _, c := range cases {
		got, ok := s.Resolve(c.ref)
		if ok != c.wantOK {
			t.Errorf("Resolve(%q) ok = %v, want %v", c.ref, ok, c.wantOK)
			continue
		}
		if ok && !reflect.DeepEqual(got, c.want) {
			t.Errorf("Resolve(%q) = %#v, want %#v", c.ref, got, c.want)
		}
	}
}

func TestScope_Interpolate_Scalars(t *testing.T) {
	s := Scope{"service_name": "billing", "env": "prod", "n": float64(5), "ok": true}
	out, missing := s.Interpolate("Deploy {{service_name}} to {{ env }} (n={{n}}, ok={{ok}})")
	if len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
	want := "Deploy billing to prod (n=5, ok=true)"
	if out != want {
		t.Errorf("Interpolate = %q, want %q", out, want)
	}
}

func TestScope_Interpolate_ListAndMapRenderAsJSON(t *testing.T) {
	s := Scope{
		"items": []any{"x", "y"},
		"cfg":   map[string]any{"region": "eu"},
	}
	out, missing := s.Interpolate("items={{items}} cfg={{cfg}}")
	if len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
	want := `items=["x","y"] cfg={"region":"eu"}`
	if out != want {
		t.Errorf("Interpolate = %q, want %q", out, want)
	}
}

func TestScope_Interpolate_MissingLeftVerbatimAndReported(t *testing.T) {
	s := Scope{"a": "1"}
	out, missing := s.Interpolate("{{a}} {{b}} {{c}} {{b}}")
	want := "1 {{b}} {{c}} {{b}}" // misses left in place
	if out != want {
		t.Errorf("Interpolate = %q, want %q", out, want)
	}
	wantMissing := []string{"b", "c"} // sorted + deduped
	if !reflect.DeepEqual(missing, wantMissing) {
		t.Errorf("missing = %v, want %v", missing, wantMissing)
	}
}

func TestScope_Interpolate_NestedRef(t *testing.T) {
	s := Scope{"plan": map[string]any{"output": "PLAN"}}
	out, missing := s.Interpolate("Implement per {{plan.output}}.")
	if len(missing) != 0 {
		t.Fatalf("unexpected missing: %v", missing)
	}
	if out != "Implement per PLAN." {
		t.Errorf("Interpolate = %q", out)
	}
}

func TestScope_Interpolate_NonRefBracesUntouched(t *testing.T) {
	s := Scope{}
	// A lone brace pair without a valid ref body is left alone.
	in := "func() { return {{}} }"
	out, _ := s.Interpolate(in)
	if out != in {
		t.Errorf("Interpolate mangled non-ref braces: %q", out)
	}
}

func TestScope_Interpolate_EmptyScopeNoRefs(t *testing.T) {
	s := Scope{}
	out, missing := s.Interpolate("plain text, no refs")
	if out != "plain text, no refs" || len(missing) != 0 {
		t.Errorf("Interpolate = %q, missing=%v", out, missing)
	}
}
