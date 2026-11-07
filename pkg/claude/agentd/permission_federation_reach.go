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

// peerPagesWhy: a live page is served from this node's stage origin into
// its local operator's own dashboard and to its agents.
const peerPagesWhy = "a page is this node's stage origin, shown to the local operator's own dashboard and its agents; a peer has no view of it"

// peerCredentialProxyWhy: the git, forge, HTTP, Linear and awb proxies carry
// this node's operator's credentials to this node's own agents. Forwarding a
// peer's agents' calls through them is parked as an idea (tcl-31l8c0).
const peerCredentialProxyWhy = "uses this node's operator's credentials; a peer's agents use their own node's proxies"

// peerUmbrellaWhy: an umbrella slug confers a family of operations at once;
// a peer is granted each one, so its grant list says exactly what it holds.
const peerUmbrellaWhy = "an umbrella over many slugs; a peer is granted each operation it needs, so its grant list says exactly what it holds"

// peerMorphWhy: a morph re-forms an agent's whole launch, which may widen it.
const peerMorphWhy = "morph re-forms an agent's whole launch (profile, harness, sandbox, grants); a peer changes only the narrow parts it is granted, such as agent.sandbox-impl and permissions.grant"

// peerInboxWatchWhy: an inbox watch delivers into the calling agent's pane.
const peerInboxWatchWhy = "delivers another member's mail into the calling agent's own pane; a peer reads mail through node.messages.read"

