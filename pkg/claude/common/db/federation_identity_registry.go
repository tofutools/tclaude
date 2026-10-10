package db

// IdentityColumn classifies every persisted peer reference. Only the rebind
// rules transfer authority; historical rows retain their original provenance.
// Close rules terminate capabilities before the predecessor loses trust.
type IdentityColumn struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Rule   string `json:"rule"`
}

const (
	IdentityRebind     = "rebind"
	IdentityHistorical = "keep-historical"
	IdentityClose      = "close/revoke"
)

var FederationIdentityColumns = []IdentityColumn{
	{"federation_peers", "instance_id", IdentityRebind},
	{"federation_catalogs", "peer", IdentityClose},
	{"federation_outbox", "to_instance", IdentityClose},
	{"federation_inbound", "from_instance", IdentityHistorical},
	{"federation_seen", "from_instance", IdentityHistorical},
	{"federation_spawn_requests", "from_instance", IdentityClose},
	{"federation_route_mirrors", "peer", IdentityClose},
	{"federation_route_proxies", "peer", IdentityClose},
	{"federation_peer_grants", "peer", IdentityRebind},
	{"federation_peer_access_requests", "peer", IdentityRebind},
	{"federation_auto_workers", "peer", IdentityRebind},
	{"federation_bundle_offers", "peer", IdentityClose},
	{"federation_agent_moves", "peer", IdentityRebind},
	{"federation_node_group_members", "peer", IdentityRebind},
	{"federation_node_profile_assignments", "peer", IdentityRebind},
	{"federation_enrollments", "peer", IdentityHistorical},
	{"federation_jobs", "peer", IdentityClose},
	{"federation_teleports", "peer", IdentityRebind},
	{"model_proxy_requests", "peer", IdentityHistorical},
	{"federation_teleport_leases", "peer", IdentityRebind},
	{"model_proxy_leases", "peer", IdentityClose},
	{"model_proxy_worker_leases", "gateway", IdentityClose},
	{"federation_identity_rotations", "old_instance", IdentityHistorical},
	{"federation_identity_rotations", "new_instance", IdentityHistorical},
	// Structured and embedded references also need an explicit rule. Frozen
	// request bodies and origin provenance are never recursively rewritten.
	{"human_messages", "group_name", IdentityHistorical},
	{"federation_jobs", "request", IdentityHistorical},
	{"federation_jobs", "result", IdentityHistorical},
	{"federation_bundle_offers", "descriptor", IdentityClose},
	{"federation_enrollments", "public_token", IdentityHistorical},
	{"agent_permissions", "scope_json", IdentityRebind},
	{"agent_group_permissions", "scope_json", IdentityRebind},
	{"agent_sudo_grants", "scope_json", IdentityClose},
	{"agent_groups", "owner_scopes_json", IdentityRebind},
	{"group_templates", "owner_scopes_json", IdentityHistorical},
	{"model_proxy_launches", "reference", IdentityClose},
	{"federation_node_profile_assignments", "snapshot", IdentityHistorical},
	{"federation_teleport_leases", "snapshot", IdentityRebind},
	{"federation_agent_moves", "payload", IdentityRebind},
	{"federation_teleports", "snapshot", IdentityRebind},
	{"federation_worker_defaults", "snapshot", IdentityHistorical},
	{"federation_node_profiles", "definition", IdentityHistorical},
}
