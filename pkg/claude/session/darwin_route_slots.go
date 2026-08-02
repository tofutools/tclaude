package session

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

const (
	// DarwinRouteSlotCountDefault is the small, fixed-capacity pool used by
	// route-capable launches unless the caller supplies an explicit pool.
	DarwinRouteSlotCountDefault = 8
	DarwinRouteSlotCountMax     = 16
	darwinRouteSlotCountEnv     = "TCLAUDE_DARWIN_ROUTE_SLOT_COUNT"
	darwinRouteSlotsEnv         = "TCLAUDE_DARWIN_ROUTE_SLOTS"
)

// DarwinRouteSlotCount resolves the bounded operator-configurable pool size.
// Invalid configuration fails closed rather than silently changing the
// launch contract.
func DarwinRouteSlotCount() (int, error) {
	raw := strings.TrimSpace(os.Getenv(darwinRouteSlotCountEnv))
	if raw == "" {
		return DarwinRouteSlotCountDefault, nil
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < 1 || size > DarwinRouteSlotCountMax {
		return 0, fmt.Errorf("%s must be an integer from 1 to %d, got %q", darwinRouteSlotCountEnv, DarwinRouteSlotCountMax, raw)
	}
	return size, nil
}

// ValidateDarwinRouteSlots validates exact TCP ports admitted by a Seatbelt
// launch. It intentionally does not scan the host or retry collisions.
func ValidateDarwinRouteSlots(slots []int) error {
	if len(slots) > DarwinRouteSlotCountMax {
		return fmt.Errorf("Darwin route slot pool has %d entries; maximum is %d", len(slots), DarwinRouteSlotCountMax)
	}
	seen := make(map[int]struct{}, len(slots))
	for _, port := range slots {
		if port < 1 || port > 65535 {
			return fmt.Errorf("Darwin route slot port %d is outside TCP port range", port)
		}
		if _, ok := seen[port]; ok {
			return fmt.Errorf("Darwin route slot port %d is duplicated", port)
		}
		seen[port] = struct{}{}
	}
	return nil
}

// DarwinRouteSlotsEnv is the observation-only launch environment key through
// which a sandboxed agent learns its exact pre-authorized pool. Route registry
// authority and leasing remain in agentd.
const DarwinRouteSlotsEnv = darwinRouteSlotsEnv

// EncodeDarwinRouteSlots returns the launch environment value for validated
// ports.
func EncodeDarwinRouteSlots(slots []int) (string, error) {
	if err := ValidateDarwinRouteSlots(slots); err != nil {
		return "", err
	}
	values := make([]string, len(slots))
	for i, port := range slots {
		values[i] = strconv.Itoa(port)
	}
	return strings.Join(values, ","), nil
}

// DarwinRouteSlotReservation holds exact TCP listeners while a Seatbelt
// profile is rendered and the child is admitted.
type DarwinRouteSlotReservation struct {
	slots     []int
	listeners []net.Listener
}

func (r *DarwinRouteSlotReservation) Slots() []int {
	if r == nil {
		return nil
	}
	return append([]int(nil), r.slots...)
}

func (r *DarwinRouteSlotReservation) Release() error {
	if r == nil {
		return nil
	}
	var first error
	for _, listener := range r.listeners {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) && first == nil {
			first = err
		}
	}
	r.listeners = nil
	return first
}

// ReserveDarwinRouteSlotsAt reserves exactly the supplied ports in one
// bounded operation. Partial reservation is rolled back and returned as a
// collision error; no global scan or retry is performed.
func ReserveDarwinRouteSlotsAt(slots []int) (*DarwinRouteSlotReservation, error) {
	if err := ValidateDarwinRouteSlots(slots); err != nil {
		return nil, err
	}
	reservation := &DarwinRouteSlotReservation{slots: append([]int(nil), slots...)}
	for _, port := range slots {
		listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			_ = reservation.Release()
			return nil, fmt.Errorf("reserve Darwin route slot %d: %w", port, err)
		}
		reservation.listeners = append(reservation.listeners, listener)
	}
	return reservation, nil
}
