//go:build !darwin

package session

func tclaudeLayerHarnessRuntimeWriteDirs(string, bool) ([]string, error) {
	return nil, nil
}
