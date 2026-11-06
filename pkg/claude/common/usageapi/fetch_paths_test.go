package usageapi

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The usage fetch paths share two rules (awb tcl-9ggcr7):
//   - source fallback: an auth rejection moves on to the next credential
//     source; any other error, a 429 included, stops at once;
//   - 429 refresh: only with TCLAUDE_DEBUG_REFRESH=1, a 429 refreshes the
//     token and retries once; a failed refresh returns the original 429.
// These pin both rules on the parsed path (FetchUsage), the raw path
// (FetchUsageRaw, behind `usage --json`), and the single-token path
// (FetchWithRetry, refresh only).

// fetchPath is one entry point, run against a stubbed fetch.
type fetchPath struct {
	name string
	// sources reports whether the path falls back across credential sources.
	sources bool
	// run stubs the path's fetch with fetch and calls it.
	run func(t *testing.T, fetch func(token string) error) error
}

func fetchPaths() []fetchPath {
	return []fetchPath{
		{"parsed", true, func(t *testing.T, fetch func(string) error) error {
			prev := fetchFunc
			fetchFunc = func(token string) (*Response, error) {
				if err := fetch(token); err != nil {
					return nil, err
				}
				return &Response{}, nil
			}
			t.Cleanup(func() { fetchFunc = prev })
			resp, err := FetchUsage()
			if err == nil {
				require.NotNil(t, resp)
			}
			return err
		}},
		{"raw", true, func(t *testing.T, fetch func(string) error) error {
			prev := fetchRawFunc
			fetchRawFunc = func(token string) ([]byte, error) {
				if err := fetch(token); err != nil {
					return nil, err
				}
				return []byte(`{}`), nil
			}
			t.Cleanup(func() { fetchRawFunc = prev })
			body, err := FetchUsageRaw()
			if err == nil {
				require.Equal(t, `{}`, string(body))
			}
			return err
		}},
		{"single token", false, func(t *testing.T, fetch func(string) error) error {
			prev := fetchFunc
			fetchFunc = func(token string) (*Response, error) {
				if err := fetch(token); err != nil {
					return nil, err
				}
				return &Response{}, nil
			}
			t.Cleanup(func() { fetchFunc = prev })
			_, err := FetchWithRetry("first")
			return err
		}},
	}
}

func twoSources(t *testing.T) {
	stubCandidates(t, func() []tokenCandidate {
		return []tokenCandidate{{token: "first", store: storeFile}, {token: "second", store: storeMacKeychain}}
	})
}

func stubRefresh(t *testing.T, token string, err error) *int {
	calls := 0
	prev := refreshTokenFunc
	refreshTokenFunc = func() (string, error) {
		calls++
		return token, err
	}
	t.Cleanup(func() { refreshTokenFunc = prev })
	return &calls
}

// script answers each call with the next error (nil is success) and records
// the tokens it was called with.
func script(errs ...error) (func(string) error, *[]string) {
	var tokens []string
	return func(token string) error {
		tokens = append(tokens, token)
		if len(errs) == 0 {
			return fmt.Errorf("unexpected call %d", len(tokens))
		}
		err := errs[0]
		errs = errs[1:]
		return err
	}, &tokens
}

var (
	errAuth  = &AuthError{Status: 401, Body: "expired"}
	errAuth2 = &AuthError{Status: 403, Body: "revoked"}
	err429   = &RateLimitError{Body: "slow down"}
	err500   = errors.New("API returned 500: server error")
)

