package agentd

import (
	"maps"
	"reflect"
	"strings"
)

// Explicit classifications cover the complete dashboard snapshot and its
// populated row types. Unknown fields fail closed; the guard test requires a
// decision when the schema changes. No local snapshot is copied into a peer
// projection. Denied fields keep their typed zero/empty values.
var peerSnapshotFields = map[reflect.Type]map[string]string{
	reflect.TypeFor[snapshotPayload](): classifyPeerFields(map[string]string{
		"public":    "GeneratedAt Version AssetsVersion ActivityBots HScrollFollow OverlayBackdrop GroupQuickOptions DefaultTerminal DefaultDirectoryPicker",
		"defaults":  "SpawnProfileDefault SandboxProfileDefault",
		"catalogs":  "Templates Profiles Roles SavedPrompts SandboxProfiles SandboxProfilesRev",
		"projected": "Groups Agents AgentRosterAuthoritative StaticVersion",
		"denied":    "Voice WindowControls StewardName StewardActive StewardRoster StewardElevated StewardAccess KnowledgeSource Harnesses AuthSession StaticUnchanged Ungrouped Pending Permissions Slugs Cron ExportJobsActive RetiredTotal Sudo Links RouteMap Usage AuthoredOpenPRs TrackedPRAuthors QueuedSpawns Messages MessagesUnread AccessRequests AccessRequestsPending Plugins PluginsCatalog PluginsWarn PluginsError PluginsTabVisible ProcessesEnabled CollabEnabled TriggersEnabled GroupsRouteMapEnabled GroupAttachmentsMode TerminalPaletteShortcut RecordedSandboxDetails UserDefaultModel SandboxImpl PopupBase NotificationsEnabled SpawnNameNormalize VegasInRegularMode HidePullLever TerminalAttach ShowAgentHideButton ShowGroupDescription ShowFinalPRChecks StayOnTerminalsAfterLastClose FusedToolbarFixedNode UsageCollapseSubviews CostTabVisible CostTabWhatIf BrokerRefusalsTotal BrokerRefusalsUnplaceable UsageTabVisible RemoteAccess CompanionCues",
	}),
	reflect.TypeFor[dashboardGroup](): classifyPeerFields(map[string]string{
		"identity":  "ID Name Descr",
		"projected": "Members Online",
		"settings":  "AttachmentURL AttachmentLabel AttachmentLabelOverride DefaultCwd DefaultContext Environment DefaultProfile DefaultPrompts SandboxProfile MaxMembers NotifyEnabled RemoteControlPolicy ReinjectAfterCompact Parent",
		"denied":    "DefaultSpawnGroup Permissions PermissionScopes UnreadablePermissionScopes OwnerScopes Mission SourceTemplate RouteGeneration Process Waves Scribe FederationLinks",
	}),
	reflect.TypeFor[dashboardMember](): classifyPeerFields(map[string]string{
		"identity": "AgentID ConvID Title",
		"roster":   "Role",
		"presence": "Online FederationPresence",
		"status":   "State taskRefView ClaudeSubscription",
		"settings": "Descr tagsView Notify NotifyEffective",
		"denied":   "CreatedAt agentLocationView repoLinksView Waking ExternalSteward SpawnedByExternalSteward RouteHealth Owner",
	}),
	reflect.TypeFor[dashboardAgent](): classifyPeerFields(map[string]string{
		"identity": "AgentID ConvID Title Groups",
		"presence": "Online",
		"status":   "State taskRefView",
		"denied":   "agentLocationView repoLinksView tagsView Waking ExternalSteward SpawnedByExternalSteward OwnedGroups Effective ActiveSudo Notify NotifyEffective",
	}),
	reflect.TypeFor[taskRefView](): classifyPeerFields(map[string]string{"status": "TaskURL TaskLabel", "denied": "TaskLabelOverride"}),
	reflect.TypeFor[agentState](): classifyPeerFields(map[string]string{
		"status": "Status SubagentCount BgShellCount MonitorCount ContextPct TokensInput TokensOutput ContextWindowSize Model EffortLevel Harness ExitReason RecoveryStatus",
		"denied": "StatusDetail StatusWarning TemporaryHarnessBuiltinMode PendingMorph FastMode LastHook Cwd ContextWindowMax ContextWindowSource CopilotAPI CopilotAPIConnected CodexAppServer CodexAppServerState CodexAppServerHealth CodexAppServerSource CodexAppServerVersion CodexAppServerDetail CodexObserverMode CodexObserverUpdated CopilotAPIChannelFailed AutoCompactWindow CostUSD VirtualCostUSD VirtualCostCredits BrokerRefusals BrokerRefusalDetail BrokerRefusalSince HarnessBuiltinMode HarnessBuiltinModeSource SandboxImplementation OSSandboxState OSSandboxSource OSSandboxUnverified SandboxProfiles SandboxProfilesRecorded SandboxAccessNotices SandboxProfilesOmitted ResourceCgroup ResourceMemoryLimit ResourceCPULimit ResourcePIDsLimit RemoteControl RecoveryDetail RecoveryReason RecoveryCount RecoveryBackoff RecoveryNextAttempt RecoveryLastExitCode",
	}),
}

