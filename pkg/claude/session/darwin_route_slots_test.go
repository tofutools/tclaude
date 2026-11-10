package session

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestValidateDarwinRouteSlotsIsBoundedAndExact(t *testing.T) {
	if err := ValidateDarwinRouteSlots([]int{41001, 41002}); err != nil {
		t.Fatalf("valid route slots rejected: %v", err)
	}
	for _, slots := range [][]int{{0}, {65536}, {41001, 41001}} {
		if err := ValidateDarwinRouteSlots(slots); err == nil {
			t.Fatalf("invalid route slots %v were accepted", slots)
		}
	}
	tooMany := make([]int, DarwinRouteSlotCountMax+1)
	for i := range tooMany {
		tooMany[i] = 41001 + i
	}
	if err := ValidateDarwinRouteSlots(tooMany); err == nil {
		t.Fatal("route pool above the configured bound was accepted")
	}
}

func TestParseDarwinRouteSlotsRequiresAnExactBoundedPool(t *testing.T) {
	slots, err := ParseDarwinRouteSlots(" 41001,41002 ")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(slots), "[41001 41002]"; got != want {
		t.Fatalf("parsed slots = %s, want %s", got, want)
	}
	for _, raw := range []string{"", "41001,", "41001,41001", "41001,not-a-port"} {
		if _, err := ParseDarwinRouteSlots(raw); err == nil {
			t.Fatalf("malformed route pool %q was accepted", raw)
		}
	}
}

func TestReserveDarwinRouteSlotsAtFailsLoudlyOnCollision(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	if _, err := ReserveDarwinRouteSlotsAt([]int{port}); err == nil {
		t.Fatalf("occupied route slot %d was reserved", port)
	}
}

func TestReserveDarwinRouteSlotsAtIsAllOrNothing(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	free, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := free.Addr().(*net.TCPAddr).Port
	free.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port
	if _, err := ReserveDarwinRouteSlotsAt([]int{freePort, occupiedPort}); err == nil {
		t.Fatal("partial route slot reservation unexpectedly succeeded")
	}
	probe, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(freePort))
	if err != nil {
		t.Fatalf("partial reservation leaked free slot %d: %v", freePort, err)
	}
	probe.Close()
}

func TestReserveDarwinRouteSlotsUsesBoundedConfiguredPool(t *testing.T) {
	t.Setenv(darwinRouteSlotCountEnv, "3")
	reservation, err := ReserveDarwinRouteSlots()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reservation.Slots()); got != 3 {
		t.Fatalf("allocated %d route slots, want 3", got)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := DarwinRouteSlotCount(); err != nil {
		t.Fatal(err)
	}
}

// closeFailListener is a reservation listener whose Close fails, the only
// way DarwinRouteSlotReservation.Release can fail.
type closeFailListener struct{ net.Listener }

func (closeFailListener) Close() error { return errors.New("close failed") }

func TestHandOffDarwinRouteAtLaunchKillsThePaneOnEveryFailure(t *testing.T) {
	cases := []struct {
		name        string
		activateErr error
		listener    net.Listener
		wantErr     string
		wantKilled  bool
	}{
		{name: "activate fails", activateErr: errors.New("db down"),
			wantErr: "activate Darwin route launch contract", wantKilled: true},
		// The drift this pins: the release arm used to return without
		// killing the pane, stranding it under a rolled-back launch.
		{name: "release fails", listener: closeFailListener{},
			wantErr: "release Darwin route launch reservation", wantKilled: true},
		{name: "both succeed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &DarwinRouteSlotReservation{}
			if tc.listener != nil {
				r.listeners = []net.Listener{tc.listener}
			}
			killed := false
			err := handOffDarwinRouteAtLaunch(r,
				func() error { return tc.activateErr },
				func() { killed = true })
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
			if killed != tc.wantKilled {
				t.Fatalf("pane killed = %v, want %v", killed, tc.wantKilled)
			}
		})
	}
}
