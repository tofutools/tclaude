package task

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			require.Equal(t, len(tt.want), len(got), "got %d tasks, want %d", len(got), len(tt.want))
			for i := range got {
				assert.Equal(t, tt.want[i].Title, got[i].Title, "task %d title", i)
				assert.Equal(t, tt.want[i].Prompt, got[i].Prompt, "task %d prompt", i)
				assert.Equal(t, tt.want[i].PlanMode, got[i].PlanMode, "task %d PlanMode", i)
				assert.Equal(t, tt.want[i].PlanAutoAccept, got[i].PlanAutoAccept, "task %d PlanAutoAccept", i)
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

	err := WriteTodoMD(path, tasks)
	require.NoError(t, err, "WriteTodoMD failed")

	// Read back and parse
	parsed, err := ParseTodoMD(path)
	require.NoError(t, err, "ParseTodoMD failed")

	require.Len(t, parsed, 2)
	assert.Equal(t, "First task", parsed[0].Title)
	assert.Equal(t, "Second task", parsed[1].Title)
}

func TestWriteTodoMDEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "TODO.md")

	err := WriteTodoMD(path, nil)
	require.NoError(t, err, "WriteTodoMD failed")

	data, err := os.ReadFile(path)
	require.NoError(t, err, "ReadFile failed")
	assert.Empty(t, string(data), "expected empty file")
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

	err := AppendDoneMD(path, result)
	require.NoError(t, err, "AppendDoneMD failed")

	data, err := os.ReadFile(path)
	require.NoError(t, err, "ReadFile failed")

	content := string(data)
	assert.Contains(t, content, "## Test task", "missing task title")
	assert.Contains(t, content, "**Status:** completed", "missing status")
	assert.Contains(t, content, "**Commit:** abc1234", "missing commit hash")
	assert.Contains(t, content, "**Plan:** ~/.claude/plans/enchanted-meandering-mountain.md", "missing plan file")
	assert.Contains(t, content, "Do something", "missing prompt")
	assert.Contains(t, content, "I did the thing.", "missing report")

	// Append a second result
	result2 := TaskResult{
		Title:     "Second task",
		Prompt:    "Do another thing",
		Status:    "failed",
		Error:     "something broke",
		Timestamp: time.Date(2026, 3, 7, 15, 0, 0, 0, time.UTC),
	}

	err = AppendDoneMD(path, result2)
	require.NoError(t, err, "AppendDoneMD (second) failed")

	data, err = os.ReadFile(path)
	require.NoError(t, err, "ReadFile (second) failed")

	content = string(data)
	assert.Contains(t, content, "## Second task", "missing second task title")
	assert.Contains(t, content, "**Status:** failed", "missing failed status")
	assert.Contains(t, content, "**Error:** something broke", "missing error message")
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
			err := os.MkdirAll(subdir, 0755)
			require.NoError(t, err, "unable to create directory %s", subdir)

			params := &AddParams{Dir: subdir}
			err = runAdd(params, tt.args)
			require.NoError(t, err, "runAdd failed")

			tasks, err := ParseTodoMD(filepath.Join(subdir, "TODO.md"))
			require.NoError(t, err, "ParseTodoMD failed")

			require.Len(t, tasks, 1)
			assert.Equal(t, tt.wantTitle, tasks[0].Title, "title")
			assert.Equal(t, tt.wantPrompt, tasks[0].Prompt, "prompt")
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
			err := runAdd(&tt.params, tt.args)
			require.NoError(t, err, "runAdd failed")

			tasks, err := ParseTodoMD(filepath.Join(tt.params.Dir, "TODO.md"))
			require.NoError(t, err, "ParseTodoMD failed")

			require.Len(t, tasks, 1)
			assert.Equal(t, tt.wantPlanMode, tasks[0].PlanMode, "PlanMode")
			assert.Equal(t, tt.wantPlanAuto, tasks[0].PlanAutoAccept, "PlanAutoAccept")
		})
	}
}

func TestRunAddAppend(t *testing.T) {
	dir := t.TempDir()
	params := &AddParams{Dir: dir}

	err := runAdd(params, []string{"First task", "Do first thing"})
	require.NoError(t, err, "first runAdd failed")

	err = runAdd(params, []string{"Second task", "Do second thing"})
	require.NoError(t, err, "second runAdd failed")

	tasks, err := ParseTodoMD(filepath.Join(dir, "TODO.md"))
	require.NoError(t, err, "ParseTodoMD failed")

	require.Len(t, tasks, 2)
	assert.Equal(t, "First task", tasks[0].Title)
	assert.Equal(t, "Do first thing", tasks[0].Prompt)
	assert.Equal(t, "Second task", tasks[1].Title)
	assert.Equal(t, "Do second thing", tasks[1].Prompt)
}

func writeTasksConfig(t *testing.T, dir string, content string) {
	t.Helper()
	path := TasksConfigPath(dir)
	err := os.MkdirAll(filepath.Dir(path), 0755)
	require.NoError(t, err, "failed to create tasks config dir")

	err = os.WriteFile(path, []byte(content), 0644)
	require.NoError(t, err, "failed to write tasks config")
}

