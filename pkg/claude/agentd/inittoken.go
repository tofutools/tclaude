package agentd

import (
	"sync"
	"time"
)

// Init tokens are short-lived, single-use capabilities that the
// loopback HTTP server's cookie-exchange endpoints — the dashboard `/`
// and the approval popup `/approve/{id}` — require before they hand
// out a long-lived session cookie. They are minted only over channels
// the human controls (the peer-cred-authenticated `/v1/dashboard/open`
// endpoint, the in-process tray, and approval creation), never on an
// unauthenticated GET.
//
// Each token carries a scope, and consumeInitToken checks it: a token
// minted for one purpose cannot be redeemed for another, so a popup
// token can never unlock the dashboard's admin surface. The store is
// in-memory only — a daemon restart drops every pending token and the
// human just reopens whatever they were after.
//
// The token does NOT fully close the same-user `/proc`-scrape window:
// the daemon embeds it in the URL it hands to the browser, so it lands
// in the browser launcher's argv. But single-use means a scraper has
// to beat the human's browser (which the daemon launches immediately)
// to the exchange — and a daemon restart, or the legitimate exchange,
// burns it. Preventing a process from reading another's argv is the
// sandbox's job, not tclaude's.

// initScopeDashboard scopes a token to the dashboard `/` exchange.
const initScopeDashboard = "dashboard"

// initScopeApprove scopes a token to one specific approval popup, so a
// token scraped for approval A cannot be replayed against approval B.
func initScopeApprove(approvalID string) string {
	return "approve:" + approvalID
}

type initTokenEntry struct {
	scope     string
	expiresAt time.Time
}

var initTokens = struct {
	mu sync.Mutex
	m  map[string]initTokenEntry
}{m: map[string]initTokenEntry{}}

// initTokenTTL bounds how long a minted token stays valid. The window
// only needs to cover "mint → browser cold-starts → browser GETs the
// exchange URL" — 60s is comfortable even for a WSL→Windows browser
// hand-off, and short enough that a leaked token is near-useless.
const initTokenTTL = 60 * time.Second

// mintInitToken creates a fresh single-use token bound to scope,
// stores it with a TTL, and opportunistically GCs expired entries.
// Safe to call from any goroutine.
func mintInitToken(scope string) string {
	tok := newApprovalID() // 16 random bytes → 32 hex chars; reuses the approval-ID generator
	now := time.Now()
	initTokens.mu.Lock()
	for k, v := range initTokens.m {
		if now.After(v.expiresAt) {
			delete(initTokens.m, k)
		}
	}
	initTokens.m[tok] = initTokenEntry{scope: scope, expiresAt: now.Add(initTokenTTL)}
	initTokens.mu.Unlock()
	return tok
}

// consumeInitToken validates tok and removes it (single-use). Returns
// true only when tok was present, unexpired, and minted for wantScope.
// A present-but-wrong-scope token is still deleted, so probing a token
// against different scopes burns it on the first try.
func consumeInitToken(tok, wantScope string) bool {
	if tok == "" {
		return false
	}
	initTokens.mu.Lock()
	defer initTokens.mu.Unlock()
	v, ok := initTokens.m[tok]
	if !ok {
		return false
	}
	delete(initTokens.m, tok) // single-use: a token never works twice
	return v.scope == wantScope && time.Now().Before(v.expiresAt)
}
