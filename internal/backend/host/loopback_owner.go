//go:build linux || darwin

package host

import "fmt"

// LoopbackOwner is a read-only identity proof for a directly started native
// server. Unlike a process-group capability it also works inside a PID namespace
// where the supervisor legitimately owns group 1. It cannot send signals.
type LoopbackOwner struct {
	pid   int
	token string
}

func PinLoopbackOwner(pid int) (LoopbackOwner, error) {
	if pid <= 0 {
		return LoopbackOwner{}, fmt.Errorf("invalid loopback owner PID")
	}
	token, err := processStartToken(pid)
	if err != nil {
		return LoopbackOwner{}, err
	}
	if token == "" {
		return LoopbackOwner{}, fmt.Errorf("loopback owner identity unavailable")
	}
	return LoopbackOwner{pid: pid, token: token}, nil
}

func (p LoopbackOwner) Owns(port int) (bool, error) {
	if p.pid <= 0 || p.token == "" || port < 1 || port > 65535 {
		return false, fmt.Errorf("invalid loopback ownership proof")
	}
	token, err := processStartToken(p.pid)
	if err != nil || token != p.token {
		return false, err
	}
	owned, err := processTreeOwnsLoopbackPort(p.pid, port)
	if err != nil || !owned {
		return false, err
	}
	token, err = processStartToken(p.pid)
	return err == nil && token == p.token, err
}