// peerTriggersManageWhy: trigger rules are authored only on this node's
// dashboard, by its operator; locally no agent creates or changes one either,
// and the slug only revalidates an agent owner no path creates.
const peerTriggersManageWhy = "trigger rules are authored only by this node's operator"

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
	PermBoardsRead:                        {Kind: FedPeerAlias, Via: []string{PermGroupsResourcesRead}},
	PermBoardsWrite:                       {Kind: FedPeerAlias, Via: []string{PermGroupsResourcesWrite}},
	PermResourcesRead:                     {Kind: FedLocal, Why: peerUmbrellaWhy},
	PermResourcesWrite:                    {Kind: FedLocal, Why: peerUmbrellaWhy},
	PermHostRead:                          {Kind: FedPeerInstance, Sensitive: true},
	PermHumanInboxRead:                    {Kind: FedPeerGroup, Cap: "human_inbox_read", Sensitive: true},
	PermHumanInboxAnswer:                  {Kind: FedPeerGroup, Cap: "human_inbox_answer", Sensitive: true},
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
	PermSelfTaskboard:                     {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermTaskboardRead:                     {Kind: FedLocal, Why: peerTaskboardWhy},
	PermTaskboardWrite:                    {Kind: FedLocal, Why: peerTaskboardWhy},
	PermTaskboardManage:                   {Kind: FedLocal, Why: peerTaskboardWhy},
	PermTaskboardAssign:                   {Kind: FedLocal, Why: peerTaskboardWhy},
	PermSelfPR:                            {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfTags:                          {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfDirRepair:                     {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfProcess:                       {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermSelfPage:                          {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermGroupsPagesRead:                   {Kind: FedLocal, Why: peerPagesWhy},
	PermGroupsPagesPatch:                  {Kind: FedLocal, Why: peerPagesWhy},
	PermGroupsPagesManage:                 {Kind: FedLocal, Why: peerPagesWhy},
	PermAgentProcess:                      {Kind: FedPeerAlias, Via: []string{PermGroupsMembersProcess}},
	PermGroupsMembersProcess:              {Kind: FedPeerGroup, Cap: "member_process", Sensitive: true},
	PermSelfMorph:                         {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermAgentMorph:                        {Kind: FedLocal, Why: peerMorphWhy},
	PermAgentReincarnate:                  {Kind: FedPeerAlias, Via: []string{PermGroupsMembersReincarnate}},
	PermAgentCompact:                      {Kind: FedPeerAlias, Via: []string{PermGroupsMembersCompact}},
	PermAgentInterrupt:                    {Kind: FedPeerAlias, Via: []string{PermGroupsMembersInterrupt}},
	PermAgentRename:                       {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermAgentClone:                        {Kind: FedPeerAlias, Via: []string{PermGroupsMembersClone}},
	PermAgentContextInfo:                  {Kind: FedPeerAlias, Via: []string{PermGroupsMembersContextInfo}},
	PermAgentDebugExport:                  {Kind: FedPeerAlias, Via: []string{PermGroupsMembersDebugExport}},
	PermAgentTask:                         {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermAgentPR:                           {Kind: FedPeerAlias, Via: []string{PermGroupsMembersPR}},
	PermAgentTags:                         {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermAgentSpawn:                        {Kind: FedSender, Via: []string{PermGroupsMembersSpawn}},
	PermGroupsMembersMorph:                {Kind: FedLocal, Why: peerMorphWhy},
	PermGroupsMembersReincarnate:          {Kind: FedPeerGroup, Cap: "reincarnate", Sensitive: true},
	PermGroupsMembersCompact:              {Kind: FedPeerGroup, Cap: "compact", Sensitive: true},
	PermGroupsMembersInterrupt:            {Kind: FedPeerGroup, Cap: "interrupt", Sensitive: true},
	PermGroupsMembersRename:               {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermGroupsMembersClone:                {Kind: FedPeerGroup, Cap: "clone", Sensitive: true},
	PermGroupsMembersContextInfo:          {Kind: FedPeerGroup, Cap: "context_info"},
	PermGroupsMembersDebugExport:          {Kind: FedPeerGroup, Cap: "member_debug_export", Sensitive: true},
	PermGroupsMembersTask:                 {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermGroupsMembersPR:                   {Kind: FedPeerGroup, Cap: "pr", Sensitive: true},
	PermGroupsMembersTags:                 {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermGroupsAdmin:                       {Kind: FedLocal, Why: peerUmbrellaWhy},
	PermGroupsCreate:                      {Kind: FedPeerAlias, Via: []string{PermNodeGroupsCreate}},
	PermGroupsDelete:                      {Kind: FedPeerGroup, Cap: "group_delete", Sensitive: true},
	PermGroupsMembersStop:                 {Kind: FedPeerGroup, Cap: "stop", Sensitive: true},
	PermGroupsMembersResume:               {Kind: FedPeerGroup, Cap: "resume", Sensitive: true},
	PermGroupsMembersRetire:               {Kind: FedPeerGroup, Cap: "retire", Sensitive: true},
	PermGroupsMembersSpawn:                {Kind: FedPeerGroup, Cap: proto.CapSpawn, Sensitive: true},
	PermGroupsOwnersManage:                {Kind: FedPeerGroup, Cap: "owners_manage", Sensitive: true},
	PermGroupsRename:                      {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsDescription:         {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsDefaultDir:          {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsDefaultContext:      {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsEnvironment:         {Kind: FedPeerGroup, Cap: "group_environment", SelfGate: true, Sensitive: true},
	PermGroupsSettingsDefaultSpawnTarget:  {Kind: FedLocal, Why: "picks where this node's own operator and agents spawn by default; a peer names the group of each spawn it asks for"},
	PermGroupsSettingsDefaultProfile:      {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsMaxMembers:          {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsNotifications:       {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsRemoteControlPolicy: {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsSettingsMemberPermissions:   {Kind: FedPeerGroup, Cap: "member_permissions", SelfGate: true, Sensitive: true},
	PermGroupsSettingsOwnerScopes:         {Kind: FedPeerGroup, Cap: "owner_scopes", SelfGate: true, Sensitive: true},
	PermGroupsMembersAdd:                  {Kind: FedPeerGroup, Cap: "members_add", Sensitive: true},
	PermGroupsMembersRemove:               {Kind: FedPeerGroup, Cap: "members_remove", Sensitive: true},
	PermGroupsMembersUpdate:               {Kind: FedPeerAlias, Via: []string{PermGroupsMembersManage}},
	PermGroupsMessagesSchedule:            {Kind: FedPeerGroup, Cap: "messages_schedule", Sensitive: true},
	PermPermissionsGrant:                  {Kind: FedPeerGroup, Cap: "permissions_grant", Sensitive: true},
	PermPermissionsRevoke:                 {Kind: FedPeerGroup, Cap: "permissions_revoke", Sensitive: true},
	PermSelfSchedule:                      {Kind: FedLocal, Why: "acts on the calling agent itself"},
	PermAgentSchedule:                     {Kind: FedPeerAlias, Via: []string{PermGroupsMembersSchedule}},
	PermGroupsMembersSchedule:             {Kind: FedPeerGroup, Cap: "member_schedule", Sensitive: true},
	PermAgentStop:                         {Kind: FedPeerAlias, Via: []string{PermGroupsMembersStop}},
	PermAgentResume:                       {Kind: FedPeerAlias, Via: []string{PermGroupsMembersResume}},
	PermGroupsArchive:                     {Kind: FedPeerGroup, Cap: "group_archive", Sensitive: true},
	PermGroupsNest:                        {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsAttachment:                  {Kind: FedPeerAlias, Via: []string{PermGroupsSettings}},
	PermGroupsClone:                       {Kind: FedPeerGroup, Cap: "group_clone", Sensitive: true},
	PermAgentDelete:                       {Kind: FedPeerAlias, Via: []string{PermGroupsMembersDelete}},
	PermGroupsMembersDelete:               {Kind: FedPeerGroup, Cap: "member_delete", Sensitive: true},
	PermGroupsLinkAdd:                     {Kind: FedPeerGroup, Cap: "link_add", Sensitive: true},
	PermGroupsLinkRemove:                  {Kind: FedPeerGroup, Cap: "link_remove", Sensitive: true},
	PermAgentPromote:                      {Kind: FedLocal, Why: "promotes a plain conversation, which no peer view shows"},
	PermGroupsMembersPromote:              {Kind: FedLocal, Why: "promotes a plain conversation, which no peer view shows"},
	PermAgentRetire:                       {Kind: FedPeerAlias, Via: []string{PermGroupsMembersRetire}},
	PermAgentStanddown:                    {Kind: FedLocal, Why: "reserved for the scoped stand-down flow; no operation checks it yet"},
	PermAgentRemoteControl:                {Kind: FedPeerAlias, Via: []string{PermGroupsMembersRemoteControl}},
	PermGroupsMembersRemoteControl:        {Kind: FedPeerGroup, Cap: "remote_control", Sensitive: true},
	PermAgentSandboxImplementation:        {Kind: FedPeerGroup, Cap: "sandbox_impl", Sensitive: true},
	PermAgentInboxWatch:                   {Kind: FedLocal, Why: peerInboxWatchWhy},
	PermGroupsMembersInboxWatch:           {Kind: FedLocal, Why: peerInboxWatchWhy},
	PermMessageDirect:                     {Kind: FedPeerGroup, Cap: proto.CapMail},
	PermGroupsExport:                      {Kind: FedPeerGroup, Cap: "group_export", Sensitive: true},
	PermGroupsImport:                      {Kind: FedPeerInstance, Sensitive: true},
	PermTemplatesManage:                   {Kind: FedPeerInstance, Sensitive: true},
	PermTemplatesUse:                      {Kind: FedPeerGroup, Cap: "templates_use", SelfGate: true, Sensitive: true},
	PermSelfTeleport:                      {Kind: FedSender, Via: []string{PermAgentsReceive, PermAgentsTeleportReceive}},
	PermAgentsTeleportReceive:             {Kind: FedPeerGroup, Cap: proto.CapTeleportReceive, RequiresGroup: true},
	PermAgentMove:                         {Kind: FedPeerGroup, Cap: "move", Sensitive: true},
	PermAgentShare:                        {Kind: FedSender, Via: []string{PermAgentsReceive}},
	PermAgentBundleExport:                 {Kind: FedPeerGroup, Cap: "agent_bundle_export", Sensitive: true},
	PermAgentBundleImport:                 {Kind: FedPeerAlias, Via: []string{PermAgentsReceive}},
	PermConfigExport:                      {Kind: FedPeerInstance, Sensitive: true},
	PermConfigImport:                      {Kind: FedPeerAlias, Via: []string{PermConfigOffer}},
	PermProfilesManage:                    {Kind: FedPeerInstance, Sensitive: true},
	PermSandboxHarnessConfig:              {Kind: FedLocal, Why: "selects a spawn's harness-config posture directly; a peer's spawn carries no such setting, and its sandbox choices take groups.members.spawn.sandbox and groups.members.spawn.permissions"},
	PermSandboxProfilesManage:             {Kind: FedPeerInstance, Sensitive: true},
	PermRelaunchPolicyManage:              {Kind: FedLocal, Why: "a peer is deliberately refused, even with unrestricted trust (relaunchPolicyFromPeer)"},
	PermSandboxProfilesDraft:              {Kind: FedLocal, Why: "a scribe the local operator summoned hands its draft to that operator"},
	PermPromptsManage:                     {Kind: FedPeerInstance, Sensitive: true},
	PermRolesManage:                       {Kind: FedPeerInstance, Sensitive: true},
	PermProcessAdvance:                    {Kind: FedPeerGroup, Cap: "process_advance", SelfGate: true, Sensitive: true},
	PermProcessTemplatesRead:              {Kind: FedPeerInstance},
	PermProcessTemplatesManage:            {Kind: FedPeerInstance, Sensitive: true},
	PermProcessRunsRead:                   {Kind: FedPeerInstance, Sensitive: true},
	PermProcessRunsStart:                  {Kind: FedPeerGroup, Cap: "process_runs_start", Sensitive: true},
	PermProcessRunsManage:                 {Kind: FedPeerInstance, Sensitive: true},
	PermTriggersRead:                      {Kind: FedPeerAlias, Via: []string{PermGroupsTriggersRead}},
	PermTriggersManage:                    {Kind: FedLocal, Why: peerTriggersManageWhy},
	PermGroupsTriggersRead:                {Kind: FedPeerGroup, Cap: "triggers_read"},
	PermGroupsTriggersManage:              {Kind: FedLocal, Why: peerTriggersManageWhy},
	PermHumanNotify:                       {Kind: FedLocal, Why: "an agent of this node reaching its own operator's Messages tab; a peer's agents notify their own operator"},
	PermHumanClipboard:                    {Kind: FedLocal, Why: "the local display"},
	session.PermAutoPermitEnterWorktree:   {Kind: FedLocal, Why: "answers a harness check inside the agent's own pane"},
	PermSettingsDefaultModel:              {Kind: FedLocal, Why: "edits the host's ~/.claude/settings.json, which reaches Claude sessions outside tclaude"},
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
	PermGroupsResourcesWrite:              {Kind: FedPeerGroup, Cap: "resource_write", Sensitive: true},
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
	PermGitRead:                           {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermGitPush:                           {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermGitHubRead:                        {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermGitHubWrite:                       {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermGitHubMerge:                       {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermGitLabRead:                        {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermGitLabWrite:                       {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermGitLabMerge:                       {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermModelsProxy:                       {Kind: FedPeerProxy},
	PermHTTP:                              {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermLinearRead:                        {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermLinearWrite:                       {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermAWBRead:                           {Kind: FedLocal, Why: peerCredentialProxyWhy},
	PermAWBWrite:                          {Kind: FedLocal, Why: peerCredentialProxyWhy},
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
