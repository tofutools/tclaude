//go:build !linux && !darwin

package nodeinfo

import "context"

func osVersion(_ context.Context) string { return "" }
