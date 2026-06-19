package common

import "testing"

func TestShellJoin(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no special chars passes through unchanged",
			args: []string{"/usr/local/bin/tclaude", "session", "attach"},
			want: "/usr/local/bin/tclaude session attach",
		},
		{
			name: "bare command passes through unchanged",
			args: []string{"tclaude", "status-bar"},
			want: "tclaude status-bar",
		},
		{
			// Regression guard for JOH-32: a binary path containing spaces must
			// stay a single shell token instead of splitting on the space.
			name: "path with spaces is single-quoted",
			args: []string{"/Users/First Last/go/bin/tclaude", "session", "attach"},
			want: "'/Users/First Last/go/bin/tclaude' session attach",
		},
		{
			name: "embedded single quote is escaped",
			args: []string{"/home/o'brien/bin/tclaude", "status-bar"},
			want: `'/home/o'\''brien/bin/tclaude' status-bar`,
		},
		{
			name: "empty",
			args: nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shellJoin(tt.args); got != tt.want {
				t.Errorf("shellJoin(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
