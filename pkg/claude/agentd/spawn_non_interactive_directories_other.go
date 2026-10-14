//go:build !linux && !darwin

package agentd

func readOneShotBootIdentity() string { return "" }
