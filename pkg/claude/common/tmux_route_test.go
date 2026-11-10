package common

import "testing"

func TestControlRoute(t *testing.T) {
	cases := []struct {
		args []string
		want routeKind
	}{
		{[]string{"has-session", "-t", "=a"}, routeQuery},
		{[]string{"display-message", "-p", "-t", "=a:", "#{pane_dead}"}, routeQuery},
		{[]string{"display-message", "-p", "#{pid}"}, routeExec},                     // no target: would read the control session
		{[]string{"display-message", "-t", "=a:", "hi"}, routeExec},                  // no -p: status-line message
		{[]string{"display-message", "-p", "-t", "=a:", "#{client_tty}"}, routeExec}, // client_* = the control client
		{[]string{"list-clients", "-t", "=a", "-F", "#{client_tty}"}, routeQuery},    // client_* of the listed clients
		{[]string{"capture-pane", "-p", "-J", "-S", "-40", "-t", "%3"}, routeExec},   // screen text a pane controls
		{[]string{"display-message", "-p", "-t", "=a:", "#{pane_current_path}"}, routeExec},
		{[]string{"display-message", "-p", "-t", "=a:", "#{q:pane_title}"}, routeExec},
		{[]string{"display-message", "-p", "-t", "=a:", "#{pane_id}\t#{window_id}"}, routeQuery},
		{[]string{"display-message", "-p", "-t", "%1", "#{==:#{pane_mode},copy-mode}"}, routeQuery},
		{[]string{"display-message", "-p", "-t", "%1", "#{?mouse_all_flag,1,0} #{@tclaude_exit_generation}"}, routeQuery},
		{[]string{"list-sessions", "-F", "#{session_name}\t#{pane_current_path}"}, routeExec},
		{[]string{"send-keys", "-t", "%1", "-l", "--", `ls\;`}, routeExec}, // argv spelling of a literal ";"
		{[]string{"list-panes", "-a", "-F", "#{pane_id}"}, routeQuery},
		{[]string{"list-panes", "-F", "#{pane_id}"}, routeExec},
		{[]string{"list-sessions", "-F", "#{session_name}"}, routeQuery},
		{[]string{"show-options", "-gv", "status"}, routeQuery},
		{[]string{"show-hooks", "-g", "pane-died"}, routeQuery},
		{[]string{"set-option", "-gu", "exit-empty"}, routeMutation},
		{[]string{"set-option", "-p", "-t", "%1", "remain-on-exit", "on"}, routeMutation},
		{[]string{"set-option", "mouse", "on"}, routeExec}, // no target: the control session
		{[]string{"kill-session", "-t", "=a"}, routeMutation},
		{[]string{"send-keys", "-t", "%1", "-l", "text"}, routeMutation},
		{[]string{"switch-client", "-t", "=a"}, routeExec}, // no -c: the control client
		{[]string{"switch-client", "-c", "/dev/pts/3", "-t", "=a"}, routeMutation},
		{[]string{"detach-client", "-s", "=a"}, routeMutation},
		{[]string{"new-session", "-d", "-s", "x"}, routeExec},
		{[]string{"kill-server"}, routeExec},
		{[]string{"bind-key", "-n", "S-Right", "run-shell", "x"}, routeExec},
		{[]string{"-V"}, routeExec},
		{[]string{"set-option", "-t", "=a:", "x", "1", ";", "set-option", "-t", "=a:", "y", "2"}, routeExec},
	}
	for _, c := range cases {
		if got := controlRoute(c.args); got != c.want {
			t.Errorf("%q: %v, want %v", c.args, got, c.want)
		}
	}
}
