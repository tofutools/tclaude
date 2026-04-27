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
			if err := os.MkdirAll(subdir, 0755); err != nil {
				t.Fatalf("unable to create directory %s: %v", subdir, err)
			}
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

func TestRunAddPlanFlags(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		params       AddParams
		wantPlanMode bool
		wantPlanAuto bool
	}{
		{
			name:         "no flags",
			args:         []string{"Do the thing", "Implement it"},
			wantPlanMode: false,
			wantPlanAuto: false,
		},
		{
			name:         "plan flag",
			args:         []string{"Design something", "Design it"},
			params:       AddParams{PlanMode: true},
			wantPlanMode: true,
			wantPlanAuto: false,
		},
		{
			name:         "plan-auto flag implies plan",
			args:         []string{"Design and build", "Design then build"},
			params:       AddParams{PlanAutoAccept: true},
			wantPlanMode: true,
			wantPlanAuto: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.params.Dir = t.TempDir()
			if err := runAdd(&tt.params, tt.args); err != nil {
				t.Fatalf("runAdd failed: %v", err)
			}
			tasks, err := ParseTodoMD(filepath.Join(tt.params.Dir, "TODO.md"))
			if err != nil {
				t.Fatalf("ParseTodoMD failed: %v", err)
			}
			if len(tasks) != 1 {
				t.Fatalf("got %d tasks, want 1", len(tasks))
			}
			if tasks[0].PlanMode != tt.wantPlanMode {
				t.Errorf("PlanMode = %v, want %v", tasks[0].PlanMode, tt.wantPlanMode)
			}
			if tasks[0].PlanAutoAccept != tt.wantPlanAuto {
				t.Errorf("PlanAutoAccept = %v, want %v", tasks[0].PlanAutoAccept, tt.wantPlanAuto)
			}
		})
	}
}

func TestRunAddAppend(t *testing.T) {
	dir := t.TempDir()
	params := &AddParams{Dir: dir}

	if err := runAdd(params, []string{"First task", "Do first thing"}); err != nil {
		t.Fatalf("first runAdd failed: %v", err)
	}
	if err := runAdd(params, []string{"Second task", "Do second thing"}); err != nil {
		t.Fatalf("second runAdd failed: %v", err)
	}

	tasks, err := ParseTodoMD(filepath.Join(dir, "TODO.md"))
	if err != nil {
		t.Fatalf("ParseTodoMD failed: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].Title != "First task" || tasks[0].Prompt != "Do first thing" {
		t.Errorf("task 0 = {%q, %q}, want {First task, Do first thing}", tasks[0].Title, tasks[0].Prompt)
	}
	if tasks[1].Title != "Second task" || tasks[1].Prompt != "Do second thing" {
		t.Errorf("task 1 = {%q, %q}, want {Second task, Do second thing}", tasks[1].Title, tasks[1].Prompt)
	}
}

func writeTasksConfig(t *testing.T, dir string, content string) {
	t.Helper()
	path := TasksConfigPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("failed to create tasks config dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write tasks config: %v", err)
	}
}

