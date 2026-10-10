package claude

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/federation/hubcmd"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Boa validates required parameters itself rather than marking Cobra's required
// annotation. Its generated help suffix is the public required-flag contract.
// Inspect both mechanisms so neither command builder can silently require an
// unrelated flag on a listing or on a shared multi-action parameter struct.
func requiredCLIFlag(f *pflag.Flag) bool {
	if len(f.Annotations[cobra.BashCompOneRequiredFlag]) > 0 {
		return true
	}
	at := strings.LastIndex(f.Usage, "(")
	if at < 0 || !strings.HasSuffix(f.Usage, ")") {
		return false
	}
	for _, marker := range strings.Split(f.Usage[at+1:len(f.Usage)-1], ",") {
		if strings.TrimSpace(marker) == "required" {
			return true
		}
	}
	return false
}

func TestCommandTreeRequiredFlags(t *testing.T) {
	agentipctest.IsolateManagedAgentEnv(t)
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	// Include optional command subtrees without reaching the operator's daemon.
	if err := config.Save(&config.Config{Agent: &config.AgentConfig{
		GitProxy: &config.GitProxyConfig{AllowedRemotes: []string{"github.com/acme"}},
		AWBProxy: &config.AWBProxyConfig{URL: "https://awb.example"},
	}}); err != nil {
		t.Fatal(err)
	}
	// Deliberately explicit: file inputs, destination groups, decisions, and
	// internal launch endpoints that cannot operate without these particular flags.
	// An action-specific runtime precondition does not belong on a shared struct.
	allow := map[string]string{
		"claude proxy awb comment add":                  "key",
		"claude proxy github pr create":                 "title",
		"claude proxy linear issue create":              "team title",
		"claude proxy linear issue link":                "url",
		"claude agent bundle export":                    "file",
		"claude agent bundle import":                    "file",
		"claude agent groups import":                    "into",
		"claude agent inbox prune":                      "older-than",
		"claude agent process-templates save":           "file",
		"claude agent process-templates validate":       "file",
		"claude agent profiles create":                  "file",
		"claude agent profiles edit":                    "file",
		"claude agent roles create":                     "file",
		"claude agent roles edit":                       "file",
		"claude agent routes open":                      "group",
		"claude agent routes publish":                   "group target",
		"claude agent sandbox-profiles create":          "file",
		"claude agent sandbox-profiles draft":           "token file",
		"claude agent sandbox-profiles edit":            "file",
		"claude agent sandbox-profiles import":          "file",
		"claude agent templates create":                 "file",
		"claude agent templates edit":                   "file",
		"claude agent templates import":                 "file",
		"claude agent templates instantiate":            "group",
		"claude agent templates reinforce":              "group",
		"claude agent tui-dashboard":                    "connect-to",
		"claude config import":                          "file",
		"claude federation answer":                      "decision",
		"claude federation boards contents":             "board item version",
		"claude federation boards create":               "name",
		"claude federation boards download":             "board file item version",
		"claude federation boards fetch":                "board item version",
		"claude federation boards import":               "board item preview-token version",
		"claude federation boards invite":               "board",
		"claude federation boards invites":              "board",
		"claude federation boards items":                "board",
		"claude federation boards join":                 "token",
		"claude federation boards leave":                "board",
		"claude federation boards members":              "board",
		"claude federation boards pin":                  "board item version",
		"claude federation boards preview":              "board item version",
		"claude federation boards publish":              "board",
		"claude federation boards remove-member":        "board instance",
		"claude federation boards revoke-invite":        "board token-id",
		"claude federation boards rotate-key":           "board",
		"claude federation boards set-role":             "board instance role",
		"claude federation boards show":                 "board",
		"claude federation boards versions":             "board item",
		"claude federation enroll-token create":         "profile",
		"claude federation file get":                    "output",
		"claude federation human-inbox reply":           "body",
		"claude federation job run":                     "node repo ref group command",
		"claude federation move-agent":                  "group",
		"claude federation repos add":                   "url clone group",
		"claude federation repos update":                "url clone group",
		"claude federation share-agent":                 "group",
		"claude federation spawn-request":               "brief",
		"claude federation view":                        "node",
		"claude process decide":                         "node verdict",
		"claude process record-outcome":                 "outcome",
		"claude process resolve-blocked":                "node attempt action",
		"claude process templates ls":                   "store-root",
		"claude session codex-app-server-relay":         "socket upstream",
		"claude session codex-app-server-token-consume": "path",
		"claude session codex-profile-cleanup":          "path",
		"claude session http-proxy-exec":                "session-id",
	}
	seen := map[string]bool{}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		allowed := map[string]bool{}
		for _, name := range strings.Fields(allow[cmd.CommandPath()]) {
			allowed[name] = true
		}
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if !requiredCLIFlag(f) {
				return
			}
			key := cmd.CommandPath() + " --" + f.Name
			seen[key] = true
			if !allowed[f.Name] {
				t.Errorf("unexpected required flag %s; mark genuinely optional parameters optional or document the command-specific requirement in the allowlist", key)
			}
		})
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(Cmd())
	walk(hubcmd.RootCmd())
	for cmd, names := range allow {
		for _, name := range strings.Fields(names) {
			if !seen[cmd+" --"+name] {
				t.Errorf("stale required-flag exception: %s --%s", cmd, name)
			}
		}
	}
}
