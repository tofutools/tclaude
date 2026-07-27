package testharness

import (
	"errors"
	"testing"
)

func TestSandboxLayerSimDefaultsAvailableAndRecordsBoundaries(t *testing.T) {
	sim := &SandboxLayerSim{}

	if err := sim.InteractiveAvailability(); err != nil {
		t.Fatalf("default interactive availability: %v", err)
	}
	if err := sim.ServerAvailability(); err != nil {
		t.Fatalf("default server availability: %v", err)
	}
	want := []SandboxLayerBoundary{SandboxLayerInteractive, SandboxLayerServer}
	assertSandboxLayerCalls(t, sim.Calls(), want)
}

func TestSandboxLayerSimKeepsBoundaryResultsIndependent(t *testing.T) {
	sim := &SandboxLayerSim{}
	interactiveErr := errors.New("interactive relay unavailable")
	sim.SetAvailability(SandboxLayerInteractive, interactiveErr)

	if err := sim.InteractiveAvailability(); !errors.Is(err, interactiveErr) {
		t.Fatalf("interactive availability error = %v, want %v", err, interactiveErr)
	}
	if err := sim.ServerAvailability(); err != nil {
		t.Fatalf("server availability inherited interactive error: %v", err)
	}

	serverErr := errors.New("server namespace unavailable")
	sim.SetAvailability(SandboxLayerServer, serverErr)
	sim.SetAvailability(SandboxLayerInteractive, nil)
	if err := sim.InteractiveAvailability(); err != nil {
		t.Fatalf("cleared interactive availability: %v", err)
	}
	if err := sim.ServerAvailability(); !errors.Is(err, serverErr) {
		t.Fatalf("server availability error = %v, want %v", err, serverErr)
	}
}

func TestSandboxLayerSimCallsReturnsSnapshot(t *testing.T) {
	sim := &SandboxLayerSim{}
	_ = sim.InteractiveAvailability()

	calls := sim.Calls()
	calls[0] = SandboxLayerServer
	assertSandboxLayerCalls(t, sim.Calls(), []SandboxLayerBoundary{SandboxLayerInteractive})
}

func assertSandboxLayerCalls(
	t *testing.T,
	got, want []SandboxLayerBoundary,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	}
}
