//go:build !linux && !darwin

package agentd

import "net/http"

type stageStatic struct{ http.RoundTripper }

type stageStaticGuard interface{ hidden(abs string) bool }

func newStageStatic(string, string, func(), stageStaticGuard) (*stageStatic, error) {
	return nil, stageSourceErrorf("static live pages need a Unix host")
}

func stageStaticReadBack(http.RoundTripper) bool { return false }
