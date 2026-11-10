package agent

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

// A page reference is <name> for one of your own pages, <owner>/<name> for
// one shared with you; either way any query the verb adds is kept.
func TestPageAPIPath(t *testing.T) {
	assert.Equal(t, "/v1/pages/app/marks", pageAPIPath("app", "/marks", nil))
	assert.Equal(t, "/v1/pages/app/marks?owner=agt_1", pageAPIPath("agt_1/app", "/marks", nil))
	assert.Equal(t, "/v1/pages/app/patches?n=3&owner=agt_1", pageAPIPath("agt_1/app", "/patches", url.Values{"n": {"3"}}))
	assert.Equal(t, "/v1/pages/app?owner=agt_1", pageAPIPath("agt_1/app", "", nil))
	assert.Equal(t, "/v1/pages/app/marks?since=2", pageAPIPath("app", "/marks", url.Values{"since": {"2"}}))
}
