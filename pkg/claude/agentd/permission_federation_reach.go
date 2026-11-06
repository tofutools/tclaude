package agentd

import (
	"fmt"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Federation reach: what a trusted federation peer can be granted for each
// registered slug. Every slug declares it here, and the peer slug tables
// (federationPeerSlugs, fedInstancePeerSlugs, the model gateway slugs) are
// derived from it, so a slug cannot gain or lose peer authority without a
// change to this table. See docs/federation-peers.md for the two grant
// stores: peer grants on the receiving node, and an agent's peer= grants on
// the sending node.

// FedReachKind says how a peer reaches a slug's operation.
type FedReachKind string

const (
	// FedPeerGroup is a peer grant, scoped to one local group or unscoped
	// (every group, including future ones). Cap names its wire capability in
	// the signed catalog.
	FedPeerGroup FedReachKind = "peer_group"
	// FedPeerInstance is an instance-wide peer grant that refuses a scope.
	FedPeerInstance FedReachKind = "peer_instance"
	// FedPeerProxy is a model gateway peer grant, scoped http_proxy=<name>.
	FedPeerProxy FedReachKind = "peer_proxy"
	// FedPeerAlias is a local gate a peer satisfies through another peer
	// grant (Via): a family grant such as groups.settings, or a global
	// agent.* slug's group-scoped sibling.
	FedPeerAlias FedReachKind = "peer_alias"
	// FedSender is a slug that only an agent holds, with a peer= scope, to
	// act on a peer; the receiving peer decides by its own grant (Via).
	FedSender FedReachKind = "sender"
	// FedLocal is never reached by a peer: the operation cannot be remote
	// (Why says why).
	FedLocal FedReachKind = "local"
)

// FedReach is a slug's federation reach.
type FedReach struct {
	Kind FedReachKind
	// Cap is a FedPeerGroup slug's wire capability in the signed catalog.
	Cap string
	// Via names the peer grants a FedPeerAlias or FedSender slug goes through.
	Via []string
	// Pending marks a peer grant no peer route serves yet: `federation grant`
	// refuses the slug, and it is left out of the derived peer slug tables
	// until a route is opened for it. Unrestricted trust may reach the
	// operation, and a restricted peer may still pass its gate inside
	// another granted operation (see peerActionPassThroughs).
	Pending bool
	// SelfGate marks a group peer grant that also answers its own local gate
	// on a peer-invoked group route (peerGrantGates); a grant otherwise
	// answers only the gates whose reach goes via it.
	SelfGate bool
	// RequiresGroup marks a group peer grant that refuses to be unscoped.
	RequiresGroup bool
	// Sensitive marks a peer grant that lets the peer act on this node, not
	// only read. The dashboard's grant picker shows it (PEER_SLUGS in
	// dashboard/js/fleet-admin-model.js, which must agree).
	Sensitive bool
	// Why says why a FedLocal slug is never remote.
	Why string
}

// peerGrantable reports a peer grant a peer can be given today.
func (f FedReach) peerGrantable() bool {
	switch f.Kind {
	case FedPeerGroup, FedPeerInstance, FedPeerProxy:
		return !f.Pending
	}
	return false
}

var slugFederationReach = map[string]FedReach{
	PermJobsRun:                           {Kind: FedPeerGroup, Cap: proto.CapJobs, Sensitive: true},
	PermDashboardViewRead:                 {Kind: FedLocal, Why: "points at the local operator's own open dashboard"},
	PermDashboardGuide:                    {Kind: FedLocal, Why: "points at the local operator's own open dashboard"},
	PermDashboardCapture:                  {Kind: FedLocal, Why: "points at the local operator's own open dashboard"},
	PermDashboardFormsCapture:             {Kind: FedLocal, Why: "points at the local operator's own open dashboard"},
	PermDashboardFormsFill:                {Kind: FedLocal, Why: "points at the local operator's own open dashboard"},
	PermDashboardMark:                     {Kind: FedLocal, Why: "points at the local operator's own open dashboard"},
	PermFleetRead:                         {Kind: FedPeerInstance},
	PermKnowledgeRead:                     {Kind: FedLocal, Why: "the docs and source of the build answering; a peer's agents read their own"},
	PermBoardsRead:                        {Kind: FedPeerInstance, Pending: true},
	PermBoardsWrite:                       {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermHostRead:                          {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermHumanInboxRead:                    {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermHumanInboxAnswer:                  {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermApprovalsAnswer:                   {Kind: FedPeerInstance, Sensitive: true},
	PermModelsProxyLeased:                 {Kind: FedPeerProxy},
	PermSelfTeleportPermissions:           {Kind: FedSender, Via: []string{PermAgentsReceivePermissions}},
	PermAgentsReceivePermissions:          {Kind: FedPeerGroup, Cap: proto.CapAgentsReceivePermissions, RequiresGroup: true, Sensitive: true},
	PermAgentsReceive:                     {Kind: FedPeerGroup, Cap: proto.CapAgentsReceive, RequiresGroup: true},
	PermGroupsVisibilityRead:              {Kind: FedPeerGroup, Cap: proto.CapGroupVisibility},
	PermGroupsLinkAccept:                  {Kind: FedPeerGroup, Cap: proto.CapGroupLinkAccept, Sensitive: true},
	PermGroupsRosterRead:                  {Kind: FedPeerGroup, Cap: proto.CapRoster},
	PermGroupsPresenceRead:                {Kind: FedPeerGroup, Cap: proto.CapPresence},
	PermMessageAttachments:                {Kind: FedPeerGroup, Cap: proto.CapAttachments},
	PermUsageRead:                         {Kind: FedPeerInstance},
	PermCostsRead:                         {Kind: FedPeerInstance},
	PermUsageRulesManage:                  {Kind: FedPeerInstance, Sensitive: true},
	PermUsageRulesEnforce:                 {Kind: FedPeerGroup, Cap: "usage_enforce", Sensitive: true},
	PermSelfRename:                        {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfCompact:                       {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfInterrupt:                     {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfClone:                         {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfRemoteControl:                 {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfTask:                          {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfPR:                            {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfTags:                          {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfDirRepair:                     {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfProcess:                       {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfPage:                          {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermGroupsPagesRead:                   {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsPagesPatch:                  {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsPagesManage:                 {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermAgentProcess:                      {Kind: FedPeerAlias, Via: []string{PermGroupsMembersProcess}},
	PermGroupsMembersProcess:              {Kind: FedPeerGroup, Cap: "member_process", Sensitive: true},
	PermSelfMorph:                         {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermAgentMorph:                        {Kind: FedPeerAlias, Via: []string{PermGroupsMembersMorph}, Pending: true},
	PermAgentReincarnate:                  {Kind: FedPeerAlias, Via: []string{PermGroupsMembersReincarnate}},
	PermAgentCompact:                      {Kind: FedPeerAlias, Via: []string{PermGroupsMembersCompact}},
	PermAgentInterrupt:                    {Kind: FedPeerAlias, Via: []string{PermGroupsMembersInterrupt}},
	PermAgentRename:                       {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermAgentClone:                        {Kind: FedPeerAlias, Via: []string{PermGroupsMembersClone}},
	PermAgentContextInfo:                  {Kind: FedPeerAlias, Via: []string{PermGroupsMembersContextInfo}},
	PermAgentDebugExport:                  {Kind: FedPeerAlias, Via: []string{PermGroupsMembersDebugExport}, Pending: true},
	PermAgentTask:                         {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermAgentPR:                           {Kind: FedPeerAlias, Via: []string{PermGroupsMembersPR}},
	PermAgentTags:                         {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermAgentSpawn:                        {Kind: FedSender, Via: []string{PermGroupsMembersSpawn}},
	PermGroupsMembersMorph:                {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsMembersReincarnate:          {Kind: FedPeerGroup, Cap: "reincarnate", Sensitive: true},
	PermGroupsMembersCompact:              {Kind: FedPeerGroup, Cap: "compact", Sensitive: true},
	PermGroupsMembersInterrupt:            {Kind: FedPeerGroup, Cap: "interrupt", Sensitive: true},
	PermGroupsMembersRename:               {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermGroupsMembersClone:                {Kind: FedPeerGroup, Cap: "clone", Sensitive: true},
	PermGroupsMembersContextInfo:          {Kind: FedPeerGroup, Cap: "context_info"},
	PermGroupsMembersDebugExport:          {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsMembersTask:                 {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermGroupsMembersPR:                   {Kind: FedPeerGroup, Cap: "pr", Sensitive: true},
	PermGroupsMembersTags:                 {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermGroupsAdmin:                       {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsCreate:                      {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermGroupsDelete:                      {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsMembersStop:                 {Kind: FedPeerGroup, Cap: "stop", Sensitive: true},
	PermGroupsMembersResume:               {Kind: FedPeerGroup, Cap: "resume", Sensitive: true},
	PermGroupsMembersRetire:               {Kind: FedPeerGroup, Cap: "retire", Sensitive: true},
	PermGroupsMembersSpawn:                {Kind: FedPeerGroup, Cap: proto.CapSpawn, Sensitive: true},
	PermGroupsOwnersManage:                {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsRename:                      {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsDescription:         {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsDefaultDir:          {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsDefaultContext:      {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsEnvironment:         {Kind: FedPeerGroup, Cap: "group_environment", SelfGate: true, Sensitive: true},
	PermGroupsSettingsDefaultSpawnTarget:  {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermGroupsSettingsDefaultProfile:      {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsMaxMembers:          {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsNotifications:       {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsRemoteControlPolicy: {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsMemberPermissions:   {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsSettingsOwnerScopes:         {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsMembersAdd:                  {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsMembersRemove:               {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsMembersUpdate:               {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermGroupsMessagesSchedule:            {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermPermissionsGrant:                  {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermPermissionsRevoke:                 {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermSelfSchedule:                      {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermAgentSchedule:                     {Kind: FedPeerAlias, Via: []string{PermGroupsMembersSchedule}, Pending: true},
	PermGroupsMembersSchedule:             {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermAgentStop:                         {Kind: FedPeerAlias, Via: []string{PermGroupsMembersStop}},
	PermAgentResume:                       {Kind: FedPeerAlias, Via: []string{PermGroupsMembersResume}},
	PermGroupsArchive:                     {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsNest:                        {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsAttachment:                  {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsClone:                       {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermAgentDelete:                       {Kind: FedPeerAlias, Via: []string{PermGroupsMembersDelete}, Pending: true},
	PermGroupsMembersDelete:               {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsLinkAdd:                     {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsLinkRemove:                  {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermAgentPromote:                      {Kind: FedLocal, Why: "promotes a plain conversation, which no peer view shows"},
	PermGroupsMembersPromote:              {Kind: FedLocal, Why: "promotes a plain conversation, which no peer view shows"},
	PermAgentRetire:                       {Kind: FedPeerAlias, Via: []string{PermGroupsMembersRetire}},
	PermAgentStanddown:                    {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermAgentRemoteControl:                {Kind: FedPeerAlias, Via: []string{PermGroupsMembersRemoteControl}, Pending: true},
	PermGroupsMembersRemoteControl:        {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermAgentSandboxImplementation:        {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermAgentInboxWatch:                   {Kind: FedPeerAlias, Via: []string{PermGroupsMembersInboxWatch}, Pending: true},
	PermGroupsMembersInboxWatch:           {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermMessageDirect:                     {Kind: FedPeerGroup, Cap: proto.CapMail},
	PermGroupsExport:                      {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermGroupsImport:                      {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermTemplatesManage:                   {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermTemplatesUse:                      {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermSelfTeleport:                      {Kind: FedSender, Via: []string{PermAgentsReceive, PermAgentsTeleportReceive}},
	PermAgentsTeleportReceive:             {Kind: FedPeerGroup, Cap: proto.CapTeleportReceive, RequiresGroup: true},
	PermAgentMove:                         {Kind: FedPeerGroup, Cap: "move", Sensitive: true},
	PermAgentShare:                        {Kind: FedSender, Via: []string{PermAgentsReceive}},
	PermAgentBundleExport:                 {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermAgentBundleImport:                 {Kind: FedPeerAlias, Via: []string{PermAgentsReceive}},
	PermConfigExport:                      {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermConfigImport:                      {Kind: FedPeerAlias, Via: []string{PermConfigOffer}},
	PermProfilesManage:                    {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermSandboxHarnessConfig:              {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermSandboxProfilesManage:             {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermRelaunchPolicyManage:              {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermSandboxProfilesDraft:              {Kind: FedLocal, Why: "a scribe the local operator summoned hands its draft to that operator"},
	PermPromptsManage:                     {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermRolesManage:                       {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermProcessAdvance:                    {Kind: FedPeerGroup, Cap: "process_advance", SelfGate: true, Sensitive: true},
	PermProcessTemplatesRead:              {Kind: FedPeerInstance, Pending: true},
	PermProcessTemplatesManage:            {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermProcessRunsRead:                   {Kind: FedPeerInstance},
	PermProcessRunsStart:                  {Kind: FedPeerGroup, Cap: "process_runs_start", Sensitive: true},
	PermProcessRunsManage:                 {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermTriggersRead:                      {Kind: FedPeerAlias, Via: []string{PermGroupsTriggersRead}, Pending: true},
	PermTriggersManage:                    {Kind: FedPeerAlias, Via: []string{PermGroupsTriggersManage}, Pending: true},
	PermGroupsTriggersRead:                {Kind: FedPeerGroup, Pending: true},
	PermGroupsTriggersManage:              {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermHumanNotify:                       {Kind: FedPeerInstance, Pending: true},
	PermHumanClipboard:                    {Kind: FedLocal, Why: "the local display"},
	session.PermAutoPermitEnterWorktree:   {Kind: FedLocal, Why: "answers a harness check inside the agent's own pane"},
	PermSettingsDefaultModel:              {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermSessionsFilesRead:                 {Kind: FedPeerGroup, Cap: proto.CapSessionsFilesRead},
	PermSessionsWatch:                     {Kind: FedPeerGroup, Cap: proto.CapSessionsWatch},
	PermSessionsAttach:                    {Kind: FedPeerGroup, Cap: proto.CapSessionsAttach, Sensitive: true},
	PermSessionsTakeover:                  {Kind: FedPeerGroup, Cap: proto.CapSessionsTakeover, Sensitive: true},
	PermAgentsStatusRead:                  {Kind: FedPeerGroup, Cap: proto.CapAgentStatus},
	PermFederationAuditRead:               {Kind: FedPeerInstance},
	PermNodeUpdate:                        {Kind: FedPeerInstance, Sensitive: true},
	PermNodeHarnessesInstall:              {Kind: FedPeerInstance, Sensitive: true},
	PermNodeExec:                          {Kind: FedPeerInstance, Sensitive: true},
	PermNodeMessagesRead:                  {Kind: FedPeerInstance},
	PermSessionsShell:                     {Kind: FedPeerGroup, Cap: proto.CapSessionsShell, Sensitive: true},
	PermGroupsMembersSpawnCwd:             {Kind: FedPeerGroup, Cap: "spawn_cwd", Sensitive: true},
	PermGroupsMembersSpawnHarness:         {Kind: FedPeerGroup, Cap: "spawn_harness"},
	PermGroupsMembersSpawnModel:           {Kind: FedPeerGroup, Cap: "spawn_model"},
	PermGroupsMembersSpawnEffort:          {Kind: FedPeerGroup, Cap: "spawn_effort"},
	PermGroupsMembersSpawnProfile:         {Kind: FedPeerGroup, Cap: "spawn_profile", Sensitive: true},
	PermGroupsMembersSpawnRoles:           {Kind: FedPeerGroup, Cap: "spawn_roles", Sensitive: true},
	PermGroupsMembersSpawnSandbox:         {Kind: FedPeerGroup, Cap: "spawn_sandbox", Sensitive: true},
	PermGroupsMembersSpawnPermissions:     {Kind: FedPeerGroup, Cap: "spawn_permissions", Sensitive: true},
	PermGroupsMembersSpawnEnvironment:     {Kind: FedPeerGroup, Cap: "spawn_environment", Sensitive: true},
	PermGroupsMembersSpawnOwner:           {Kind: FedPeerGroup, Cap: "spawn_owner", Sensitive: true},
	PermGroupsMembersSpawnFeatures:        {Kind: FedPeerGroup, Cap: "spawn_features"},
	PermGroupsMembersSpawnPrompts:         {Kind: FedPeerGroup, Cap: "spawn_prompts"},
	PermGroupsMembersSpawnWorktree:        {Kind: FedPeerGroup, Cap: "spawn_worktree", Sensitive: true},
	PermGroupsMembersSpawnAll:             {Kind: FedPeerGroup, Cap: "spawn_all", Sensitive: true},
	PermGroupsMembersManage:               {Kind: FedPeerGroup, Cap: "member_manage"},
	PermGroupsResourcesRead:               {Kind: FedPeerGroup, Cap: "resource_read"},
	PermGroupsResourcesWrite:              {Kind: FedPeerGroup, Pending: true, Sensitive: true},
	PermGroupsResourcesLock:               {Kind: FedPeerGroup, Cap: "resource_lock"},
	PermGroupsSettings:                    {Kind: FedPeerGroup, Cap: "group_settings", Sensitive: true},
	PermNodeSpawnRead:                     {Kind: FedPeerInstance, Sensitive: true},
	PermNodeSpawnDefaultsManage:           {Kind: FedPeerInstance, Sensitive: true},
	PermNodeSpawnProfilesCreate:           {Kind: FedPeerInstance, Sensitive: true},
	PermNodeGroupsCreate:                  {Kind: FedPeerInstance, Sensitive: true},
	PermNodeMessagesManage:                {Kind: FedPeerInstance, Sensitive: true},
	PermNodeMessagesMarkRead:              {Kind: FedPeerInstance},
	PermNodeCredentialsReceive:            {Kind: FedPeerInstance, Sensitive: true},
	PermSnapshotsReceive:                  {Kind: FedPeerInstance, Sensitive: true},
	PermNodeHarnessesRead:                 {Kind: FedPeerInstance},
	PermNodeRead:                          {Kind: FedPeerInstance},
	PermSessionsRead:                      {Kind: FedPeerGroup, Cap: proto.CapSessions},
	PermRoutesPublishHost:                 {Kind: FedLocal, Why: "host-only by definition"},
	PermRoutesPublish:                     {Kind: FedLocal, Why: "publishes the caller's own route; peers open shared group routes through routes.consume"},
	PermRoutesConsume:                     {Kind: FedPeerGroup, Cap: proto.CapRoutes},
	PermGitRead:                           {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermGitPush:                           {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermGitHubRead:                        {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermGitHubWrite:                       {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermGitHubMerge:                       {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermGitLabRead:                        {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermGitLabWrite:                       {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermGitLabMerge:                       {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermModelsProxy:                       {Kind: FedPeerProxy},
	PermHTTP:                              {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermLinearRead:                        {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermLinearWrite:                       {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermAWBRead:                           {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermAWBWrite:                          {Kind: FedPeerInstance, Pending: true, Sensitive: true},
	PermConfigOffer:                       {Kind: FedPeerInstance},
}

// peerPassThrough is a gate a restricted peer passes inside another granted
// peer action, beyond the Via of the gate's own reach.
type peerPassThrough struct {
	// Via is the action's peer grant.
	Via string
	// Footprint is the peer grant then required in every group of the
	// target agent, when it is not Via itself.
	Footprint string
	// InGroup checks Via in the group the gate names instead of the target
	// agent's footprint, and answers only a gate without a target.
	InGroup bool
}

// peerActionPassThroughs are those gates: a move or teleport pulls the agent
// through its export, share and self-teleport, and through its retire under
// the retire grant; a clone landing in another group joins it as a member
// there.
var peerActionPassThroughs = map[string][]peerPassThrough{
	PermAgentRetire:       {{Via: PermAgentMove, Footprint: PermGroupsMembersRetire}},
	PermAgentShare:        {{Via: PermAgentMove}},
	PermSelfTeleport:      {{Via: PermAgentMove}},
	PermAgentBundleExport: {{Via: PermAgentMove}},
	PermGroupsMembersAdd:  {{Via: PermGroupsMembersClone, InGroup: true}},
}

// groupLocalGate reports a gate scoped to one group: its registry entry
// declares the group scope dimension and is not itself a global agent.*
// capability with a group sibling (groups.members.stop is group-local;
// agent.stop is not). Peer-invoked group routes answer group-local gates; a
// peer action on an agent answers the others.
func groupLocalGate(slug string) bool {
	for _, entry := range permissionRegistry {
		if entry.Slug == slug {
			return containsScopeDim(entry.ScopeDims, ScopeDimGroup) && entry.GroupSibling == ""
		}
	}
	return false
}

// peerGateGrants are the peer grants that answer gate by its own reach: the
// gate itself when it is a grantable peer slug, or the grants it goes via.
func peerGateGrants(gate string) []string {
	reach := slugFederationReach[gate]
	switch {
	case reach.peerGrantable():
		return []string{gate}
	case reach.Kind == FedPeerAlias && !reach.Pending:
		return reach.Via
	}
	return nil
}

// peerActionFootprint resolves gate for a peer action under the rule's peer
// grant: ok when the grant answers it, with the grant the target's footprint
// must then hold (or, for an in-group pass-through, the group named).
func peerActionFootprint(gate, rule string) (pass peerPassThrough, ok bool) {
	if !groupLocalGate(gate) && slices.Contains(peerGateGrants(gate), rule) {
		return peerPassThrough{Via: rule, Footprint: rule}, true
	}
	for _, p := range peerActionPassThroughs[gate] {
		if p.Via == rule {
			if p.Footprint == "" {
				p.Footprint = rule
			}
			return p, true
		}
	}
	return peerPassThrough{}, false
}

// peerGroupGateGrants maps each group peer grant to the group-local gates it
// answers on a peer-invoked group route.
var peerGroupGateGrants = derivePeerGroupGateGrants()

func derivePeerGroupGateGrants() map[string][]string {
	out := map[string][]string{}
	for slug, reach := range slugFederationReach {
		if reach.SelfGate && reach.peerGrantable() {
			out[slug] = append(out[slug], slug)
		}
		if reach.Kind != FedPeerAlias || reach.Pending || !groupLocalGate(slug) {
			continue
		}
		for _, via := range reach.Via {
			out[via] = append(out[via], slug)
		}
	}
	for grant := range out {
		slices.Sort(out[grant])
	}
	return out
}

// fedPeerSlugRequiresGroup reports a peer grant that must be group-scoped.
func fedPeerSlugRequiresGroup(slug string) bool {
	return slugFederationReach[slug].RequiresGroup
}

// fedPeerSlugsOfKind lists the grantable peer slugs of kind.
func fedPeerSlugsOfKind(kind FedReachKind) []string {
	var out []string
	for slug, reach := range slugFederationReach {
		if reach.Kind == kind && reach.peerGrantable() {
			out = append(out, slug)
		}
	}
	slices.Sort(out)
	return out
}

func deriveFederationPeerSlugs() map[string]string {
	out := map[string]string{}
	for _, slug := range fedPeerSlugsOfKind(FedPeerGroup) {
		out[slug] = slugFederationReach[slug].Cap
	}
	return out
}

func deriveFedInstancePeerSlugs() map[string]bool {
	out := map[string]bool{}
	for _, slug := range fedPeerSlugsOfKind(FedPeerInstance) {
		out[slug] = true
	}
	return out
}

// fedProxyPeerSlug reports a model gateway peer grant (http_proxy= scoped).
func fedProxyPeerSlug(slug string) bool {
	reach, ok := slugFederationReach[slug]
	return ok && reach.Kind == FedPeerProxy && reach.peerGrantable()
}

// initFederationReach copies each slug's reach onto the registry, and
// panics when the table and the registry disagree.
func initFederationReach(registry []PermSlug, reachTable map[string]FedReach) {
	seen := map[string]bool{}
	for i := range registry {
		reach, ok := reachTable[registry[i].Slug]
		if !ok {
			panic(fmt.Sprintf("permission registry: slug %q declares no federation reach (slugFederationReach)", registry[i].Slug))
		}
		registry[i].Federation = reach
		seen[registry[i].Slug] = true
	}
	for slug, reach := range reachTable {
		if !seen[slug] {
			panic(fmt.Sprintf("slugFederationReach: %q is not a registered slug", slug))
		}
		for _, via := range reach.Via {
			if _, ok := reachTable[via]; !ok {
				panic(fmt.Sprintf("slugFederationReach: %q goes via unknown slug %q", slug, via))
			}
		}
		if (reach.SelfGate || reach.RequiresGroup) && reach.Kind != FedPeerGroup {
			panic(fmt.Sprintf("slugFederationReach: %q: only a group peer slug is self-gating or requires a group", slug))
		}
		if (reach.Cap != "") != (reach.Kind == FedPeerGroup && !reach.Pending) {
			panic(fmt.Sprintf("slugFederationReach: %q: only a grantable group peer slug carries a wire capability", slug))
		}
	}
}
