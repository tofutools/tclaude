package session

import (
	"net"
	"strconv"
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
