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
	// A new launch generation supersedes every predecessor capability for this
	// conversation, even when route authority itself remains continuous.
	revokeRouteHelperCredentials(convID, "")
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
	rows, err := db.FindSessionsByConvID(capability.convID)
	if err != nil {
		return false
	}
	// The newest non-exited session row is the live launch authority. A
	// capability from any other generation is stale on read-only discovery too;
	// this closes the predecessor window before a route mutation occurs.
	for _, row := range rows {
		if strings.EqualFold(strings.TrimSpace(row.Status), "exited") {
			continue
		}
		identity, identityErr := db.GetSessionExitLaunchIdentity(row.ID)
		if identityErr == nil && strings.TrimSpace(identity.Generation) != "" {
			return identity.Generation == capability.launchGeneration
		}
		break
	}
	for _, row := range rows {
		identity, identityErr := db.GetSessionExitLaunchIdentity(row.ID)
		if identityErr == nil && identity.Generation == capability.launchGeneration && row.Status == "exited" {
			return false
		}
	}
	return true
}

// routeHelperCapabilityActive is a read-only capability discovery seam for
// dashboard health. It never returns the opaque credential and reuses the
// same exact-generation validation as route requests.
func routeHelperCapabilityActive(agentID, convID, generation string) bool {
	agentID = strings.TrimSpace(agentID)
	convID = strings.TrimSpace(convID)
	generation = strings.TrimSpace(generation)
	if agentID == "" || convID == "" || generation == "" {
		return false
	}
	routeHelperCredentials.RLock()
	var match routeHelperCredential
	for _, capability := range routeHelperCredentials.items {
		if capability.agentID == agentID && capability.convID == convID && capability.launchGeneration == generation {
			match = capability
			break
		}
	}
	routeHelperCredentials.RUnlock()
	return match.agentID != "" && routeHelperCredentialCurrent(match)
}

func requireRouteHelperCredential(w http.ResponseWriter, r *http.Request) (routeHelperCredential, bool) {
	capability, present, valid := routeHelperCredentialForRequest(r)
	if !present || !valid {
		writeRouteError(w, http.StatusUnauthorized, "route_helper_auth", "route helper credential is missing, stale, or invalid")
		return routeHelperCredential{}, false
	}
	return capability, true
}
