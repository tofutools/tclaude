## Make test more robust

In `@pkg/claude/task/run_test.go` around lines 134 - 135, The test currently
swallows errors from exec.Command("git", "-C", dir, "add", "file.txt").Run() and
exec.Command("git", "-C", dir, "commit", "-m", "add file").Run(); create a small
helper (like the existing initGitRepo pattern) e.g. gitRun(t, dir, args...) that
calls t.Helper(), runs the command capturing output and error
(cmd.CombinedOutput or cmd.Run), and fails fast with t.Fatalf or require.NoError
including the command output on error; replace the two silent .Run() calls with
calls to that helper so failures are reported immediately and with context.