func TestLoadTasksConfig(t *testing.T) {
	t.Run("missing file returns defaults", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.VerifyCmd != "" {
			t.Errorf("VerifyCmd = %q, want empty", cfg.VerifyCmd)
		}
		if cfg.MaxVerifyIterations != defaultMaxVerifyIterations {
			t.Errorf("MaxVerifyIterations = %d, want %d", cfg.MaxVerifyIterations, defaultMaxVerifyIterations)
		}
	})

	t.Run("verify only", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"verify":"go test ./..."}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.VerifyCmd != "go test ./..." {
			t.Errorf("VerifyCmd = %q, want %q", cfg.VerifyCmd, "go test ./...")
		}
		if cfg.MaxVerifyIterations != defaultMaxVerifyIterations {
			t.Errorf("MaxVerifyIterations = %d, want %d", cfg.MaxVerifyIterations, defaultMaxVerifyIterations)
		}
	})

	t.Run("verify and max iterations", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"verify":"make test","max_verify_iterations":5}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.VerifyCmd != "make test" {
			t.Errorf("VerifyCmd = %q, want %q", cfg.VerifyCmd, "make test")
		}
		if cfg.MaxVerifyIterations != 5 {
			t.Errorf("MaxVerifyIterations = %d, want 5", cfg.MaxVerifyIterations)
		}
	})

	t.Run("zero max_verify_iterations falls back to default", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_verify_iterations":0}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.MaxVerifyIterations != defaultMaxVerifyIterations {
			t.Errorf("MaxVerifyIterations = %d, want %d", cfg.MaxVerifyIterations, defaultMaxVerifyIterations)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_verify_iterations":0,"verify_timeout":"2m"}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.VerifyTimeout != 2*time.Minute {
			t.Errorf("VerifyTimeout = %d, want %d", cfg.VerifyTimeout, 2*time.Minute)
		}
	})

	t.Run("max_review_iterations default", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.MaxReviewIterations != defaultMaxReviewIterations {
			t.Errorf("MaxReviewIterations = %d, want %d", cfg.MaxReviewIterations, defaultMaxReviewIterations)
		}
	})

	t.Run("max_review_iterations custom", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_review_iterations":3}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.MaxReviewIterations != 3 {
			t.Errorf("MaxReviewIterations = %d, want 3", cfg.MaxReviewIterations)
		}
	})

	t.Run("zero max_review_iterations falls back to default", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_review_iterations":0}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.MaxReviewIterations != defaultMaxReviewIterations {
			t.Errorf("MaxReviewIterations = %d, want %d", cfg.MaxReviewIterations, defaultMaxReviewIterations)
		}
	})

	t.Run("review_timeout default", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ReviewTimeout != defaultReviewTimeout {
			t.Errorf("ReviewTimeout = %v, want %v", cfg.ReviewTimeout, defaultReviewTimeout)
		}
	})

	t.Run("review_timeout custom", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"review_timeout":"10m"}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ReviewTimeout != 10*time.Minute {
			t.Errorf("ReviewTimeout = %v, want 10m", cfg.ReviewTimeout)
		}
	})

	t.Run("invalid review_timeout returns error", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"review_timeout":"notaduratiokn"}`)
		_, err := LoadTasksConfig(dir)
		if err == nil {
			t.Error("expected error for invalid review_timeout, got nil")
		}
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `not json`)
		_, err := LoadTasksConfig(dir)
		if err == nil {
			t.Error("expected error for invalid JSON, got nil")
		}
	})

	t.Run("review_diff defaults to true when absent", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ReviewDiff == nil || !*cfg.ReviewDiff {
			t.Errorf("ReviewDiff = %v, want true", cfg.ReviewDiff)
		}
	})

	t.Run("review_diff false", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"review_diff":false}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ReviewDiff == nil || *cfg.ReviewDiff {
			t.Errorf("ReviewDiff = %v, want false", cfg.ReviewDiff)
		}
	})

	t.Run("review_diff true explicit", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"review_diff":true}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.ReviewDiff == nil || !*cfg.ReviewDiff {
			t.Errorf("ReviewDiff = %v, want true", cfg.ReviewDiff)
		}
	})

	t.Run("stuck_timeout default", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.StuckTimeout != defaultStuckTimeout {
			t.Errorf("StuckTimeout = %v, want %v", cfg.StuckTimeout, defaultStuckTimeout)
		}
	})

	t.Run("stuck_timeout custom", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"stuck_timeout":"10m"}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.StuckTimeout != 10*time.Minute {
			t.Errorf("StuckTimeout = %v, want 10m", cfg.StuckTimeout)
		}
	})

	t.Run("stuck_timeout zero disables detection", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"stuck_timeout":"0s"}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.StuckTimeout != 0 {
			t.Errorf("StuckTimeout = %v, want 0", cfg.StuckTimeout)
		}
	})

	t.Run("stuck_timeout below minimum returns error", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"stuck_timeout":"20s"}`)
		_, err := LoadTasksConfig(dir)
		if err == nil {
			t.Error("expected error for stuck_timeout below minimum, got nil")
		}
	})

	t.Run("invalid stuck_timeout returns error", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"stuck_timeout":"notaduration"}`)
		_, err := LoadTasksConfig(dir)
		if err == nil {
			t.Error("expected error for invalid stuck_timeout, got nil")
		}
	})

	t.Run("max_stuck_nudges default", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.MaxStuckNudges != defaultMaxStuckNudges {
			t.Errorf("MaxStuckNudges = %d, want %d", cfg.MaxStuckNudges, defaultMaxStuckNudges)
		}
	})

	t.Run("max_stuck_nudges custom", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_stuck_nudges":5}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.MaxStuckNudges != 5 {
			t.Errorf("MaxStuckNudges = %d, want 5", cfg.MaxStuckNudges)
		}
	})

	t.Run("zero max_stuck_nudges falls back to default", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_stuck_nudges":0}`)
		cfg, err := LoadTasksConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.MaxStuckNudges != defaultMaxStuckNudges {
			t.Errorf("MaxStuckNudges = %d, want %d", cfg.MaxStuckNudges, defaultMaxStuckNudges)
		}
	})
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
