package buildversion

import "github.com/tofutools/tclaude/pkg/federation/proto"

// InstallMethod is stamped only by the official release pipeline. Ordinary
// source and go-install builds intentionally leave it empty.
var InstallMethod string

type Info struct {
	Version         string `json:"version"`
	InstallMethod   string `json:"install_method"`
	ProtocolVersion int    `json:"protocol_version"`
}

func BuildInfo() Info {
	return Info{Version: AppVersion(), InstallMethod: InstallMethod, ProtocolVersion: proto.ProtocolVersion}
}
