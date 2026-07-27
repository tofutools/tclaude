package testharness

import "sync"

// SandboxLayerBoundary identifies the two host-capability probes used by
// tclaude-layer launches. Interactive panes require the terminal relay;
// relay-free servers use the narrower server boundary.
type SandboxLayerBoundary string

const (
	SandboxLayerInteractive SandboxLayerBoundary = "interactive"
	SandboxLayerServer      SandboxLayerBoundary = "server"
)

// SandboxLayerSim is the flow-test stand-in for tclaude-layer host capability.
// Its zero/default state reports both boundaries available. Tests may set an
// independent result for either boundary and inspect which probe production
// selected.
//
// This sim deliberately models no filesystem, mount, namespace, or network
// behavior. Those contracts belong to the hardware-backed assumptions suite;
// the flow boundary is only capability approval or a named refusal.
type SandboxLayerSim struct {
	mu             sync.Mutex
	interactiveErr error
	serverErr      error
	calls          []SandboxLayerBoundary
}

// SetAvailability sets the result returned by one boundary. A nil error means
// available; a non-nil error is carried verbatim into production's refusal.
func (s *SandboxLayerSim) SetAvailability(boundary SandboxLayerBoundary, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch boundary {
	case SandboxLayerInteractive:
		s.interactiveErr = err
	case SandboxLayerServer:
		s.serverErr = err
	default:
		panic("unknown sandbox layer boundary: " + boundary)
	}
}

// InteractiveAvailability records and answers an interactive-pane probe.
func (s *SandboxLayerSim) InteractiveAvailability() error {
	return s.probe(SandboxLayerInteractive)
}

// ServerAvailability records and answers a relay-free-server probe.
func (s *SandboxLayerSim) ServerAvailability() error {
	return s.probe(SandboxLayerServer)
}

func (s *SandboxLayerSim) probe(boundary SandboxLayerBoundary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, boundary)
	switch boundary {
	case SandboxLayerInteractive:
		return s.interactiveErr
	case SandboxLayerServer:
		return s.serverErr
	default:
		panic("unknown sandbox layer boundary: " + boundary)
	}
}

// Calls returns a snapshot of capability boundaries queried so far, in order.
func (s *SandboxLayerSim) Calls() []SandboxLayerBoundary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SandboxLayerBoundary(nil), s.calls...)
}
