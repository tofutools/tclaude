//go:build !linux && !darwin

package resumeprovenance

import (
	"fmt"
	"os"
)

func platformFileIdentity(info os.FileInfo) (uint64, uint64, error) {
	return 0, 0, fmt.Errorf("resume provenance is unsupported on this platform")
}
