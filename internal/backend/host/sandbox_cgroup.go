//go:build linux || darwin

package host

import (
	"errors"
	"syscall"
	"time"
)

const sandboxResourceChildMarker = "TCLAUDE_SANDBOX_RESOURCE_CHILD"

// sandboxCgroup is private host evidence, never an operator profile identity.
// A launch keeps its own kernel boundary while later launches resolve current
// profile values independently.
type sandboxCgroup struct {
	Path   string
	Device uint64
	Inode  uint64
	Memory string
	CPU    string
}

func (c *sandboxCgroup) settle() error {
	if err := c.kill(); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Second)
	for {
		err := c.remove()
		if err == nil || !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
