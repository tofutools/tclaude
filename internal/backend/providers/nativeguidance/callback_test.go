package nativeguidance

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestCallbackResourceRetainsExactPrivateCredentialForRecovery(t *testing.T) {
	resource, err := PrepareCallback(t.TempDir())
	require.NoError(t, err)
	info, err := os.Stat(resource.CredentialPath())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	ingress := &testIngress{}
	require.NoError(t, resource.Register(context.Background(), ingress, "execution-1", 2, noopHandler{}))
	require.NotEmpty(t, ingress.registration.CredentialDigest)
	evidence := resource.Evidence()
	recovered, err := RecoverCallback(resource.root, evidence)
	require.NoError(t, err)
	require.Equal(t, evidence, recovered.Evidence())
	require.NoError(t, resource.Remove(context.Background()))
	require.Equal(t, 1, ingress.cleanup.closed)
}

type testIngress struct {
	registration ports.CallbackRegistration
	cleanup      testCleanup
}

func (i *testIngress) RegisterCallback(_ context.Context, registration ports.CallbackRegistration) (ports.CallbackBinding, error) {
	i.registration = registration
	i.cleanup = testCleanup{id: registration.RegistrationID, executionID: registration.ExecutionID, attempt: registration.Attempt}
	return ports.CallbackBinding{RegistrationID: registration.RegistrationID, ExecutionID: registration.ExecutionID, Attempt: registration.Attempt, Endpoint: "/tmp/callback.sock", Route: "/v2/provider-callbacks/" + registration.RegistrationID, Cleanup: &i.cleanup}, nil
}

type testCleanup struct {
	id          string
	executionID model.ExecutionID
	attempt     model.AttemptGeneration
	closed      int
}

func (c *testCleanup) RegistrationID() string           { return c.id }
func (c *testCleanup) ExecutionID() model.ExecutionID   { return c.executionID }
func (c *testCleanup) Attempt() model.AttemptGeneration { return c.attempt }
func (c *testCleanup) Close(context.Context) error      { c.closed++; return nil }

type noopHandler struct{}

func (noopHandler) HandleNativeCallback(context.Context, ports.RawNativeCallback, ports.RawNativeCallbackResponder) error {
	return nil
}
