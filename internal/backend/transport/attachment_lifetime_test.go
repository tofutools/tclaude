package transport

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

type reviewProvider struct {
	lifecycleProvider
	canceled chan struct{}
}

func (p *reviewProvider) Prepare(_ context.Context, r ports.PreparationRequest) (ports.PreparedAttempt, error) {
	return &reviewPrepared{preparedLifecycle: preparedLifecycle{p: &p.lifecycleProvider, spec: r.Spec}, canceled: p.canceled}, nil
}

type reviewPrepared struct {
	preparedLifecycle
	canceled chan struct{}
}

func (p *reviewPrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseStarted, Runtime: &reviewRuntime{lifecycleRuntime: lifecycleRuntime{id: p.spec.ExecutionID}, canceled: p.canceled}, Evidence: lifecycleEvidence()}, nil
}

type reviewRuntime struct {
	lifecycleRuntime
	canceled chan struct{}
}

func (r *reviewRuntime) Attach(ctx context.Context, _ ports.AttachmentRequest) (ports.AttachmentResult, error) {
	a, b := net.Pipe()
	// Same lifetime contract as the actual Claude terminal's exec.CommandContext.
	go func() { <-ctx.Done(); a.Close(); b.Close(); close(r.canceled) }()
	return ports.AttachmentResult{Disposition: ports.EffectAccepted, Attachment: pipeAttachment{a}, Evidence: lifecycleEvidence()}, nil
}
func TestAttachmentContextLivesUntilDisconnect(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "b.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := &reviewProvider{canceled: make(chan struct{})}
	service := app.New(store, providers.NewRegistry(p))
	h := testHandler(t, service)
	launch := request(h, "POST", "/v2/launch", `{"request_id":"l","target":{"standalone":{"desired":{"Harness":"test-native","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"unconfined"}}}}`, testCredential)
	if launch.Code != 202 {
		t.Fatal(launch.Body.String())
	}
	snapshot, err := service.Snapshot(context.Background(), app.SnapshotRequest{Principal: model.OperatorPrincipal()})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	defer s.Close()
	req, _ := http.NewRequest("GET", s.URL+"/v2/attach?execution_id="+string(snapshot.Executions[0].ID)+"&request_id=a", nil)
	req.Header.Set("Authorization", "Bearer "+testCredential)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 101 {
		t.Fatalf("status %d", response.StatusCode)
	}
	select {
	case <-p.canceled:
		t.Fatal("attachment canceled before disconnect")
	case <-time.After(100 * time.Millisecond):
	}
	response.Body.Close()
	select {
	case <-p.canceled:
	case <-time.After(time.Second):
		t.Fatal("attachment context survived disconnect")
	}
}