func TestFetchPathsFallBackAcrossSourcesOnlyOnAuth(t *testing.T) {
	for _, p := range fetchPaths() {
		if !p.sources {
			continue
		}
		t.Run(p.name+"/auth then ok", func(t *testing.T) {
			setupTestCache(t)
			twoSources(t)
			fetch, tokens := script(errAuth, nil)
			require.NoError(t, p.run(t, fetch))
			assert.Equal(t, []string{"first", "second"}, *tokens)
		})
		t.Run(p.name+"/every source rejected returns the last", func(t *testing.T) {
			setupTestCache(t)
			twoSources(t)
			fetch, tokens := script(errAuth, errAuth2)
			err := p.run(t, fetch)
			assert.Same(t, errAuth2, err)
			assert.Equal(t, []string{"first", "second"}, *tokens)
		})
		for name, stop := range map[string]error{"429": err429, "500": err500} {
			t.Run(p.name+"/"+name+" stops at the first source", func(t *testing.T) {
				setupTestCache(t)
				twoSources(t)
				refreshes := stubRefresh(t, "refreshed", nil)
				fetch, tokens := script(stop)
				assert.Same(t, stop, p.run(t, fetch))
				assert.Equal(t, []string{"first"}, *tokens)
				assert.Zero(t, *refreshes, "no refresh without TCLAUDE_DEBUG_REFRESH")
			})
		}
		t.Run(p.name+"/no sources", func(t *testing.T) {
			setupTestCache(t)
			stubCandidates(t, func() []tokenCandidate { return nil })
			fetch, tokens := script()
			assert.EqualError(t, p.run(t, fetch), errNoCredentials().Error())
			assert.Empty(t, *tokens)
		})
	}
}

func TestFetchPathsRefreshOn429OnlyWhenOptedIn(t *testing.T) {
	refreshFailed := errors.New("refresh failed")
	for _, p := range fetchPaths() {
		t.Run(p.name+"/refresh then retry succeeds", func(t *testing.T) {
			setupTestCache(t)
			twoSources(t)
			t.Setenv("TCLAUDE_DEBUG_REFRESH", "1")
			refreshes := stubRefresh(t, "refreshed", nil)
			fetch, tokens := script(err429, nil)
			require.NoError(t, p.run(t, fetch))
			assert.Equal(t, []string{"first", "refreshed"}, *tokens)
			assert.Equal(t, 1, *refreshes)
		})
		t.Run(p.name+"/a failed refresh returns the 429", func(t *testing.T) {
			setupTestCache(t)
			twoSources(t)
			t.Setenv("TCLAUDE_DEBUG_REFRESH", "1")
			stubRefresh(t, "", refreshFailed)
			fetch, tokens := script(err429)
			assert.Same(t, err429, p.run(t, fetch))
			assert.Equal(t, []string{"first"}, *tokens)
		})
		t.Run(p.name+"/a failed retry returns its error", func(t *testing.T) {
			setupTestCache(t)
			twoSources(t)
			t.Setenv("TCLAUDE_DEBUG_REFRESH", "1")
			stubRefresh(t, "refreshed", nil)
			fetch, tokens := script(err429, err500)
			assert.Same(t, err500, p.run(t, fetch))
			assert.Equal(t, []string{"first", "refreshed"}, *tokens)
		})
		t.Run(p.name+"/not opted in", func(t *testing.T) {
			setupTestCache(t)
			twoSources(t)
			t.Setenv("TCLAUDE_DEBUG_REFRESH", "")
			refreshes := stubRefresh(t, "refreshed", nil)
			fetch, _ := script(err429)
			assert.Same(t, err429, p.run(t, fetch))
			assert.Zero(t, *refreshes)
		})
		for name, stop := range map[string]error{"500": err500, "auth": errAuth} {
			t.Run(p.name+"/a "+name+" is not refreshed", func(t *testing.T) {
				setupTestCache(t)
				stubCandidates(t, func() []tokenCandidate { return []tokenCandidate{{token: "first", store: storeFile}} })
				t.Setenv("TCLAUDE_DEBUG_REFRESH", "1")
				refreshes := stubRefresh(t, "refreshed", nil)
				fetch, _ := script(stop)
				assert.Same(t, stop, p.run(t, fetch))
				assert.Zero(t, *refreshes)
			})
		}
	}
}
