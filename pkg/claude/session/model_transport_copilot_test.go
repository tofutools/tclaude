package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// copilotLaunchContext builds a launch context whose environment is fully
// specified, so a resolver decision can never depend on the developer's own
// shell. Every variable the resolver reads is set here — including the ones
// set to empty, which is what makes "unset" a tested state rather than an
// inherited accident.
func copilotLaunchContext(t *testing.T, overrides map[string]string) (
	ModelTransportLaunchContext, string,
) {
	t.Helper()
	home := t.TempDir()
	environment := []sandboxpolicy.EnvironmentEntry{{Name: "HOME", Value: home}}
	for _, name := range append(harness.CopilotRouteMovingEnvVars(),
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy",
		harness.CopilotHomeEnvVar) {
		environment = append(environment,
			sandboxpolicy.EnvironmentEntry{Name: name, Value: overrides[name]})
	}
	return ModelTransportLaunchContext{
		Model:       "claude-sonnet-5",
		Cwd:         home,
		Environment: environment,
	}, home
}

func writeCopilotLaunchSettings(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".copilot")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("prepare Copilot home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, harness.CopilotSettingsFileName),
		[]byte(body), 0o600); err != nil {
		t.Fatalf("write Copilot settings: %v", err)
	}
}

// TestResolveCopilotModelTransportAcceptsFirstPartyRoute is the positive case:
// a launch that names no route-moving input resolves to the first-party route
// with no endpoint of its own, which is what lets the descriptor supply the
// audited destinations.
func TestResolveCopilotModelTransportAcceptsFirstPartyRoute(t *testing.T) {
	h := harness.MustGet(harness.CopilotName)
	context, home := copilotLaunchContext(t, nil)

	for _, settings := range []string{
		"",
		`{"model":"claude-sonnet-5"}`,
		// An explicitly empty / null routing key leaves the default route in
		// place, so refusing over it would send an operator hunting for a
		// setting that changes nothing.
		`{"copilotUrl":""}`,
		`{"copilotUrl":null,"proxyUrl":"  "}`,
	} {
		if settings != "" {
			writeCopilotLaunchSettings(t, home, settings)
		}
		resolved, err := ResolveTclaudeLayerModelTransport(h, context)
		if err != nil {
			t.Fatalf("settings %q: ResolveTclaudeLayerModelTransport = %v", settings, err)
		}
		if !resolved.ProviderResolved {
			t.Fatalf("settings %q: provider was not resolved", settings)
		}
		if resolved.Provider != harness.CopilotName {
			t.Errorf("settings %q: Provider = %q, want %q",
				settings, resolved.Provider, harness.CopilotName)
		}
		if resolved.BaseURL != "" {
			t.Errorf("settings %q: BaseURL = %q, want empty; the destinations are the "+
				"descriptor's audited defaults, not a launch-resolved endpoint",
				settings, resolved.BaseURL)
		}
	}
}

// TestResolveCopilotModelTransportRefusesRouteMovingEnvironment covers every
// variable in the contract list, so a variable added to the list without a
// refusal path (or removed from it silently) fails here.
func TestResolveCopilotModelTransportRefusesRouteMovingEnvironment(t *testing.T) {
	h := harness.MustGet(harness.CopilotName)
	for _, variable := range harness.CopilotRouteMovingEnvVars() {
		t.Run(variable, func(t *testing.T) {
			context, _ := copilotLaunchContext(t, map[string]string{
				variable: "https://example.invalid/v1",
			})
			_, err := ResolveTclaudeLayerModelTransport(h, context)
			var capErr *harness.SandboxCapabilityError
			if !errors.As(err, &capErr) {
				t.Fatalf("got %v, want a refusal for %s", err, variable)
			}
			if capErr.Kind != harness.SandboxCapabilityModelTransport {
				t.Errorf("Kind = %q, want %q", capErr.Kind, harness.SandboxCapabilityModelTransport)
			}
			if !strings.Contains(capErr.Message, variable) {
				t.Errorf("message %q does not name the variable an operator must remove",
					capErr.Message)
			}
		})
	}
}

// TestResolveCopilotModelTransportRefusesEnterpriseHostSelection is called out
// separately from the loop above because it is the specific posture that must
// NOT be resolved by widening the pack: the enterprise CAPI host exists in the
// shipped runtime, but how a launch selects it is not inspectable here, so the
// posture is refused rather than granted an extra destination.
func TestResolveCopilotModelTransportRefusesEnterpriseHostSelection(t *testing.T) {
	h := harness.MustGet(harness.CopilotName)
	for _, variable := range []string{"GH_HOST", "COPILOT_GH_HOST"} {
		context, _ := copilotLaunchContext(t, map[string]string{
			variable: "mycompany.ghe.com",
		})
		_, err := ResolveTclaudeLayerModelTransport(h, context)
		if err == nil {
			t.Fatalf("%s must refuse: the enterprise route is not resolvable at this seam", variable)
		}
	}
}

