package harness

import (
	"fmt"
	"io"
	"strings"
)

// PrintContextFeatureCatalog writes the human-readable catalog behind
// `--help-context-features`. Kept out of the flag's own help string so
// `tclaude session new --help` stays readable while the slugs, what each one
// costs, and the sharp edges are still discoverable from the terminal. The
// dashboard renders the same catalog from the snapshot.
func PrintContextFeatureCatalog(w io.Writer) {
	fmt.Fprintln(w, "Startup-context features --context-features can steer (Claude Code only).")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  Syntax:  --context-features <feature>[=on|off][,…]   (a bare feature means off)")
	fmt.Fprintln(w, "  States:  off = trim it out of the agent's startup context")
	fmt.Fprintln(w, "           on  = keep it even when a profile or group default trimmed it")
	fmt.Fprintln(w, "           unset = leave Claude Code's own default alone")
	fmt.Fprintln(w)
	for _, f := range ContextFeatures() {
		marker := "  "
		if f.Heavy {
			marker = "★ " // biggest startup-context payoff
		}
		fmt.Fprintf(w, "%s%-22s %s\n", marker, f.Slug, f.Descr)
		if f.Caution != "" {
			fmt.Fprintf(w, "  %-22s ⚠ %s\n", "", f.Caution)
		}
		switch {
		case f.EnvVar != "":
			fmt.Fprintf(w, "  %-22s via %s\n", "", f.EnvVar)
		case f.SettingsKey != "":
			fmt.Fprintf(w, "  %-22s via settings.json %s\n", "", f.SettingsKey)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Example: --context-features "+strings.Join([]string{
		"bundled-skills", "artifact", "workflows",
	}, ","))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "These switches were catalogued from a specific Claude Code build rather than a")
	fmt.Fprintln(w, "documented contract, so one may stop taking effect after a harness upgrade. That")
	fmt.Fprintln(w, "degrades safely — the agent keeps a capability it did not need. Check /context")
	fmt.Fprintln(w, "inside a spawned agent if you want to confirm a trim landed.")
}
