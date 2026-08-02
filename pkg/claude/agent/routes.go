package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// The routes CLI is intentionally a thin client of the versioned daemon API.
// Route authority belongs to agentd, so this package never reads or mutates
// route rows directly.
const (
	maxRouteCLINameBytes  = 128
	maxRouteCLIRefBytes   = 512
	maxRouteCLIGroupBytes = 256
)

type routeCLI struct {
	APIVersion                string `json:"api_version"`
	ID                        string `json:"id"`
	GroupID                   int64  `json:"group_id"`
	Group                     string `json:"group"`
	PublisherAgentID          string `json:"publisher_agent_id"`
	PublisherConvID           string `json:"publisher_conv_id,omitempty"`
	PublisherLaunchGeneration string `json:"publisher_launch_generation"`
	GroupGeneration           int64  `json:"group_generation"`
	Name                      string `json:"name"`
	Reference                 string `json:"reference,omitempty"`
	StableReference           string `json:"stable_reference,omitempty"`
	Transport                 string `json:"transport"`
	Target                    string `json:"target"`
	State                     string `json:"state"`
	CreatedAt                 string `json:"created_at"`
	WithdrawnAt               string `json:"withdrawn_at,omitempty"`
	WithdrawReason            string `json:"withdraw_reason,omitempty"`
}

type routeLeaseCLI struct {
	APIVersion               string `json:"api_version"`
	ID                       string `json:"id"`
	RouteID                  string `json:"route_id"`
	RouteReference           string `json:"route_reference,omitempty"`
	ConsumerAgentID          string `json:"consumer_agent_id"`
	ConsumerConvID           string `json:"consumer_conv_id,omitempty"`
	ConsumerLaunchGeneration string `json:"consumer_launch_generation"`
	GroupGeneration          int64  `json:"group_generation"`
	State                    string `json:"state"`
	OpenedAt                 string `json:"opened_at"`
	ClosedAt                 string `json:"closed_at,omitempty"`
	Endpoint                 string `json:"endpoint,omitempty"`
}

type routeListCLI struct {
	APIVersion      string          `json:"api_version"`
	GroupID         int64           `json:"group_id,omitempty"`
	Group           string          `json:"group,omitempty"`
	GroupGeneration int64           `json:"group_generation,omitempty"`
	Routes          []routeCLI      `json:"routes"`
	Groups          []routeGroupCLI `json:"groups,omitempty"`
}

type routeGroupCLI struct {
	GroupID         int64      `json:"group_id"`
	Group           string     `json:"group"`
	GroupGeneration int64      `json:"group_generation"`
	Routes          []routeCLI `json:"routes"`
}

func routesCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "routes",
		Short:       "Publish and consume named group routes",
		Long:        "Expose named opaque TCP endpoints to a selected group. Route authority and permissions are enforced by agentd; consumers receive a local endpoint when the platform adapter is ready.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			routesPublishCmd(),
			routesOpenCmd(),
			routesLsCmd(),
			routesCloseCmd(),
		},
	}.ToCobra()
}

type routesPublishParams struct {
	Name      string `pos:"true" help:"Route name (1-128 bytes; slash and control characters are not allowed)"`
	Group     string `long:"group" short:"g" help:"Explicit target group name or numeric ID"`
	Target    string `long:"target" help:"Publisher-local TCP endpoint (for example tcp://127.0.0.1:43127)"`
	Transport string `long:"transport" optional:"true" help:"Route transport (currently tcp)"`
	JSON      bool   `long:"json" help:"Output stable JSON"`
}

