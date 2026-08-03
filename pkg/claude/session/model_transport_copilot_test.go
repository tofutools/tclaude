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
