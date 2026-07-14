package agentd

import (
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestDeriveTaskLabel(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"linear with slug", "https://linear.app/acme/issue/JOH-353/add-task-links", "JOH-353"},
		{"linear no slug", "https://linear.app/acme/issue/ENG-12", "ENG-12"},
		{"linear lowercase id uppercased", "https://linear.app/acme/issue/eng-7/x", "ENG-7"},
		{"linear subdomain", "https://acme.linear.app/team/issue/AB-1/y", "AB-1"},
		{"github issue", "https://github.com/tofutools/tclaude/issues/42", "#42"},
		{"github pull", "https://github.com/tofutools/tclaude/pull/7", "#7"},
		{"github non-numeric tail is not a pr", "https://github.com/tofutools/tclaude/pulls", "github.com"},
		{"generic host", "https://jira.example.com/browse/PROJ-1", "jira.example.com"},
		{"www stripped", "https://www.example.com/x", "example.com"},
		{"empty", "", ""},
		{"unparseable", "://nonsense", ""},
		{"no host", "https:///path", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveTaskLabel(c.url); got != c.want {
				t.Errorf("deriveTaskLabel(%q) = %q, want %q", c.url, got, c.want)
			}
		})
	}
}

func TestEffectiveTaskLabel(t *testing.T) {
	// Explicit label wins over a derivable one.
	if got := effectiveTaskLabel(db.AgentTaskRef{URL: "https://github.com/o/r/issues/1", Label: "hotfix"}); got != "hotfix" {
		t.Errorf("explicit label should win, got %q", got)
	}
	// Blank explicit label falls back to derivation.
	if got := effectiveTaskLabel(db.AgentTaskRef{URL: "https://github.com/o/r/issues/1", Label: "  "}); got != "#1" {
		t.Errorf("blank label should derive, got %q", got)
	}
}

func TestTaskRefViewFor(t *testing.T) {
	if v := taskRefViewFor(db.AgentTaskRef{}); v != (taskRefView{}) {
		t.Errorf("empty ref should yield zero view, got %+v", v)
	}
	if v := taskRefViewFor(db.AgentTaskRef{URL: "   "}); v != (taskRefView{}) {
		t.Errorf("whitespace url should yield zero view, got %+v", v)
	}
	v := taskRefViewFor(db.AgentTaskRef{URL: "https://linear.app/a/issue/JOH-1/x"})
	if v.TaskURL != "https://linear.app/a/issue/JOH-1/x" || v.TaskLabel != "JOH-1" {
		t.Errorf("unexpected view %+v", v)
	}
	v = taskRefViewFor(db.AgentTaskRef{URL: "https://x.io/t", Label: "custom"})
	if v.TaskLabel != "custom" || v.TaskLabelOverride != "custom" {
		t.Errorf("explicit label should carry as display + editable override, got %+v", v)
	}
}

func TestValidateTaskRefURL(t *testing.T) {
	good := []string{
		"https://linear.app/a/issue/JOH-1",
		"http://example.com/x",
		"https://github.com/o/r/pull/9",
	}
	for _, u := range good {
		if err := validateTaskRefURL(u); err != nil {
			t.Errorf("validateTaskRefURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"",
		"   ",
		"javascript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"ftp://example.com/x",
		"mailto:x@y.com",
		"notaurl",
		"https://", // no host
		"//example.com/x",
		"https://x.io/" + strings.Repeat("a", maxTaskRefURLLen), // over the length cap
	}
	for _, u := range bad {
		if err := validateTaskRefURL(u); err == nil {
			t.Errorf("validateTaskRefURL(%q) = nil, want error", u)
		}
	}
}

func TestValidateTaskRefLabel(t *testing.T) {
	if err := validateTaskRefLabel(""); err != nil {
		t.Errorf("empty label should be allowed, got %v", err)
	}
	if err := validateTaskRefLabel(strings.Repeat("x", maxTaskRefLabelLen)); err != nil {
		t.Errorf("label at the cap should be allowed, got %v", err)
	}
	if err := validateTaskRefLabel(strings.Repeat("x", maxTaskRefLabelLen+1)); err == nil {
		t.Errorf("over-long label should be rejected")
	}
}
