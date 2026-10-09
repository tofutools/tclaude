package agentd

import (
	"reflect"
	"strings"
)

// Explicit classifications cover the complete dashboard snapshot and its
// populated row types. Unknown fields fail closed; the guard test requires a
// decision when the schema changes. No local snapshot is copied into a peer
// projection. Denied fields keep their typed zero/empty values.
var peerSnapshotFields = map[reflect.Type]map[string]string{
	reflect.TypeFor[snapshotPayload](): classifyPeerFields(map[string]string{
		"public":    "GeneratedAt Version AssetsVersion ActivityBots HScrollFollow GroupQuickOptions DefaultTerminal DefaultDirectoryPicker",
		"projected": "Groups Agents AgentRosterAuthoritative StaticVersion",
		"denied":    "Harnesses AuthSession StaticUnchanged SandboxProfiles SandboxProfileDefault Ungrouped Pending Permissions Slugs Cron ExportJobsActive RetiredTotal Sudo Links RouteMap Usage AuthoredOpenPRs Templates Profiles SpawnProfileDefault Roles Messages MessagesUnread AccessRequests AccessRequestsPending Plugins PluginsCatalog PluginsWarn PluginsError PluginsTabVisible DebugTabVisible ProcessesEnabled TriggersEnabled GroupsRouteMapEnabled GroupAttachmentsMode TerminalPaletteShortcut RecordedSandboxDetails UserDefaultModel SandboxImpl PopupBase NotificationsEnabled SpawnNameNormalize VegasInRegularMode HidePullLever TerminalAttach ShowAgentHideButton ShowGroupDescription CostTabVisible CostTabWhatIf BrokerRefusalsTotal BrokerRefusalsUnplaceable UsageTabVisible RemoteAccess",
	}),
	reflect.TypeFor[dashboardGroup](): classifyPeerFields(map[string]string{
		"identity":  "Name Descr",
		"projected": "Members Online",
		"denied":    "AttachmentURL AttachmentLabel AttachmentLabelOverride DefaultCwd DefaultSpawnGroup DefaultContext Environment DefaultProfile SandboxProfile Permissions PermissionScopes UnreadablePermissionScopes OwnerScopes MaxMembers NotifyEnabled RemoteControlPolicy ReinjectAfterCompact Mission SourceTemplate Parent RouteGeneration Process Waves Scribe FederationLinks",
	}),
	reflect.TypeFor[dashboardMember](): classifyPeerFields(map[string]string{
		"identity": "AgentID ConvID Title",
		"roster":   "Role",
		"presence": "Online",
		"status":   "State taskRefView",
		"denied":   "CreatedAt Descr agentLocationView repoLinksView tagsView Waking RouteHealth Owner Notify NotifyEffective",
	}),
	reflect.TypeFor[dashboardAgent](): classifyPeerFields(map[string]string{
		"identity": "AgentID ConvID Title Groups",
		"presence": "Online",
		"status":   "State taskRefView",
		"denied":   "agentLocationView repoLinksView tagsView Waking OwnedGroups Effective ActiveSudo Notify NotifyEffective",
	}),
	reflect.TypeFor[taskRefView](): classifyPeerFields(map[string]string{"status": "TaskURL TaskLabel", "denied": "TaskLabelOverride"}),
	reflect.TypeFor[agentState](): classifyPeerFields(map[string]string{
		"status": "Status SubagentCount BgShellCount MonitorCount ContextPct TokensInput TokensOutput ContextWindowSize Model EffortLevel Harness ExitReason RecoveryStatus",
		"denied": "StatusDetail TemporaryHarnessBuiltinMode FastMode LastHook Cwd ContextWindowMax ContextWindowSource CopilotAPI CopilotAPIConnected CodexAppServer CodexAppServerState CodexAppServerHealth CodexAppServerSource CodexAppServerVersion CodexAppServerDetail CodexObserverMode CodexObserverUpdated CopilotAPIChannelFailed AutoCompactWindow CostUSD VirtualCostUSD VirtualCostCredits BrokerRefusals BrokerRefusalDetail BrokerRefusalSince HarnessBuiltinMode HarnessBuiltinModeSource SandboxImplementation OSSandboxState OSSandboxSource OSSandboxUnverified SandboxProfiles SandboxProfilesRecorded SandboxAccessNotices SandboxProfilesOmitted ResourceCgroup ResourceMemoryLimit ResourceCPULimit ResourcePIDsLimit RemoteControl RecoveryDetail RecoveryReason RecoveryCount RecoveryBackoff RecoveryNextAttempt RecoveryLastExitCode",
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