func TestLoadTasksConfig(t *testing.T) {
	t.Run("missing file returns defaults", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		require.NoError(t, err, "unexpected error")
		assert.Empty(t, cfg.VerifyCmd)
		assert.Equal(t, defaultMaxVerifyIterations, cfg.MaxVerifyIterations)
	})

	t.Run("verify only", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"verify":"go test ./..."}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, "go test ./...", cfg.VerifyCmd)
		assert.Equal(t, defaultMaxVerifyIterations, cfg.MaxVerifyIterations)
	})

	t.Run("verify and max iterations", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"verify":"make test","max_verify_iterations":5}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, "make test", cfg.VerifyCmd)
		assert.Equal(t, 5, cfg.MaxVerifyIterations)
	})

	t.Run("zero max_verify_iterations falls back to default", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_verify_iterations":0}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, defaultMaxVerifyIterations, cfg.MaxVerifyIterations)
	})

	t.Run("timeout", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_verify_iterations":0,"verify_timeout":"2m"}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, 2*time.Minute, cfg.VerifyTimeout)
	})

	t.Run("max_review_iterations default", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, defaultMaxReviewIterations, cfg.MaxReviewIterations)
	})

	t.Run("max_review_iterations custom", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_review_iterations":3}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, 3, cfg.MaxReviewIterations)
	})

	t.Run("zero max_review_iterations falls back to default", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_review_iterations":0}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, defaultMaxReviewIterations, cfg.MaxReviewIterations)
	})

	t.Run("review_prefix default", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, defaultReviewPrefix, cfg.ReviewPrefix)
	})

	t.Run("review_prefix custom", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"review_prefix":"some prefix:"}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, "some prefix:", cfg.ReviewPrefix)
	})

	t.Run("review_timeout default", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, defaultReviewTimeout, cfg.ReviewTimeout)
	})

	t.Run("review_timeout custom", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"review_timeout":"10m"}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, 10*time.Minute, cfg.ReviewTimeout)
	})

	t.Run("invalid review_timeout returns error", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"review_timeout":"notaduratiokn"}`)
		_, err := LoadTasksConfig(dir)
		assert.Error(t, err, "expected error for invalid review_timeout")
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `not json`)
		_, err := LoadTasksConfig(dir)
		assert.Error(t, err, "expected error for invalid JSON")
	})

	t.Run("review_diff defaults to true when absent", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		require.NoError(t, err, "unexpected error")
		require.NotNil(t, cfg.ReviewDiff)
		assert.True(t, *cfg.ReviewDiff)
	})

	t.Run("review_diff false", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"review_diff":false}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		require.NotNil(t, cfg.ReviewDiff)
		assert.False(t, *cfg.ReviewDiff)
	})

	t.Run("review_diff true explicit", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"review_diff":true}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		require.NotNil(t, cfg.ReviewDiff)
		assert.True(t, *cfg.ReviewDiff)
	})

	t.Run("stuck_timeout default", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, defaultStuckTimeout, cfg.StuckTimeout)
	})

	t.Run("stuck_timeout custom", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"stuck_timeout":"10m"}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, 10*time.Minute, cfg.StuckTimeout)
	})

	t.Run("stuck_timeout zero disables detection", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"stuck_timeout":"0s"}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, time.Duration(0), cfg.StuckTimeout)
	})

	t.Run("stuck_timeout below minimum returns error", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"stuck_timeout":"20s"}`)
		_, err := LoadTasksConfig(dir)
		assert.Error(t, err, "expected error for stuck_timeout below minimum")
	})

	t.Run("invalid stuck_timeout returns error", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"stuck_timeout":"notaduration"}`)
		_, err := LoadTasksConfig(dir)
		assert.Error(t, err, "expected error for invalid stuck_timeout")
	})

	t.Run("max_stuck_nudges default", func(t *testing.T) {
		cfg, err := LoadTasksConfig(t.TempDir())
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, defaultMaxStuckNudges, cfg.MaxStuckNudges)
	})

	t.Run("max_stuck_nudges custom", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_stuck_nudges":5}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, 5, cfg.MaxStuckNudges)
	})

	t.Run("zero max_stuck_nudges falls back to default", func(t *testing.T) {
		dir := t.TempDir()
		writeTasksConfig(t, dir, `{"max_stuck_nudges":0}`)
		cfg, err := LoadTasksConfig(dir)
		require.NoError(t, err, "unexpected error")
		assert.Equal(t, defaultMaxStuckNudges, cfg.MaxStuckNudges)
	})
}

func TestParseTodoMDNotFound(t *testing.T) {
	tasks, err := ParseTodoMD("/nonexistent/TODO.md")
	assert.NoError(t, err, "expected nil error for missing file")
	assert.Nil(t, tasks, "expected nil tasks for missing file")
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

	err := WriteTodoMD(path, original)
	require.NoError(t, err, "WriteTodoMD failed")

	parsed, err := ParseTodoMD(path)
	require.NoError(t, err, "ParseTodoMD failed")

	require.Len(t, parsed, len(original))

	for i := range original {
		assert.Equal(t, original[i].Title, parsed[i].Title, "task %d title", i)
		assert.Equal(t, original[i].Prompt, parsed[i].Prompt, "task %d prompt", i)
		assert.Equal(t, original[i].PlanMode, parsed[i].PlanMode, "task %d PlanMode", i)
		assert.Equal(t, original[i].PlanAutoAccept, parsed[i].PlanAutoAccept, "task %d PlanAutoAccept", i)
	}
}
