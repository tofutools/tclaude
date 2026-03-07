# Zsh shell wrapper for tclaude worktree switch
#
# This function shadows the tclaude binary to intercept 'worktree switch' commands
# and actually change directory to the worktree.
#
# Usage:
#   source /path/to/tclaude-worktree-switch.zsh
#
# Then:
#   tclaude worktree switch feat/my-feature  # cd's to the worktree
#   tclaude worktree s main                  # short alias
#   tclaude worktree c main                  # checkout alias

tclaude() {
    # Check if this is a worktree switch command
    # Matches: tclaude worktree (switch|s|checkout|c) <target>
    if [[ $# -ge 3 && "$1" == "worktree" && "$2" =~ ^(switch|s|checkout|c)$ ]]; then
        local dir
        dir=$(command tclaude "$@" 2>&1)
        local status_code=$?

        if [[ $status_code -eq 0 && -n "$dir" && -d "$dir" ]]; then
            cd "$dir"
        else
            echo "$dir" >&2
            return $status_code
        fi
    else
        # Pass through to the real tclaude command
        command tclaude "$@"
    fi
}