func routesPublishCmd() *cobra.Command {
	return boa.CmdT[routesPublishParams]{
		Use:         "publish <name>",
		Short:       "Publish a named endpoint to a group",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *routesPublishParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *routesPublishParams, _ *cobra.Command, _ []string) {
			os.Exit(runRoutesPublish(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRoutesPublish(p *routesPublishParams, stdout, stderr io.Writer) int {
	name, group, target, rc := validateRoutePublishCLI(p, stderr)
	if rc != rcOK {
		return rc
	}
	if rc = RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	payload := map[string]any{"name": name, "group": group, "target": target}
	if transport := strings.TrimSpace(p.Transport); transport != "" {
		payload["transport"] = transport
	}
	var route routeCLI
	if err := DaemonRequest(http.MethodPost, "/v1/routes/publish", payload, &route, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeRouteJSON(stdout, route)
	}
	ref := route.Reference
	if ref == "" {
		ref = route.Name
	}
	fmt.Fprintf(stdout, "Published route %s (id %s) in group %s.\n", ref, route.ID, route.Group)
	return rcOK
}

func validateRoutePublishCLI(p *routesPublishParams, stderr io.Writer) (name, group, target string, rc int) {
	name = strings.TrimSpace(p.Name)
	group = strings.TrimSpace(p.Group)
	target = strings.TrimSpace(p.Target)
	if err := validateRouteCLIText(name, "route name", maxRouteCLINameBytes, false); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return "", "", "", rcInvalidArg
	}
	if strings.ContainsRune(name, '/') {
		fmt.Fprintln(stderr, "Error: route name must not contain '/'")
		return "", "", "", rcInvalidArg
	}
	if err := validateRouteCLIText(group, "group", maxRouteCLIGroupBytes, false); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return "", "", "", rcInvalidArg
	}
	if target == "" {
		fmt.Fprintln(stderr, "Error: --target is required")
		return "", "", "", rcInvalidArg
	}
	return name, group, target, rcOK
}

type routesOpenParams struct {
	Reference string `pos:"true" help:"Route ID or message-friendly reference (<publisher>/<name> or <group-id>/<publisher>/<name>)"`
	Group     string `long:"group" short:"g" help:"Explicit target group name or numeric ID"`
	JSON      bool   `long:"json" help:"Output stable JSON"`
}

func routesOpenCmd() *cobra.Command {
	return boa.CmdT[routesOpenParams]{
		Use:         "open <route>",
		Short:       "Open a route and receive a consumer-local endpoint",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *routesOpenParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *routesOpenParams, _ *cobra.Command, _ []string) {
			os.Exit(runRoutesOpen(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRoutesOpen(p *routesOpenParams, stdout, stderr io.Writer) int {
	ref := strings.TrimSpace(p.Reference)
	group := strings.TrimSpace(p.Group)
	if err := validateRouteCLIText(ref, "route reference", maxRouteCLIRefBytes, false); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if err := validateRouteCLIText(group, "group", maxRouteCLIGroupBytes, false); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	routeID, route, rc := resolveRouteCLIReference(ref, group, stderr)
	if rc != rcOK {
		return rc
	}
	var lease routeLeaseCLI
	payload := map[string]any{"route_id": routeID, "group": group}
	if err := DaemonRequest(http.MethodPost, "/v1/routes/open", payload, &lease, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if lease.RouteReference == "" {
		lease.RouteReference = route.Reference
	}
	if p.JSON {
		return writeRouteJSON(stdout, lease)
	}
	friendly := lease.RouteReference
	if friendly == "" {
		friendly = ref
	}
	fmt.Fprintf(stdout, "Opened route %s (lease %s).\n", friendly, lease.ID)
	if lease.Endpoint != "" {
		fmt.Fprintf(stdout, "Consumer endpoint: %s\n", lease.Endpoint)
	} else {
		fmt.Fprintln(stdout, "Consumer endpoint: pending platform adapter allocation")
	}
	return rcOK
}

type routesLsParams struct {
	Group string `long:"group" short:"g" optional:"true" help:"List routes for this explicit group; omit only when the caller has one unambiguous group"`
	JSON  bool   `long:"json" help:"Output stable JSON"`
}

func routesLsCmd() *cobra.Command {
	return boa.CmdT[routesLsParams]{
		Use:         "ls",
		Aliases:     []string{"list"},
		Short:       "List named routes",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *routesLsParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *routesLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runRoutesLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRoutesLs(p *routesLsParams, stdout, stderr io.Writer) int {
	group := strings.TrimSpace(p.Group)
	if err := validateRouteCLIText(group, "group", maxRouteCLIGroupBytes, true); err != nil && group != "" {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp routeListCLI
	if err := DaemonRequest(http.MethodGet, routeListPath(group), nil, &resp, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeRouteJSON(stdout, resp)
	}
	if len(resp.Groups) > 0 {
		for _, nested := range resp.Groups {
			printRouteRows(stdout, nested.Group, nested.Routes)
		}
		return rcOK
	}
	printRouteRows(stdout, resp.Group, resp.Routes)
	return rcOK
}

func printRouteRows(stdout io.Writer, group string, routes []routeCLI) {
	if len(routes) == 0 {
		if group == "" {
			fmt.Fprintln(stdout, "(no routes)")
		} else {
			fmt.Fprintf(stdout, "(no routes in group %s)\n", group)
		}
		return
	}
	fmt.Fprintln(stdout, "REFERENCE\tSTATE\tTRANSPORT\tGROUP")
	for _, route := range routes {
		ref := route.Reference
		if ref == "" {
			ref = route.ID
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", ref, route.State, route.Transport, route.Group)
	}
}

type routesCloseParams struct {
	Reference string `pos:"true" help:"Lease ID, route ID, or message-friendly route reference"`
	Group     string `long:"group" short:"g" optional:"true" help:"Explicit target group when closing a route reference"`
	JSON      bool   `long:"json" help:"Output stable JSON"`
}

func routesCloseCmd() *cobra.Command {
	return boa.CmdT[routesCloseParams]{
		Use:         "close <route>",
		Short:       "Close a published route or consumer lease",
		Long:        "Closes the caller's own route or consumer lease. Pass the lease ID printed by routes open, or a route reference together with --group. The daemon decides whether the caller owns the published route or an open consumer lease.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *routesCloseParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.Group).SetAlternativesFunc(completeGroupNames)
			return nil
		},
		RunFunc: func(p *routesCloseParams, _ *cobra.Command, _ []string) {
			os.Exit(runRoutesClose(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRoutesClose(p *routesCloseParams, stdout, stderr io.Writer) int {
	ref := strings.TrimSpace(p.Reference)
	group := strings.TrimSpace(p.Group)
	if err := validateRouteCLIText(ref, "route reference", maxRouteCLIRefBytes, false); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if err := validateRouteCLIText(group, "group", maxRouteCLIGroupBytes, false); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	leaseID := ref
	var route routeCLI
	if !strings.HasPrefix(ref, "rlease_") {
		var rc int
		if strings.HasPrefix(ref, "rte_") {
			_, route, rc = resolveRouteCLIReference(ref, group, stderr)
		} else {
			if group == "" {
				fmt.Fprintln(stderr, "Error: --group is required when closing a route reference")
				return rcInvalidArg
			}
			_, route, rc = resolveRouteCLIReference(ref, group, stderr)
		}
		if rc != rcOK {
			return rc
		}
		var withdrawn routeCLI
		if err := DaemonRequest(http.MethodDelete, "/v1/routes/"+url.PathEscape(route.ID), nil, &withdrawn, DaemonOpts{}); err == nil {
			if p.JSON {
				return writeRouteJSON(stdout, withdrawn)
			}
			friendly := withdrawn.Reference
			if friendly == "" {
				friendly = route.Reference
			}
			fmt.Fprintf(stdout, "Closed published route %s.\n", friendly)
			return rcOK
		} else if !routeCloseMayFallBackToLease(err) {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		if group == "" {
			group = route.Group
		}
		var leases struct {
			Leases []routeLeaseCLI `json:"leases"`
		}
		if err := DaemonRequest(http.MethodGet, routeLeasesPath(group), nil, &leases, DaemonOpts{}); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		matches := make([]routeLeaseCLI, 0, 1)
		for _, lease := range leases.Leases {
			if lease.RouteID == route.ID && lease.State == "open" {
				matches = append(matches, lease)
			}
		}
		if len(matches) == 0 {
			fmt.Fprintf(stderr, "Error: route %s has no open consumer lease\n", ref)
			return rcNotFound
		}
		if len(matches) > 1 {
			fmt.Fprintf(stderr, "Error: route %s has multiple open leases; close by lease ID\n", ref)
			return rcAmbiguous
		}
		leaseID = matches[0].ID
	}
	var lease routeLeaseCLI
	if err := DaemonRequest(http.MethodDelete, "/v1/routes/leases/"+url.PathEscape(leaseID), nil, &lease, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeRouteJSON(stdout, lease)
	}
	fmt.Fprintf(stdout, "Closed route lease %s.\n", lease.ID)
	return rcOK
}

func routeCloseMayFallBackToLease(err error) bool {
	de, ok := err.(*DaemonError)
	if !ok {
		return false
	}
	return de.Code == "route_not_owner" || de.Code == "route_permission"
}

func routeListPath(group string) string {
	if group == "" {
		return "/v1/routes"
	}
	return "/v1/routes?group=" + url.QueryEscape(group)
}

func routeLeasesPath(group string) string {
	return "/v1/routes/leases?group=" + url.QueryEscape(group)
}

func resolveRouteCLIReference(ref, group string, stderr io.Writer) (string, routeCLI, int) {
	if strings.HasPrefix(ref, "rte_") {
		var route routeCLI
		path := "/v1/routes/" + url.PathEscape(ref)
		if err := DaemonRequest(http.MethodGet, path, nil, &route, DaemonOpts{}); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return "", routeCLI{}, MapDaemonErrorToRC(err)
		}
		if group != "" && route.Group != "" && route.Group != group && fmt.Sprint(route.GroupID) != group {
			fmt.Fprintf(stderr, "Error: route %s is not in group %s\n", ref, group)
			return "", routeCLI{}, rcInvalidArg
		}
		return route.ID, route, rcOK
	}
	var list routeListCLI
	if err := DaemonRequest(http.MethodGet, routeListPath(group), nil, &list, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return "", routeCLI{}, MapDaemonErrorToRC(err)
	}
	routes := list.Routes
	for _, nested := range list.Groups {
		routes = append(routes, nested.Routes...)
	}
	matches := make([]routeCLI, 0, 1)
	for _, route := range routes {
		if routeReferenceMatches(ref, route) {
			matches = append(matches, route)
		}
	}
	if len(matches) == 0 {
		fmt.Fprintf(stderr, "Error: no route matches %s\n", ref)
		return "", routeCLI{}, rcNotFound
	}
	if len(matches) > 1 {
		fmt.Fprintf(stderr, "Error: route reference %s is ambiguous; pass a stable route ID or explicit group\n", ref)
		return "", routeCLI{}, rcAmbiguous
	}
	return matches[0].ID, matches[0], rcOK
}

func routeReferenceMatches(ref string, route routeCLI) bool {
	return ref == route.ID || ref == route.Reference || ref == route.StableReference ||
		ref == route.PublisherAgentID+"/"+route.Name ||
		ref == fmt.Sprintf("%d/%s/%s", route.GroupID, route.PublisherAgentID, route.Name)
}

func validateRouteCLIText(value, label string, maxBytes int, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("%s is required", label)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s is too long (maximum %d bytes)", label, maxBytes)
	}
	if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains an invalid control character", label)
	}
	return nil
}

func writeRouteJSON(stdout io.Writer, value any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		return rcIOFailure
	}
	return rcOK
}