func classifyPeerFields(byPolicy map[string]string) map[string]string {
	out := map[string]string{}
	for policy, names := range byPolicy {
		for _, name := range strings.Fields(names) {
			out[name] = policy
		}
	}
	return out
}

// Projection constructors select authorized values; this final gate also
// prevents an accidentally copied private field or an unclassified addition
// from escaping. It does not mutate the shared gather.
var allPeerProjectionFields = map[string]bool{"public": true, "projected": true, "identity": true, "roster": true, "presence": true, "status": true}

// peerGroupSettingsPolicy classifies the group and member settings a peer
// reads only where its grants let it change them: every row drops them, and
// projectPeerGroupSettings and projectPeerMemberSettings set the granted ones
// afterwards, so the final snapshot gate (peerSnapshotGate) passes them.
const peerGroupSettingsPolicy = "settings"

// peerSnapshotGate is the final gate's policy set for a snapshot projected
// with fields: the per-row settings projection already decided those.
func peerSnapshotGate(fields map[string]bool) map[string]bool {
	out := maps.Clone(fields)
	out[peerGroupSettingsPolicy] = true
	return out
}

// peerDefaultsProjectionFields adds the node's global spawn and sandbox
// profile names, for a peer that reads the spawn catalogs or manages the
// defaults: the Groups toolbar shows what it would change.
var peerDefaultsProjectionFields = map[string]bool{"public": true, "projected": true, "identity": true, "roster": true, "presence": true, "status": true, "defaults": true}

// peerCatalogProjectionFields adds the catalogs the dock lists -- group
// templates, spawn profiles, roles, saved prompts and the sandbox profile
// names -- for a peer that reads the spawn catalogs (node.spawn.read): the
// fused dashboard's right panel shows what each node holds, read-only.
var peerCatalogProjectionFields = map[string]bool{"public": true, "projected": true, "identity": true, "roster": true, "presence": true, "status": true, "defaults": true, "catalogs": true}

func filterPeerFields(v reflect.Value, grants map[string]bool) {
	switch v.Kind() {
	case reflect.Struct:
		fields, classified := peerSnapshotFields[v.Type()]
		if !classified {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			policy := fields[v.Type().Field(i).Name]
			if !grants[policy] {
				zeroPeerField(field)
				continue
			}
			filterPeerFields(field, grants)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			filterPeerFields(v.Index(i), grants)
		}
	}
}
func zeroPeerField(v reflect.Value) {
	if v.CanSet() {
		v.SetZero()
		return
	}
	// Anonymous unexported view structs have exported JSON fields. Zero their
	// children rather than using unsafe reflection to set the embedded value.
	if v.Kind() == reflect.Struct {
		for i := 0; i < v.NumField(); i++ {
			zeroPeerField(v.Field(i))
		}
	}
}
