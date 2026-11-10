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

// Bound the interval in which a seed-based harness may enroll. A gated launch
// left beyond this window must be resumed to receive a new helper capability.
const routeHelperEnrollmentTTL = 15 * time.Minute

type routeHelperCredential struct {
	agentID          string
	convID           string
	launchGeneration string
	issuedAt         time.Time
	pendingSessionID string
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
	if agentID == "" {
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
	if convID != "" {
		revokeRouteHelperCredentials(convID, "")
	}
	routeHelperCredentials.Lock()
	for token, old := range routeHelperCredentials.items {
		if old.agentID == agentID || old.convID == "" && time.Since(old.issuedAt) > routeHelperEnrollmentTTL {
			delete(routeHelperCredentials.items, token)
		}
	}
	routeHelperCredentials.items[credential] = routeHelperCredential{
		agentID: agentID, convID: convID, launchGeneration: launchGeneration,
		issuedAt: time.Now().UTC(),
	}
	routeHelperCredentials.Unlock()
	return credential, launchGeneration, nil
}

// stageRouteHelperCredential also records the daemon-owned launch label so a
// dropped predecessor reservation cannot revoke a replacement launch's helper.
func stageRouteHelperCredential(agentID, sessionID string) (string, string, error) {
	token, generation, err := mintRouteHelperCredential(agentID, "")
	if err != nil {
		return "", "", err
	}
	routeHelperCredentials.Lock()
	capability, ok := routeHelperCredentials.items[token]
	if ok {
		capability.pendingSessionID = sessionID
		routeHelperCredentials.items[token] = capability
	}
	routeHelperCredentials.Unlock()
	return token, generation, nil
}

// attachRouteHelperConversation is called only after daemon-owned spawn enrollment.
// An unbound capability never authenticates requests, including discovery.
func attachRouteHelperConversation(agentID, convID, launchGeneration string) {
	bound, err := db.AgentIDForConv(convID)
	if err != nil || bound != agentID || agentID == "" || launchGeneration == "" {
		return
	}
	routeHelperCredentials.Lock()
	defer routeHelperCredentials.Unlock()
	for token, capability := range routeHelperCredentials.items {
		if capability.agentID == agentID && capability.convID == "" && capability.launchGeneration == launchGeneration {
			if time.Since(capability.issuedAt) > routeHelperEnrollmentTTL {
				delete(routeHelperCredentials.items, token)
				continue
			}
			capability.convID = convID
			routeHelperCredentials.items[token] = capability
		}
	}
}

// The wrapper persists the helper generation on its exact session row before
// starting the pane, so sweeper enrollment survives without trusting helper input.
func routeHelperGenerationForSession(sessionID string) string {
	identity, err := db.GetSessionExitLaunchIdentity(sessionID)
	if err != nil {
		return ""
	}
	return identity.Generation
}

func revokePendingRouteHelperCredentials(agentID, sessionID string) {
	routeHelperCredentials.Lock()
	defer routeHelperCredentials.Unlock()
	for token, capability := range routeHelperCredentials.items {
		if capability.convID == "" && capability.agentID == agentID && capability.pendingSessionID == sessionID {
			delete(routeHelperCredentials.items, token)
		}
	}
}

func hasRouteHelperForConversation(agentID, convID string) bool {
	routeHelperCredentials.RLock()
	defer routeHelperCredentials.RUnlock()
	for _, capability := range routeHelperCredentials.items {
		if capability.agentID == agentID && capability.convID == convID && routeHelperCredentialCurrent(capability) {
			return true
		}
	}
	return false
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
		if ok && (capability.convID != "" || time.Since(capability.issuedAt) > routeHelperEnrollmentTTL) {
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

// routeHelperCredentialStaleness explains why the daemon's helper capability
// for this conversation does not authorize the given launch generation:
// "other" when it belongs to a different generation, "ended" when it matches
// the generation but that launch is no longer the live one, and "" when the
// daemon holds no capability for the conversation at all.
func routeHelperCredentialStaleness(agentID, convID, generation string) string {
	agentID = strings.TrimSpace(agentID)
	convID = strings.TrimSpace(convID)
	generation = strings.TrimSpace(generation)
	routeHelperCredentials.RLock()
	defer routeHelperCredentials.RUnlock()
	staleness := ""
	for _, capability := range routeHelperCredentials.items {
		if capability.agentID != agentID || capability.convID != convID {
			continue
		}
		if capability.launchGeneration == generation {
			return "ended"
		}
		staleness = "other"
	}
	return staleness
}

func requireRouteHelperCredential(w http.ResponseWriter, r *http.Request) (routeHelperCredential, bool) {
	capability, present, valid := routeHelperCredentialForRequest(r)
	if !present || !valid {
		writeRouteError(w, http.StatusUnauthorized, "route_helper_auth", "route helper credential is missing, stale, or invalid")
		return routeHelperCredential{}, false
	}
	return capability, true
}
