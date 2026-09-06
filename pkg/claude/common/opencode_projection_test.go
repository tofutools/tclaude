package common

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOpenCodeLaunchProjectionMarker(t *testing.T) {
	const (
		serverURL = "http://127.0.0.1:43210"
		password  = "private-password"
	)
	marker := OpenCodeLaunchProjectionMarker(serverURL, password)
	assert.Equal(t, marker, OpenCodeLaunchProjectionMarker(" "+serverURL+" ", password))
	assert.NotEqual(t, marker, OpenCodeLaunchProjectionMarker(serverURL, " "+password+" "))
	assert.True(t, strings.HasPrefix(marker, openCodeLaunchProjectionMarkerPrefix))
	assert.NotContains(t, marker, serverURL)
	assert.NotContains(t, marker, password)
	assert.NotEqual(t, marker, OpenCodeLaunchProjectionMarker(serverURL, "replacement-password"))
	assert.Empty(t, OpenCodeLaunchProjectionMarker("", password))
	assert.Empty(t, OpenCodeLaunchProjectionMarker(serverURL, ""))
}
