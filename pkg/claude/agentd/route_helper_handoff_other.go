//go:build !linux

package agentd

import "fmt"

func prepareRouteHelperCredentialHandoff(string) (string, func(), error) {
	return "", func() {}, fmt.Errorf("route helper credential handoff is only available on Linux")
}
