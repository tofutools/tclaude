# Fish shell wrapper for tclaude worktree switch
#
# This function shadows the tclaude binary to intercept 'worktree switch' commands
# and actually change directory to the worktree.
#
# Usage:
#   source /path/to/tclaude-worktree-switch.fish
#
# Then:
#   tclaude worktree switch feat/my-feature  # cd's to the worktree
#   tclaude worktree s main                  # short alias
#   tclaude worktree c main                  # checkout alias

function tclaude
    # Check if this is a worktree switch command
    # Matches: tclaude worktree (switch|s|checkout|c) <target>
    if test (count $argv) -ge 3
        and test "$argv[1]" = "worktree"
        and contains -- "$argv[2]" switch s checkout c

        set -l dir (command tclaude $argv 2>&1)
        set -l status_code $status

        if test $status_code -eq 0 -a -n "$dir" -a -d "$dir"
            cd $dir
        else
            # Output error message
            echo $dir >&2
            return $status_code
        end
    else
        # Pass through to the real tclaude command
        command tclaude $argv
    end
end
