package agentd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const routeHelperCredentialHeader = "X-Tclaude-Route-Helper-Credential"

type routeHelperCredential struct {
	agentID          string
	convID           string
	launchGeneration string
	issuedAt         time.Time
}

var routeHelperCredentials = struct {
	sync.RWMutex
	items map[string]routeHelperCredential
}{items: make(map[string]routeHelperCredential)}

// mintRouteHelperCredential creates a daemon-local capability for the sibling
// namespace helper. It is intentionally not persisted: an agentd restart
// invalidates every outstanding helper capability.
func mintRouteHelperCredential(agentID, convID string) (credential, launchGeneration string, err error) {
	agentID = strings.TrimSpace(agentID)
	convID = strings.TrimSpace(convID)
	if agentID == "" || convID == "" {
		return "", "", errors.New("route helper credential identity is incomplete")
	}
	credential, err = randomRouteHelperSecret(32)
	if err != nil {
		return "", "", err
	}
	launchGeneration, err = randomRouteHelperSecret(16)
	if err != nil {
		return "", "", err
	}
	routeHelperCredentials.Lock()
	routeHelperCredentials.items[credential] = routeHelperCredential{
		agentID: agentID, convID: convID, launchGeneration: launchGeneration,
		issuedAt: time.Now().UTC(),
	}
	routeHelperCredentials.Unlock()
	return credential, launchGeneration, nil
}

func randomRouteHelperSecret(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func revokeRouteHelperCredentials(convID, launchGeneration string) {
	convID = strings.TrimSpace(convID)
	launchGeneration = strings.TrimSpace(launchGeneration)
	routeHelperCredentials.Lock()
	defer routeHelperCredentials.Unlock()
	for token, capability := range routeHelperCredentials.items {
		if capability.convID == convID && (launchGeneration == "" || capability.launchGeneration == launchGeneration) {
			delete(routeHelperCredentials.items, token)
		}
	}
}

// routeHelperCredentialForRequest authenticates only the opaque capability.
// The identity headers remain descriptive cross-checks; they are never used as
// proof. Route membership and route permissions are checked by the caller.
func routeHelperCredentialForRequest(r *http.Request) (routeHelperCredential, bool, bool) {
	token := strings.TrimSpace(r.Header.Get(routeHelperCredentialHeader))
	if token == "" {
		return routeHelperCredential{}, false, false
	}
	routeHelperCredentials.RLock()
	capability, ok := routeHelperCredentials.items[token]
	routeHelperCredentials.RUnlock()
	if !ok || !routeHelperCredentialCurrent(capability) {
		if ok {
			revokeRouteHelperCredentials(capability.convID, capability.launchGeneration)
		}
		return routeHelperCredential{}, true, false
	}
	return capability, true, true
}

func routeHelperCredentialCurrent(capability routeHelperCredential) bool {
	if capability.convID == "" || capability.agentID == "" || capability.launchGeneration == "" {
		return false
	}
	// A resume capability is minted before the replacement session row exists.
	// Do not compare it to the predecessor row here: doing so would consume the
	// capability during that launch window. The route broker performs the same
	// generation check when a route is actually attached, and the old
	// capability is revoked by the SessionEnd/reaper path.
	rows, err := db.FindSessionsByConvID(capability.convID)
	if err != nil {
		return false
	}
	for _, row := range rows {
		identity, identityErr := db.GetSessionExitLaunchIdentity(row.ID)
		if identityErr == nil && identity.Generation == capability.launchGeneration && row.Status == "exited" {
			return false
		}
	}
	return true
}

func requireRouteHelperCredential(w http.ResponseWriter, r *http.Request) (routeHelperCredential, bool) {
	capability, present, valid := routeHelperCredentialForRequest(r)
	if !present || !valid {
		writeRouteError(w, http.StatusUnauthorized, "route_helper_auth", "route helper credential is missing, stale, or invalid")
		return routeHelperCredential{}, false
	}
	return capability, true
}
