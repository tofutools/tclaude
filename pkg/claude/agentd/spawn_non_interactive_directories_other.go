//go:build !linux && !darwin

package agentd

import "fmt"

func readOneShotBootIdentity() string { return "" }

func readOneShotOwnerIdentity(int) (string, error) {
	return "", fmt.Errorf("foreground owner identity unsupported")
}
