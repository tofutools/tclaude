//go:build !linux

// Package nsroot is Linux-only: other platforms have no mount namespaces.
package nsroot

import "errors"

// Root is never opened off Linux.
type Root struct{}

// ForeignCandidates finds nothing off Linux.
func ForeignCandidates([]int) (string, []int) { return "", nil }

// Open always fails off Linux.
func Open(int, string) (*Root, error) { return nil, errors.New("mount namespaces are Linux-only") }

// Alive is false off Linux.
func (r *Root) Alive() bool { return false }

// NS is empty off Linux.
func (r *Root) NS() string { return "" }

// OpenDir always fails off Linux.
func (r *Root) OpenDir(string) (int, error) { return -1, errors.New("mount namespaces are Linux-only") }

// Close does nothing off Linux.
func (r *Root) Close() {}
