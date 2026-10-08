package db

import (
	"math"
	"time"
)

// FederationActivity is a metadata-only projection. Audit details, request
// payloads, sealed envelopes, model credentials and output never enter it.
// Audit rows are events; the other sources describe their current durable state.
type FederationActivity struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Source    string    `json:"source"`
	Direction string    `json:"direction"`
	Peer      string    `json:"peer,omitempty"`
	Kind      string    `json:"kind"`
	State     string    `json:"state,omitempty"`
	Actor     string    `json:"actor,omitempty"`
	Target    string    `json:"target,omitempty"`
	Group     string    `json:"group,omitempty"`
	Status    int       `json:"status,omitempty"`
}

// ListFederationActivity filters before limiting, so a busy unrelated peer
// cannot hide a selected peer's older rows. Sorting is stable across sources.
func ListFederationActivity(peer string, since time.Time, limit int) ([]FederationActivity, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	sinceAt := int64(math.MinInt64)
	if !since.IsZero() {
		if since.After(time.Unix(0, math.MaxInt64)) {
			return []FederationActivity{}, nil
		}
		if since.After(time.Unix(0, math.MinInt64)) {
			sinceAt = since.UnixNano()
		}
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`WITH peer_names AS (
 SELECT instance_id AS peer,instance_id AS name FROM federation_peers
 UNION SELECT instance_id,label FROM federation_peers WHERE label<>''
 UNION SELECT instance_id,name FROM federation_peers WHERE name<>''
 ), activity(id,at,source,direction,peer,kind,state,actor,target,grp,status) AS (
 SELECT 'audit:'||a.id,a.at,'audit','event',COALESCE((SELECT CASE WHEN COUNT(DISTINCT pn.peer)=1 THEN MIN(pn.peer) ELSE '' END FROM peer_names pn
 WHERE a.actor_label='remote:'||pn.name OR a.actor_label=pn.name
 OR a.target_label=pn.name OR a.target_label='remote:'||pn.name
 OR substr(a.actor_label,-length(pn.name)-1)='@'||pn.name
 OR substr(a.target_label,-length(pn.name)-1)='@'||pn.name
),''),a.verb,'',a.actor_label,COALESCE(NULLIF(a.target_agent,''),a.target_conv),a.group_name,a.status
 FROM audit_log a WHERE a.source='federation' OR a.verb LIKE 'federation.%'
 UNION ALL
 SELECT 'outbox:'||envelope_id,created_at,'outbox','out',to_instance,kind,state,
 COALESCE(NULLIF(from_agent,''),from_conv),COALESCE(NULLIF(to_agent,''),to_label),'',0 FROM federation_outbox
 UNION ALL
 SELECT 'mail:'||i.message_id,i.received_at,'inbound','in',i.from_instance,'mail',
 CASE WHEN m.read_at IS NOT NULL THEN 'read' WHEN m.delivered_at IS NOT NULL THEN 'delivered' ELSE 'pending' END,
 i.from_agent,COALESCE(NULLIF(m.to_agent,''),m.to_conv),COALESCE(g.name,''),0
 FROM federation_inbound i JOIN agent_messages m ON m.id=i.message_id LEFT JOIN agent_groups g ON g.id=m.group_id
 UNION ALL
 SELECT 'human-message:'||id,created_at,'operator_inbox','in',substr(group_name,12),'operator_message',CASE WHEN read_at IS NULL THEN 'unread' ELSE 'read' END,from_title,'','',0
 FROM human_messages WHERE substr(group_name,1,11)='federation:'
 UNION ALL
 SELECT 'spawn:'||id,created_at,'spawns','in',from_instance,'spawn',status,from_agent,result_agent,group_name,0 FROM federation_spawn_requests
 UNION ALL
 SELECT 'job:'||direction||':'||peer||':'||id,created_at,'jobs',direction,peer,'job',state,caller_agent,worker_id,'',0 FROM federation_jobs
 UNION ALL
 SELECT 'offer:'||o.direction||':'||o.peer||':'||o.id,o.created_at,'offers',o.direction,o.peer,o.kind||'_offer',o.state,o.sender_agent,o.import_agent,COALESCE(g.name,''),0
 FROM federation_bundle_offers o LEFT JOIN agent_groups g ON g.id=o.group_id
 UNION ALL
 SELECT 'teleport:'||direction||':'||peer||':'||offer,created_at,'teleports',direction,peer,'teleport',state,source_agent,target_agent,'',0 FROM federation_teleports
 UNION ALL
 SELECT 'route-mirror:'||m.route_id,m.created_at,'routes','out',m.peer,'route',r.state,r.publisher_agent_id,m.remote_route,COALESCE(g.name,''),0
 FROM federation_route_mirrors m JOIN agent_routes r ON r.id=m.route_id LEFT JOIN agent_groups g ON g.id=r.group_id
 UNION ALL
 SELECT 'route-proxy:'||p.lease_id,p.created_at,'routes','in',p.peer,'route',l.state,l.consumer_agent_id,p.route_id,COALESCE(g.name,''),0
 FROM federation_route_proxies p JOIN agent_route_leases l ON l.id=p.lease_id JOIN agent_routes r ON r.id=p.route_id LEFT JOIN agent_groups g ON g.id=r.group_id
 UNION ALL
 SELECT 'model-lease:'||id,touched_at,'model_leases','out',peer,'model_lease',
 CASE WHEN revoked=1 THEN 'revoked' WHEN touched_at+idle_seconds*1000000000<=? THEN 'idle_expired' WHEN worker='' THEN 'pending' ELSE 'active' END,
 '',worker,'',0 FROM model_proxy_leases
 UNION ALL
 SELECT 'model-request:'||id,started_at,'model_requests','in',peer,'model_request',CASE WHEN complete=1 THEN 'complete' ELSE 'incomplete' END,
 '',session,'',status FROM model_proxy_requests
 ) SELECT id,at,source,direction,peer,kind,state,actor,target,grp,status FROM activity
 WHERE (?='' OR peer=?) AND at>=? ORDER BY at DESC,id DESC LIMIT ?`, dbTime(time.Now()), peer, peer, sinceAt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationActivity{}
	for rows.Next() {
		var r FederationActivity
		var at dbTimestamp
		if err = rows.Scan(&r.ID, &at, &r.Source, &r.Direction, &r.Peer, &r.Kind, &r.State, &r.Actor, &r.Target, &r.Group, &r.Status); err != nil {
			return nil, err
		}
		r.At = at.Time()
		out = append(out, r)
	}
	return out, rows.Err()
}
