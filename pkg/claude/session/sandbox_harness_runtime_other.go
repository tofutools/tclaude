//go:build !darwin

package session

func tclaudeLayerHarnessRuntimeWriteDirs(string) ([]string, error) {
	return nil, nil
}
