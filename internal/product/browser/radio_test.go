package browser

import (
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type radioRoundTrip func(*http.Request) (*http.Response, error)

func (f radioRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRadioMetadataRequiresSessionAndFixedStation(t *testing.T) {
	calls := 0
	s := &Server{origin: "http://127.0.0.1:1234", cookieName: "test", session: "secret", sessionUntil: time.Now().Add(time.Hour)}
	s.radio.client = &http.Client{Transport: radioRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "https://somafm.com/songs/thistle.xml", r.URL.String())
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`<?xml version="1.0" encoding="ISO-8859-1"?><songs><song><title>A &amp; B</title><artist>Fiddler</artist><album>Tavern</album><date>123</date></song></songs>`))}, nil
	})}
	request := func(path string, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", s.origin+path, nil)
		if authenticated {
			r.AddCookie(&http.Cookie{Name: "test", Value: "secret"})
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	require.Equal(t, 401, request("/radio/now-playing?channel=thistle", false).Code)
	require.Equal(t, 400, request("/radio/now-playing?channel=https://example.com", true).Code)
	require.Zero(t, calls)
	w := request("/radio/now-playing?channel=thistle", true)
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"artist":"Fiddler"`)
	require.Contains(t, w.Body.String(), `"title":"A \u0026 B"`)
	require.Equal(t, 200, request("/radio/now-playing?channel=thistle", true).Code)
	require.Equal(t, 1, calls)
}
