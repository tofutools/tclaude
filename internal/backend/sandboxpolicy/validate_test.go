package sandboxpolicy_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

func TestAuthoredSandboxPolicyRequiresNoHostEffects(t *testing.T) {
	// Neither nonexistent paths nor executable-looking literal values are
	// interpreted while authoring. Host existence and confinement are later gates.
	p := model.SandboxPolicy{
		Includes:         []model.SandboxProfileRef{{ProfileID: "sandbox_parent", RevisionID: "sandbox_revision", ContentHash: strings.Repeat("a", 64)}},
		Filesystem:       []model.SandboxFilesystemRule{{HostPath: "/nonexistent/sandbox-source", Access: model.SandboxFilesystemRead, GuestPath: "/workspace/input", ExpectedKind: "file"}},
		Tmpfs:            []model.SandboxTmpfs{{GuestPath: "/scratch", Size: ".5MiB"}},
		Environment:      model.Environment{"LITERAL": "$(touch /not-executed)\n'quoted'"},
		AgentDirectories: []string{"BUILD_CACHE"},
		FilesystemRoot:   model.SandboxRootSeparate,
		Network:          &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny, Allow: []model.SandboxDestination{{Host: "API.Example.com", Ports: []uint16{443}}, {CIDR: "::ffff:192.0.2.0/120"}}},
		UnixSockets:      &model.SandboxUnixSockets{Mode: "list", Allow: []model.SandboxSocketSelector{{PathGlob: "/nonexistent/agent-*/control.sock"}}},
		Resources:        model.SandboxResources{Memory: "1.5GiB", CPU: ".125"},
		PreLaunch:        []model.SandboxSetupBlock{{Name: "first", Script: "export PATH=/private/tools:$PATH\n", Exports: []string{"PATH"}}, {Name: "second", Script: "printf '%s' \"$LITERAL\""}},
	}
	require.NoError(t, sandboxpolicy.Validate(p))
	require.Equal(t, "export PATH=/private/tools:$PATH\n", p.PreLaunch[0].Script)
	require.Equal(t, "API.Example.com", p.Network.Allow[0].Host)
	require.Equal(t, "$(touch /not-executed)\n'quoted'", p.Environment["LITERAL"])
}

func TestSandboxAuthoringRefusesAmbiguousAndUnrepresentablePolicies(t *testing.T) {
	cases := map[string]model.SandboxPolicy{
		"remap root":                    {Filesystem: []model.SandboxFilesystemRule{{HostPath: "/host", Access: model.SandboxFilesystemRead, GuestPath: "/"}}},
		"deny file":                     {Filesystem: []model.SandboxFilesystemRule{{HostPath: "/host", Access: model.SandboxFilesystemDeny, ExpectedKind: "file"}}},
		"relative host":                 {Filesystem: []model.SandboxFilesystemRule{{HostPath: "../secret", Access: model.SandboxFilesystemRead}}},
		"unclean guest":                 {Tmpfs: []model.SandboxTmpfs{{GuestPath: "/work/../scratch"}}},
		"deny remap":                    {Filesystem: []model.SandboxFilesystemRule{{HostPath: "/secret", GuestPath: "/other", Access: model.SandboxFilesystemDeny}}},
		"unknown kind":                  {Filesystem: []model.SandboxFilesystemRule{{HostPath: "/secret", Access: model.SandboxFilesystemRead, ExpectedKind: "whatever"}}},
		"reserved environment":          {Environment: model.Environment{"HOME": "/other"}},
		"directory shadows literal":     {Environment: model.Environment{"CACHE": "/tmp"}, AgentDirectories: []string{"CACHE"}},
		"invalid UTF8 environment":      {Environment: model.Environment{"VALUE": string([]byte{255})}},
		"invalid UTF8 script":           {PreLaunch: []model.SandboxSetupBlock{{Name: "setup", Script: string([]byte{255})}}},
		"NUL script":                    {PreLaunch: []model.SandboxSetupBlock{{Name: "setup", Script: "echo\x00"}}},
		"duplicate setup name":          {PreLaunch: []model.SandboxSetupBlock{{Name: "setup", Script: "true"}, {Name: "setup", Script: "false"}}},
		"script name injection":         {PreLaunch: []model.SandboxSetupBlock{{Name: "setup; echo bad", Script: "true"}}},
		"socket both selectors":         {UnixSockets: &model.SandboxUnixSockets{Mode: "list", Allow: []model.SandboxSocketSelector{{Path: "/a", PathGlob: "/b/*"}}}},
		"socket recursive glob":         {UnixSockets: &model.SandboxUnixSockets{Mode: "list", Allow: []model.SandboxSocketSelector{{PathGlob: "/a/**/socket"}}}},
		"socket multiple glob segments": {UnixSockets: &model.SandboxUnixSockets{Mode: "list", Allow: []model.SandboxSocketSelector{{PathGlob: "/a/*/b/*"}}}},
		"socket closed selectors":       {UnixSockets: &model.SandboxUnixSockets{Mode: "closed", Allow: []model.SandboxSocketSelector{{Path: "/a"}}}},
		"missing hash":                  {Includes: []model.SandboxProfileRef{{ProfileID: "sandbox_a", RevisionID: "sandbox_b"}}},
		"unknown filesystem posture":    {FilesystemRoot: "unrestricted"},
		"unknown harness config":        {HarnessConfig: "off"},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) { require.Error(t, sandboxpolicy.Validate(p)) })
	}
}

