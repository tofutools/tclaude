//go:build !linux

package jstest

// guardSuiteMemory is Linux-only: it reads /proc. Elsewhere it guards nothing.
func guardSuiteMemory(int, int64) (stop func() []string) {
	return func() []string { return nil }
}
