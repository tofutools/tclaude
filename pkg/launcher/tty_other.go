//go:build !linux && !darwin

package launcher

import "errors"

func hasControllingTTY() (bool, error) { return false, errors.New("not supported") }
