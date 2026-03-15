package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTasks(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Task
	}{
		{
			name:  "empty",
			input: "",
			want:  nil,
		},
		{
			name:  "single task",
			input: "## Fix the bug\n\nPlease fix the null pointer bug in main.go\n",
			want: []Task{
				{Title: "Fix the bug", Prompt: "Please fix the null pointer bug in main.go"},
			},
		},
		{
			name: "multiple tasks",
			input: `## Add logging

Add structured logging to the HTTP server.

## Fix tests

Fix the failing integration tests in pkg/api/.
`,
			want: []Task{
				{Title: "Add logging", Prompt: "Add structured logging to the HTTP server."},
				{Title: "Fix tests", Prompt: "Fix the failing integration tests in pkg/api/."},
			},
		},
		{
			name: "multiline prompt",
			input: `## Refactor auth

Refactor the authentication module:
1. Extract the JWT validation into its own function
2. Add refresh token support
3. Update the tests
`,
			want: []Task{
				{
					Title:  "Refactor auth",
					Prompt: "Refactor the authentication module:\n1. Extract the JWT validation into its own function\n2. Add refresh token support\n3. Update the tests",
				},
			},
		},
		{
			name:  "task with no prompt",
			input: "## Empty task\n",
			want: []Task{
				{Title: "Empty task", Prompt: ""},
			},
		},
		{
			name:  "content before first header ignored",
			input: "Some preamble\n\n## Real task\n\nDo something\n",
			want: []Task{
				{Title: "Real task", Prompt: "Do something"},
			},
		},
		{
			name:  "plan mode task",
			input: "## [plan] Design new API\n\nDesign the REST API for the new service.\n",
			want: []Task{
				{Title: "Design new API", Prompt: "Design the REST API for the new service.", PlanMode: true},
			},
		},
		{
			name: "mixed plan and normal tasks",
			input: `## [plan] Architect the system

Design the overall architecture.

## Implement the feature

Build it out.
`,
			want: []Task{
				{Title: "Architect the system", Prompt: "Design the overall architecture.", PlanMode: true},
				{Title: "Implement the feature", Prompt: "Build it out."},
			},
		},
		{
			name:  "plan-auto task",
			input: "## [plan-auto] Design new API\n\nDesign and implement the REST API.\n",
			want: []Task{
				{Title: "Design new API", Prompt: "Design and implement the REST API.", PlanMode: true, PlanAutoAccept: true},
			},
		},
		{
			name: "mixed plan-auto plan and normal",
			input: `## [plan-auto] Design and build auth

Design auth then implement it.

## [plan] Review architecture

Review the architecture.

## Fix the bug

Fix it.
`,
			want: []Task{
				{Title: "Design and build auth", Prompt: "Design auth then implement it.", PlanMode: true, PlanAutoAccept: true},
				{Title: "Review architecture", Prompt: "Review the architecture.", PlanMode: true},
				{Title: "Fix the bug", Prompt: "Fix it."},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTasks(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d tasks, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i].Title != tt.want[i].Title {
					t.Errorf("task %d title = %q, want %q", i, got[i].Title, tt.want[i].Title)
				}
				if got[i].Prompt != tt.want[i].Prompt {
					t.Errorf("task %d prompt = %q, want %q", i, got[i].Prompt, tt.want[i].Prompt)
				}
				if got[i].PlanMode != tt.want[i].PlanMode {
					t.Errorf("task %d PlanMode = %v, want %v", i, got[i].PlanMode, tt.want[i].PlanMode)
				}
				if got[i].PlanAutoAccept != tt.want[i].PlanAutoAccept {
					t.Errorf("task %d PlanAutoAccept = %v, want %v", i, got[i].PlanAutoAccept, tt.want[i].PlanAutoAccept)
				}
			}
		})
	}
}

func TestWriteTodoMD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TODO.md")

	tasks := []Task{
		{Title: "First task", Prompt: "Do the first thing"},
		{Title: "Second task", Prompt: "Do the second thing"},
	}

	if err := WriteTodoMD(path, tasks); err != nil {
		t.Fatalf("WriteTodoMD failed: %v", err)
	}

	// Read back and parse
	parsed, err := ParseTodoMD(path)
	if err != nil {
		t.Fatalf("ParseTodoMD failed: %v", err)
	}

	if len(parsed) != 2 {
		t.Fatalf("got %d tasks, want 2", len(parsed))
	}
	if parsed[0].Title != "First task" {
		t.Errorf("task 0 title = %q, want %q", parsed[0].Title, "First task")
	}
	if parsed[1].Title != "Second task" {
		t.Errorf("task 1 title = %q, want %q", parsed[1].Title, "Second task")
	}
}

func TestWriteTodoMDEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TODO.md")

	if err := WriteTodoMD(path, nil); err != nil {
		t.Fatalf("WriteTodoMD failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(data) != "" {
		t.Errorf("expected empty file, got %q", string(data))
	}
}

