//go:build !linux

package agentd

import "net/http"

func registerV1RouteAdapter(_ *http.ServeMux) {}
