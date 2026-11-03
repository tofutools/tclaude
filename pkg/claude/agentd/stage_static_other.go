//go:build !unix

package agentd

import "net/http"

type stageStatic struct{ http.RoundTripper }

func newStageStatic(string, string, func()) (*stageStatic, error) {
	return nil, stageSourceErrorf("static live pages need a Unix host")
}