// TestResolveCopilotModelTransportRefusesSettingsRouteKeys covers the on-disk
// half of the same contract, including the ambiguity case: a routing key whose
// value is not a string is refused by NAME rather than read as unset.
func TestResolveCopilotModelTransportRefusesSettingsRouteKeys(t *testing.T) {
	h := harness.MustGet(harness.CopilotName)
	for _, tc := range []struct {
		name string
		body string
		key  string
	}{
		{name: "copilotUrl moves the CAPI endpoint",
			body: `{"copilotUrl":"https://copilot-api.example.ghe.com"}`, key: "copilotUrl"},
		{name: "proxyUrl hides the real destination",
			body: `{"proxyUrl":"http://proxy.corp.example:3128"}`, key: "proxyUrl"},
		{name: "a non-string routing value is ambiguous, not unset",
			body: `{"copilotUrl":{"host":"example.com"}}`, key: "copilotUrl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			context, home := copilotLaunchContext(t, nil)
			writeCopilotLaunchSettings(t, home, tc.body)
			_, err := ResolveTclaudeLayerModelTransport(h, context)
			var capErr *harness.SandboxCapabilityError
			if !errors.As(err, &capErr) {
				t.Fatalf("got %v, want a refusal", err)
			}
			if !strings.Contains(capErr.Message, tc.key) {
				t.Errorf("message %q does not name the settings key", capErr.Message)
			}
		})
	}
}

// TestResolveCopilotModelTransportRefusesUnreadableSettings keeps an
// unparsable file from being read as "no routing keys, so default route". The
// distinction matters because the refusal and the acceptance look identical
// from the outside: both produce no key name.
func TestResolveCopilotModelTransportRefusesUnreadableSettings(t *testing.T) {
	h := harness.MustGet(harness.CopilotName)
	context, home := copilotLaunchContext(t, nil)
	writeCopilotLaunchSettings(t, home, `{"copilotUrl": `)
	if _, err := ResolveTclaudeLayerModelTransport(h, context); err == nil {
		t.Fatal("unparsable Copilot settings must refuse rather than default to the first-party route")
	}
}

// TestResolveCopilotModelTransportProxyRegression is the guardrail the outer
// launch depends on, and it asserts BOTH directions:
//
//   - An INHERITED proxy variable refuses the launch. The real destination is
//     then behind a proxy this seam does not resolve, so the authored allow
//     list could not be checked against it.
//   - tclaude's OWN proxy-engine injection is not what this gate sees. The
//     launcher replaces the routing variables AFTER this preflight (see
//     proxyNetworkRoutingVariables), so a managed launch must not be refused
//     merely because tclaude will later set HTTP_PROXY itself.
func TestResolveCopilotModelTransportProxyRegression(t *testing.T) {
	h := harness.MustGet(harness.CopilotName)
	for _, variable := range []string{
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy",
	} {
		t.Run("inherited "+variable, func(t *testing.T) {
			context, _ := copilotLaunchContext(t, map[string]string{
				variable: "http://proxy.corp.example:3128",
			})
			_, err := ResolveTclaudeLayerModelTransport(h, context)
			if err == nil {
				t.Fatalf("an inherited %s must refuse: the destination is not resolvable behind it",
					variable)
			}
			if !strings.Contains(err.Error(), variable) {
				t.Errorf("message %q does not name the proxy variable", err.Error())
			}
		})
	}

	// The managed replacement is a launcher OUTPUT, produced after this gate
	// runs. Asserting the two lists here documents the ordering the guardrail
	// rests on: every variable tclaude injects is one this preflight refuses
	// when it arrives INHERITED, which is exactly why the injection has to
	// happen downstream of it.
	injected := make(map[string]bool)
	for _, entry := range ProxyNetworkCarriage("127.0.0.1:9") {
		if entry.Carriage != "" {
			injected[entry.Name] = true
		}
	}
	for _, variable := range proxyNetworkRoutingVariables {
		if !injected[variable] {
			t.Errorf("launcher routing variable %s is not part of the injected carriage; "+
				"the preflight/injection ordering this test documents no longer holds", variable)
		}
	}
	context, _ := copilotLaunchContext(t, nil)
	if _, err := ResolveTclaudeLayerModelTransport(h, context); err != nil {
		t.Fatalf("a launch with no INHERITED proxy must resolve, even though the launcher "+
			"will inject its own proxy variables afterwards: %v", err)
	}
}

