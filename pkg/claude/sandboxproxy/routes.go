package sandboxproxy

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
)

// RouteIdentity is the consumer-side stable identity the route authority must
// check for every synthetic destination. Display names are intentionally absent:
// group and agent IDs, conversation and launch generations, and the M1 lease
// are the authority boundary.
type RouteIdentity struct {
	GroupID          int64
	GroupGeneration  int64
	AgentID          string
	ConvID           string
	LaunchGeneration string
	LeaseID          string
}

// RouteAuth is a short compatibility spelling for callers that use "auth" for
// the generation-bound consumer identity.
type RouteAuth = RouteIdentity

func (i RouteIdentity) valid() bool {
	return i.GroupID > 0 && i.GroupGeneration > 0 &&
		strings.TrimSpace(i.AgentID) != "" &&
		strings.TrimSpace(i.ConvID) != "" &&
		strings.TrimSpace(i.LaunchGeneration) != "" &&
		strings.TrimSpace(i.LeaseID) != ""
}

// RouteRequest is one route connection request. The route ID comes from the
// opaque synthetic hostname; Port remains the client-requested port for the
// authority to compare with the route's published target contract.
type RouteRequest struct {
	Identity RouteIdentity
	RouteID  string
	Port     int
}

// RouteResolution is the authority's already-resolved local endpoint. Endpoint
// is deliberately an IP literal rather than a hostname: route connections do
// not enter DNS, ambient proxy discovery, or the Internet policy evaluator.
// The publisher fields carry the stable route owner identity through the
// authority seam for audit and stale-generation checks.
type RouteResolution struct {
	RouteID                   string
	GroupID                   int64
	GroupGeneration           int64
	PublisherAgentID          string
	PublisherConvID           string
	PublisherLaunchGeneration string
	Endpoint                  netip.AddrPort
	// TargetPort is the published route target's port. A zero value leaves
	// port matching to the authority for compatibility with route contracts
	// whose target port is not separately exposed.
	TargetPort int
}

// RouteResolver is the M1 authority seam for named route destinations. It
// must refuse cross-group, stale, withdrawn, and closed-lease requests. It
// returns only a generation-checked local endpoint; it never receives or
// returns carried payload bytes.
type RouteResolver interface {
	ResolveRoute(context.Context, RouteRequest) (RouteResolution, error)
}

// RouteAuthority is an explicit spelling for RouteResolver used by callers
// that model the resolver as the M1 authority registry.
type RouteAuthority = RouteResolver

func validateRouteResolution(request RouteRequest, resolution RouteResolution) error {
	if strings.TrimSpace(request.RouteID) == "" ||
		resolution.RouteID != request.RouteID {
		return fmt.Errorf("route authority returned a mismatched route identity")
	}
	if resolution.GroupID != request.Identity.GroupID ||
		resolution.GroupGeneration != request.Identity.GroupGeneration {
		return fmt.Errorf("route authority returned a stale group identity")
	}
	if strings.TrimSpace(resolution.PublisherAgentID) == "" ||
		strings.TrimSpace(resolution.PublisherConvID) == "" ||
		strings.TrimSpace(resolution.PublisherLaunchGeneration) == "" {
		return fmt.Errorf("route authority returned an incomplete publisher identity")
	}
	if resolution.TargetPort < 0 || resolution.TargetPort > 65535 {
		return fmt.Errorf("route authority returned an invalid target port")
	}
	if resolution.TargetPort != 0 && resolution.TargetPort != request.Port {
		return fmt.Errorf("route request port does not match the published route")
	}
	endpoint := resolution.Endpoint
	if !endpoint.IsValid() || endpoint.Addr().Zone() != "" ||
		!endpoint.Addr().IsLoopback() || endpoint.Addr().IsUnspecified() {
		return fmt.Errorf("route authority returned a non-loopback endpoint")
	}
	return nil
}