func TestAppendDoneMD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "DONE.md")

	result := TaskResult{
		Title:     "Test task",
		Prompt:    "Do something",
		Status:    "completed",
		Commit:    "abc1234",
		PlanFile:  "~/.claude/plans/enchanted-meandering-mountain.md",
		Report:    "I did the thing.",
		Timestamp: time.Date(2026, 3, 7, 14, 30, 0, 0, time.UTC),
	}

	if err := AppendDoneMD(path, result); err != nil {
		t.Fatalf("AppendDoneMD failed: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "## Test task") {
		t.Error("missing task title")
	}
	if !strings.Contains(content, "**Status:** completed") {
		t.Error("missing status")
	}
	if !strings.Contains(content, "**Commit:** abc1234") {
		t.Error("missing commit hash")
	}
	if !strings.Contains(content, "**Plan:** ~/.claude/plans/enchanted-meandering-mountain.md") {
		t.Error("missing plan file")
	}
	if !strings.Contains(content, "Do something") {
		t.Error("missing prompt")
	}
	if !strings.Contains(content, "I did the thing.") {
		t.Error("missing report")
	}

	// Append a second result
	result2 := TaskResult{
		Title:     "Second task",
		Prompt:    "Do another thing",
		Status:    "failed",
		Error:     "something broke",
		Timestamp: time.Date(2026, 3, 7, 15, 0, 0, 0, time.UTC),
	}

	if err := AppendDoneMD(path, result2); err != nil {
		t.Fatalf("AppendDoneMD (second) failed: %v", err)
	}

	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile (second) failed: %v", err)
	}

	content = string(data)
	if !strings.Contains(content, "## Second task") {
		t.Error("missing second task title")
	}
	if !strings.Contains(content, "**Status:** failed") {
		t.Error("missing failed status")
	}
	if !strings.Contains(content, "**Error:** something broke") {
		t.Error("missing error message")
	}
}

func TestRunAddComma(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantTitle  string
		wantPrompt string
	}{
		{
			name:       "comma in prompt",
			args:       []string{"Fix stuff", "Fix items a, b, and c"},
			wantTitle:  "Fix stuff",
			wantPrompt: "Fix items a, b, and c",
		},
		{
			name:       "comma in title",
			args:       []string{"Auth, logging, and tests", "Implement all three"},
			wantTitle:  "Auth, logging, and tests",
			wantPrompt: "Implement all three",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subdir := filepath.Join(t.TempDir(), tt.name)
			os.MkdirAll(subdir, 0755)
			params := &AddParams{Dir: subdir}
			if err := runAdd(params, tt.args); err != nil {
				t.Fatalf("runAdd failed: %v", err)
			}
			tasks, err := ParseTodoMD(filepath.Join(subdir, "TODO.md"))
			if err != nil {
				t.Fatalf("ParseTodoMD failed: %v", err)
			}
			if len(tasks) != 1 {
				t.Fatalf("got %d tasks, want 1", len(tasks))
			}
			if tasks[0].Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", tasks[0].Title, tt.wantTitle)
			}
			if tasks[0].Prompt != tt.wantPrompt {
				t.Errorf("prompt = %q, want %q", tasks[0].Prompt, tt.wantPrompt)
			}
		})
	}
}

func TestParseTodoMDNotFound(t *testing.T) {
	tasks, err := ParseTodoMD("/nonexistent/TODO.md")
	if err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if tasks != nil {
		t.Fatalf("expected nil tasks for missing file, got: %v", tasks)
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TODO.md")

	original := []Task{
		{Title: "Task A", Prompt: "Prompt A line 1\nPrompt A line 2"},
		{Title: "Task B", Prompt: "Simple prompt", PlanMode: true},
		{Title: "Task C", Prompt: "Multi\nline\nprompt"},
		{Title: "Task D", Prompt: "Plan and implement", PlanMode: true, PlanAutoAccept: true},
	}

	if err := WriteTodoMD(path, original); err != nil {
		t.Fatalf("WriteTodoMD failed: %v", err)
	}

	parsed, err := ParseTodoMD(path)
	if err != nil {
		t.Fatalf("ParseTodoMD failed: %v", err)
	}

	if len(parsed) != len(original) {
		t.Fatalf("got %d tasks, want %d", len(parsed), len(original))
	}

	for i := range original {
		if parsed[i].Title != original[i].Title {
			t.Errorf("task %d title = %q, want %q", i, parsed[i].Title, original[i].Title)
		}
		if parsed[i].Prompt != original[i].Prompt {
			t.Errorf("task %d prompt = %q, want %q", i, parsed[i].Prompt, original[i].Prompt)
		}
		if parsed[i].PlanMode != original[i].PlanMode {
			t.Errorf("task %d PlanMode = %v, want %v", i, parsed[i].PlanMode, original[i].PlanMode)
		}
		if parsed[i].PlanAutoAccept != original[i].PlanAutoAccept {
			t.Errorf("task %d PlanAutoAccept = %v, want %v", i, parsed[i].PlanAutoAccept, original[i].PlanAutoAccept)
		}
	}
}
