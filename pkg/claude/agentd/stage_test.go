package agentd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStageHostToken(t *testing.T) {
	tok := strings.Repeat("ab", 16)
	for _, c := range []struct {
		host    string
		token   string
		isStage bool
	}{
		{"f-" + tok + ".ui.localhost:8080", tok, true},
		{"F-" + strings.ToUpper(tok) + ".Ui.LocalHost.", tok, true},
		{"ui.localhost", "", true},
		{"x.f-" + tok + ".ui.localhost", "", true},
		{"f-" + tok[:30] + ".ui.localhost", "", true},
		{"f-" + tok + "zz.ui.localhost", "", true},
		{"127.0.0.1:8080", "", false},
		{"localhost:8080", "", false},
		{"f-" + tok + ".ui.localhost.evil.com", "", false},
		{"evilui.localhost", "", false},
	} {
		token, isStage := stageHostToken(c.host)
		assert.Equal(t, c.token, token, c.host)
		assert.Equal(t, c.isStage, isStage, c.host)
	}
}

func TestStageLoopbackPortRefusesNetworkConnections(t *testing.T) {
	req := func(local net.Addr, remote string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if local != nil {
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
		}
		r.RemoteAddr = remote
		return r
	}
	lo := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7000}
	lan := &net.TCPAddr{IP: net.IPv4(192, 168, 1, 5), Port: 7000}

	port, ok := stageLoopbackPort(req(lo, "127.0.0.1:5555"))
	assert.True(t, ok)
	assert.Equal(t, 7000, port)
	_, ok = stageLoopbackPort(req(lan, "192.168.1.9:5555"))
	assert.False(t, ok)
	_, ok = stageLoopbackPort(req(lo, "192.168.1.9:5555"))
	assert.False(t, ok)
	_, ok = stageLoopbackPort(req(nil, "127.0.0.1:5555"))
	assert.False(t, ok, "no listener address: refuse")
}

func TestStageProbeHasNoScriptClose(t *testing.T) {
	assert.NotContains(t, strings.ToLower(string(stageProbeJS)), "</script")
}
