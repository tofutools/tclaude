//go:build linux

package session

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func PrepareNetworkSyncLaunch(spec *TclaudeLayerLaunchSpec, snapshot *sandboxpolicy.Snapshot, sessionID string) error {
	if snapshot == nil || snapshot.ProfilesOmitted {
		return nil
	}
	plan, err := sandboxpolicy.RenderMountPlanWithEngine(spec.Effective, spec.Contract.NetworkEngine)
	if err != nil {
		return err
	}
	if plan.FilteredNetwork == nil || tclaudeLayerPlanDeploysProxy(plan) {
		return nil
	}
	id := uuid.NewString()
	if err := db.RegisterNetworkSyncLaunch(id, sessionID, *snapshot); err != nil {
		return err
	}
	spec.Contract.NetworkSyncID = id
	spec.Contract.NetworkSyncDatabase = db.DBPath()
	return nil
}

func startNetworkSyncLoop(id string, relay *preparedFilteredNetworkRelay, namespacePID int) func() {
	if id == "" || relay.DNSBroker == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := db.BeginNetworkSyncLaunch(id); err != nil {
			relay.DNSBroker.fail(err)
			return
		}
		defer func() { _ = db.FinishNetworkSyncLaunch(id) }()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := db.NetworkSyncHeartbeat(id); err == nil {
				reconcileNetworkSync(id, relay, namespacePID)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

func reconcileNetworkSync(id string, relay *preparedFilteredNetworkRelay, namespacePID int) {
	row, err := db.ReadNetworkSyncLaunch(id)
	if err != nil {
		return
	}
	current := row.Snapshot
	requested := row.Revision > row.Acknowledged && row.Requested != nil
	if requested {
		current = *row.Requested
	} else if row.Automatic {
		current, err = db.ResolveNetworkSyncSnapshot(row.Snapshot)
	} else if row.Status != "starting" {
		return
	}
	if err == nil && !requested && row.Status == "applied" && reflect.DeepEqual(current, row.Snapshot) {
		return
	}
	status := "applied"
	detail := ""
	if err == nil {
		var plan sandboxpolicy.MountPlan
		plan, err = sandboxpolicy.RenderMountPlan(current.Effective)
		if err == nil && (plan.FilteredNetwork == nil || tclaudeLayerPlanDeploysProxy(plan)) {
			status = "restart_required"
			err = fmt.Errorf("network engine or namespace topology changed; restart required")
		}
		if err == nil {
			// Only network authority changes. The launch filesystem, environment,
			// identity and resource limits are never replaced by registry values.
			if !reflect.DeepEqual(relay.Rules, *plan.FilteredNetwork) {
				err = relay.reloadNetworkRules(*plan.FilteredNetwork, namespacePID)
			}
		}
	}
	if err != nil {
		if status != "restart_required" {
			status = "failed"
		}
		detail = err.Error()
		_ = db.AcknowledgeNetworkSync(id, row.Revision, status, detail, nil)
		return
	}
	if err := db.AcknowledgeNetworkSync(id, row.Revision, status, detail, &current); err != nil {
		relay.DNSBroker.fail(fmt.Errorf("record applied network policy: %w", err))
	}
}
