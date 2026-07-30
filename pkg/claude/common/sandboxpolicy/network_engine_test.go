package sandboxpolicy

import (
	"reflect"
	"testing"
)

func TestResolveNetworkEngineMostExplicitWins(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selections []NetworkEngineSelection
		want       ResolvedNetworkEngine
	}{
		{
			name: "no layer names an engine",
			selections: []NetworkEngineSelection{
				{Layer: NetworkEngineLayerGlobal},
				{Layer: NetworkEngineLayerSession},
			},
			want: ResolvedNetworkEngine{},
		},
		{
			name: "only global names one",
			selections: []NetworkEngineSelection{
				{Layer: NetworkEngineLayerSession},
				{Layer: NetworkEngineLayerGlobal, Engine: NetworkEngineProxy,
					Source: "global"},
			},
			want: ResolvedNetworkEngine{
				Engine: NetworkEngineProxy,
				Layer:  NetworkEngineLayerGlobal,
				Source: "global",
			},
		},
		{
			name: "session overrides group and global",
			selections: []NetworkEngineSelection{
				{Layer: NetworkEngineLayerGlobal, Engine: NetworkEnginePacket,
					Source: "global"},
				{Layer: NetworkEngineLayerGroup, Engine: NetworkEnginePacket,
					Source: "frontend-team"},
				{Layer: NetworkEngineLayerSession, Engine: NetworkEngineProxy,
					Source: "session"},
			},
			want: ResolvedNetworkEngine{
				Engine: NetworkEngineProxy,
				Layer:  NetworkEngineLayerSession,
				Source: "session",
				Overridden: []NetworkEngineSelection{
					{Layer: NetworkEngineLayerGroup, Engine: NetworkEnginePacket,
						Source: "frontend-team"},
					{Layer: NetworkEngineLayerGlobal, Engine: NetworkEnginePacket,
						Source: "global"},
				},
			},
		},
		{
			name: "group wins when session is silent",
			selections: []NetworkEngineSelection{
				{Layer: NetworkEngineLayerGlobal, Engine: NetworkEnginePacket,
					Source: "global"},
				{Layer: NetworkEngineLayerGroup, Engine: NetworkEngineProxy,
					Source: "frontend-team"},
				{Layer: NetworkEngineLayerSession},
			},
			want: ResolvedNetworkEngine{
				Engine: NetworkEngineProxy,
				Layer:  NetworkEngineLayerGroup,
				Source: "frontend-team",
				Overridden: []NetworkEngineSelection{
					{Layer: NetworkEngineLayerGlobal, Engine: NetworkEnginePacket,
						Source: "global"},
				},
			},
		},
		{
			name: "agreement is not an override",
			selections: []NetworkEngineSelection{
				{Layer: NetworkEngineLayerGlobal, Engine: NetworkEngineProxy,
					Source: "global"},
				{Layer: NetworkEngineLayerSession, Engine: NetworkEngineProxy,
					Source: "session"},
			},
			want: ResolvedNetworkEngine{
				Engine: NetworkEngineProxy,
				Layer:  NetworkEngineLayerSession,
				Source: "session",
			},
		},
		{
			name: "precedence follows the layer, not the argument order",
			selections: []NetworkEngineSelection{
				{Layer: NetworkEngineLayerSession, Engine: NetworkEnginePacket,
					Source: "session"},
				{Layer: NetworkEngineLayerGlobal, Engine: NetworkEngineProxy,
					Source: "global"},
			},
			want: ResolvedNetworkEngine{
				Engine: NetworkEnginePacket,
				Layer:  NetworkEngineLayerSession,
				Source: "session",
				Overridden: []NetworkEngineSelection{
					{Layer: NetworkEngineLayerGlobal, Engine: NetworkEngineProxy,
						Source: "global"},
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveNetworkEngine(tc.selections)
			if err != nil {
				t.Fatalf("ResolveNetworkEngine: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("resolved %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestResolveNetworkEngineRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selections []NetworkEngineSelection
	}{
		{
			name: "unknown layer",
			selections: []NetworkEngineSelection{
				{Layer: "tenant", Engine: NetworkEngineProxy},
			},
		},
		{
			name: "duplicate layer",
			selections: []NetworkEngineSelection{
				{Layer: NetworkEngineLayerGroup, Engine: NetworkEngineProxy},
				{Layer: NetworkEngineLayerGroup, Engine: NetworkEnginePacket},
			},
		},
		{
			name: "unknown engine",
			selections: []NetworkEngineSelection{
				{Layer: NetworkEngineLayerSession, Engine: "socks"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveNetworkEngine(tc.selections); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

func TestNetworkRulesAreDiscriminating(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules NetworkRules
		want  bool
	}{
		{
			name:  "unset posture expresses no opinion",
			rules: NetworkRules{},
			want:  false,
		},
		{
			name:  "allow-all with no denies has nothing to filter",
			rules: NetworkRules{Mode: AccessModeOpen},
			want:  false,
		},
		{
			name: "deny-only under an allow baseline is discriminating",
			rules: NetworkRules{
				Mode: AccessModeOpen,
				Deny: []NetworkAllowEntry{{Domain: "tracker.example"}},
			},
			want: true,
		},
		{
			name:  "closed authorizes nothing",
			rules: NetworkRules{Mode: AccessModeClosed},
			want:  false,
		},
		{
			name: "closed with denies still authorizes nothing",
			rules: NetworkRules{
				Mode: AccessModeClosed,
				Deny: []NetworkAllowEntry{{Domain: "tracker.example"}},
			},
			want: false,
		},
		{
			name:  "an empty list allows nothing to filter",
			rules: NetworkRules{Mode: AccessModeList},
			want:  false,
		},
		{
			name: "an empty list with denies still allows nothing",
			rules: NetworkRules{
				Mode: AccessModeList,
				Deny: []NetworkAllowEntry{{Domain: "tracker.example"}},
			},
			want: false,
		},
		{
			name: "a loopback-only list is expressed by the floor",
			rules: NetworkRules{
				Mode:  AccessModeList,
				Allow: []NetworkAllowEntry{{Loopback: true, Ports: []int{8080}}},
			},
			want: false,
		},
		{
			name: "loopback plus one domain is discriminating",
			rules: NetworkRules{
				Mode: AccessModeList,
				Allow: []NetworkAllowEntry{
					{Loopback: true},
					{Domain: "example.com", IncludeSubdomains: true},
				},
			},
			want: true,
		},
		{
			name: "a cidr list is discriminating",
			rules: NetworkRules{
				Mode:  AccessModeList,
				Allow: []NetworkAllowEntry{{CIDR: "10.20.0.0/16", Ports: []int{5432}}},
			},
			want: true,
		},
		{
			name: "a loopback-only list with a deny is discriminating",
			rules: NetworkRules{
				Mode:  AccessModeList,
				Allow: []NetworkAllowEntry{{Loopback: true}},
				Deny:  []NetworkAllowEntry{{Loopback: true, Ports: []int{22}}},
			},
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NetworkRulesAreDiscriminating(tc.rules)
			if err != nil {
				t.Fatalf("NetworkRulesAreDiscriminating: %v", err)
			}
			if got != tc.want {
				t.Fatalf("discriminating = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNetworkRulesAreDiscriminatingRequiresMaterializedIntent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules NetworkRules
	}{
		{
			name:  "unresolved baseline",
			rules: NetworkRules{Baseline: NetworkBaselineDeny},
		},
		{
			name:  "unexpanded allow pack",
			rules: NetworkRules{Mode: AccessModeList, Packs: []string{"github"}},
		},
		{
			name:  "unexpanded deny pack",
			rules: NetworkRules{Mode: AccessModeOpen, DenyPacks: []string{"github"}},
		},
		{
			name:  "invalid mode",
			rules: NetworkRules{Mode: "proxy"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NetworkRulesAreDiscriminating(tc.rules)
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if got {
				t.Fatal("an erroring predicate must not report discrimination")
			}
		})
	}
}
