package transport

import (
	"net/http"
	"testing"
)

// Regression retained from the independent cold review of launch briefs.
func TestLaunchRejectsInvalidUTF8InitialMessage(t *testing.T) {
	p := &commandProbe{}
	body := `{"request_id":"request-invalid-utf8","initial_message":"` + string([]byte{0xff}) + `","target":{"standalone":{"desired":{"Harness":"claude","WorkingDirectory":"/work"}}}}`
	w := request(testHandler(t, p), http.MethodPost, "/v2/launch", body, testCredential)
	if w.Code != http.StatusBadRequest || p.launchCalls != 0 {
		t.Fatalf("invalid UTF-8 was normalized and admitted: status=%d calls=%d body=%s initial=%q", w.Code, p.launchCalls, w.Body.String(), p.launch.InitialMessage)
	}
}