// writeCopilotLaunchFile writes any file into the launch's Copilot home, so a
// case can describe the two-file world the route gate actually faces.
func writeCopilotLaunchFile(t *testing.T, home, name, body string) {
	t.Helper()
	dir := filepath.Join(home, ".copilot")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("prepare Copilot home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestResolveCopilotModelTransportReadsTheLegacyConfigRoute is the F2
// regression, and the bug it pins was invisible from inside this gate.
//
// The route check read settings.json alone. Copilot migrates the legacy
// config.json OVER settings.json at startup and the legacy file wins, so a
// `copilotUrl` living there was reported as "no routing keys, default route".
// The residual this closes is specific, and worth stating narrowly rather than
// as "a bypass": a filtered launch that runs tclaude's managed proxy engine is
// unaffected, because the injected proxy environment beats the on-disk value.
// A filtered launch WITHOUT that engine is the exposed one — there the on-disk
// `proxyUrl` is live and can tunnel model traffic through loopback or any
// already-allowed host, inside the wall and past the destination review the
// wall exists to perform. Pre-fix, that launch was admitted and then merely
// broke against the wall instead of being refused with a reason.
//
// The empty/replacement arms matter for the opposite reason: the merge is
// shallow at the top level, so a legacy file that sets a route key REPLACES the
// canonical one, and a legacy file that is silent leaves it alone. Getting that
// backwards would either miss an override or refuse a launch that has none.
func TestResolveCopilotModelTransportReadsTheLegacyConfigRoute(t *testing.T) {
	h := harness.MustGet(harness.CopilotName)
	for _, tc := range []struct {
		name       string
		settings   string
		config     string
		wantRefuse bool
		wantKey    string
		wantFile   string
	}{
		{
			// proxyUrl first because it is the MATERIAL one: measured on 1.0.77
			// it really does send model traffic somewhere the allow list never
			// approved. This exact shape — a legacy-only proxyUrl — is what the
			// pre-fix gate admitted.
			name:       "a live proxyUrl living only in the legacy config",
			config:     `{"proxyUrl":"http://proxy.corp.example:3128"}`,
			wantRefuse: true, wantKey: "proxyUrl", wantFile: harness.CopilotConfigFileName,
		},
		{
			// copilotUrl is refused conservatively rather than because it is
			// known to be live: it appears inert and undocumented at 1.0.77, but
			// it is named in the shipped runtime and would be the obvious lever
			// if wired up. A refusal costs one setting; a miss costs the wall
			// its meaning.
			name:       "a conservative copilotUrl refusal from the legacy config",
			config:     `{"copilotUrl":"https://copilot-api.example.ghe.com"}`,
			wantRefuse: true, wantKey: "copilotUrl", wantFile: harness.CopilotConfigFileName,
		},
		{
			name:       "the legacy config sets a route beside an unrelated canonical key",
			settings:   `{"theme":"dark"}`,
			config:     `{"proxyUrl":"http://proxy.corp.example:3128"}`,
			wantRefuse: true, wantKey: "proxyUrl", wantFile: harness.CopilotConfigFileName,
		},
		{
			name:       "the legacy config REPLACES the canonical route key",
			settings:   `{"copilotUrl":"https://one.example"}`,
			config:     `{"copilotUrl":"https://two.example"}`,
			wantRefuse: true, wantKey: "copilotUrl", wantFile: harness.CopilotConfigFileName,
		},
		{
			name: "a legacy config that CLEARS the route key leaves the default route",
			// Whole-key replacement, so an empty string in the legacy file
			// genuinely removes the override rather than merging under it.
			settings: `{"copilotUrl":"https://one.example"}`,
			config:   `{"copilotUrl":""}`,
		},
		{
			name:     "a legacy config silent about routing leaves the canonical value alone",
			settings: `{"theme":"dark"}`,
			config:   `{"banner":"never"}`,
		},
		{
			name:     "the managed post-migration stub is the ordinary posture",
			settings: `{"theme":"dark"}`,
			config: "// User settings belong in settings.json.\n" +
				"// This file is managed automatically.\n" +
				"{\n  \"firstLaunchAt\": \"2026-03-11T00:00:00.000Z\"\n}\n",
		},
		{
			name:       "an unparsable legacy config is refused, not read as absent",
			settings:   `{"theme":"dark"}`,
			config:     `{"copilotUrl": `,
			wantRefuse: true, wantFile: harness.CopilotConfigFileName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			context, home := copilotLaunchContext(t, nil)
			if tc.settings != "" {
				writeCopilotLaunchSettings(t, home, tc.settings)
			}
			writeCopilotLaunchFile(t, home, harness.CopilotConfigFileName, tc.config)

			_, err := ResolveTclaudeLayerModelTransport(h, context)
			if !tc.wantRefuse {
				if err != nil {
					t.Fatalf("got %v, want the first-party route accepted", err)
				}
				return
			}
			var capErr *harness.SandboxCapabilityError
			if !errors.As(err, &capErr) {
				t.Fatalf("got %v, want a refusal", err)
			}
			if tc.wantKey != "" && !strings.Contains(capErr.Message, tc.wantKey) {
				t.Errorf("message %q does not name the settings key", capErr.Message)
			}
			// The remedy has to name the file that actually decides. Pointing an
			// operator at settings.json when config.json overrides it is advice
			// that does not work.
			if !strings.Contains(capErr.Message, tc.wantFile) {
				t.Errorf("message %q does not name the deciding file %s",
					capErr.Message, tc.wantFile)
			}
		})
	}
}
