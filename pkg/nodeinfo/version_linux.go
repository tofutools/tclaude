package nodeinfo

import (
	"context"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"os"
	"strings"
)

func osVersion(_ context.Context) string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return proto.SafeName(strings.Trim(value, "\"'"), false)
		}
	}
	return ""
}