func TestSandboxNetworkKeepsLoopbackAuthorityExplicit(t *testing.T) {
	for _, cidr := range []string{"127.0.0.1/32", "0.0.0.0/0", "0.0.0.0/32", "::/0", "::1/128", "::ffff:127.0.0.1/128", "::ffff:0.0.0.0/128", "::ffff:0:0/96"} {
		t.Run(cidr, func(t *testing.T) {
			require.Error(t, sandboxpolicy.Validate(model.SandboxPolicy{Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny, Allow: []model.SandboxDestination{{CIDR: cidr}}}}))
		})
	}
	for _, d := range []model.SandboxDestination{
		{Host: "https://example.com"}, {Host: "127.0.0.1"}, {Host: "example.com", Loopback: true},
		{Host: "example.com", IncludeSubdomains: true}, {Domain: "*.example.com"}, {Domain: "example.com", Ports: []uint16{0}},
	} {
		require.Error(t, sandboxpolicy.Validate(model.SandboxPolicy{Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny, Allow: []model.SandboxDestination{d}}}))
	}
	require.NoError(t, sandboxpolicy.Validate(model.SandboxPolicy{Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny, Allow: []model.SandboxDestination{{Loopback: true, Ports: []uint16{8080}}}}}))
	require.Error(t, sandboxpolicy.Validate(model.SandboxPolicy{Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkInherit, Allow: []model.SandboxDestination{{Host: "example.com"}}}}))
}

func TestSandboxQuantitiesPreserveExactCeilings(t *testing.T) {
	for value, want := range map[string]uint64{".1B": 1, "1.5GiB": 1610612736, "9007199254740993B": 9007199254740993, "18446744073709551615B": 18446744073709551615} {
		got, err := sandboxpolicy.ParseMemoryLimitBytes(value)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, value := range []string{"0B", "-1B", "NaN", "1e3B", "18446744073709551616B", strings.Repeat("9", 129) + "B"} {
		_, err := sandboxpolicy.ParseMemoryLimitBytes(value)
		require.Error(t, err)
	}
	for value, want := range map[string]uint64{".01": 1000, ".100000001": 10001, "90071992547.40993": 9007199254740993} {
		got, err := sandboxpolicy.CPUQuotaMicros(value)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, value := range []string{".009", "0", "NaN", "+Inf", "1e4", "18446744073709551616"} {
		_, err := sandboxpolicy.CPUQuotaMicros(value)
		require.Error(t, err)
	}
}
