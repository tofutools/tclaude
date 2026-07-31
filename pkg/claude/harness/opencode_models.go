package harness

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

const openCodeModelCacheTTL = 5 * time.Minute

// openCodeModelRefreshMinInterval floors how often a BACKGROUND refresh may be
// STARTED, independent of whether the last one succeeded. The single-flight
// flag already guarantees at most one subprocess in flight; this guarantees the
// rate as well, so a permanently failing or permanently slow `opencode models`
// cannot turn into back-to-back forks (each one starting the moment the last
// gave up). Belt and braces: the 5-minute TTL is the binding constraint on the
// happy path, and this floor only matters when refreshes keep failing.
const openCodeModelRefreshMinInterval = time.Minute

// openCodeModelsExecTimeout bounds the subprocess itself.
const openCodeModelsExecTimeout = 10 * time.Second

type openCodeCatalog struct {
	models  []string
	efforts []string
	err     error
	at      time.Time
}

var openCodeModelCache struct {
	sync.Mutex
	value openCodeCatalog
	// lastRefreshStart is when a background refresh was last STARTED (not
	// finished), which is what openCodeModelRefreshMinInterval bounds.
	//
	// Keyed on the START, not the finish. An "is one running?" flag would be
	// the obvious second guard and is deliberately absent: it is cleared only
	// by the refreshing goroutine, so anything that parked that goroutine would
	// freeze the catalog for the daemon's whole life — silently, since no
	// reader blocks any more. A start-keyed floor cannot be left set.
	//
	// That removes one wedge but not the underlying one: a refresh that never
	// returns still holds openCodeCatalogFlight, and later refreshes join it
	// rather than forking a second subprocess. Liveness therefore rests on
	// runOpenCodeModels ALWAYS returning — which is why its exec carries both a
	// context deadline and a WaitDelay. Safety (one subprocess) comes from the
	// flight group; the floor bounds the rate.
	lastRefreshStart time.Time
}

// openCodeRefreshesInFlight counts background refreshes that have started and
// not yet returned. Observability, never a gate — see lastRefreshStart.
var openCodeRefreshesInFlight atomic.Int64

// resolveOpenCodeExecutable is indirected so a test can describe an installed
// OpenCode without owning the machine it runs on.
var resolveOpenCodeExecutable = OpenCodeExecutable

var runOpenCodeModels = func(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeModelsExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "models", "openai", "--verbose")
	// WaitDelay is load-bearing, not a nicety. Output() reads until the stdout
	// pipe hits EOF, and cancelling the context only kills `opencode` itself —
	// an inherited grandchild holding that pipe open keeps Output() parked long
	// after the deadline. OpenCode is a JS runtime that spawns children, so this
	// is reachable, and the caller is a background refresh whose completion is
	// what re-arms future refreshes.
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// openCodeCatalogFlight collapses concurrent refreshes — background and
// synchronous alike — into ONE subprocess. The disclosure flag below only
// bounds background goroutines; this is what makes "at most one
// `opencode models` running at any moment" true globally, including a burst of
// concurrent spawns each validating a model against a cold cache.
var openCodeCatalogFlight singleflight.Group

type openCodeModels struct{}

func (openCodeModels) ValidateModel(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	catalog := loadOpenCodeCatalog()
	if catalog.err != nil {
		return "", fmt.Errorf("read OpenCode model catalog: %w", catalog.err)
	}
	if !slices.Contains(catalog.models, value) {
		return "", fmt.Errorf("unknown OpenCode OpenAI model %q (available: %v)", value, catalog.models)
	}
	return value, nil
}

func (openCodeModels) ValidateEffort(value string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "", nil
	}
	catalog := loadOpenCodeCatalog()
	if catalog.err != nil {
		return "", fmt.Errorf("read OpenCode effort catalog: %w", catalog.err)
	}
	if !slices.Contains(catalog.efforts, value) {
		return "", fmt.Errorf("unknown OpenCode variant %q (available: %v)", value, catalog.efforts)
	}
	return value, nil
}

// Models and EffortLevels are DISCLOSURE: their only caller is the dashboard's
// harness catalog, rebuilt on every 2-second /api/snapshot poll. They never
// execute OpenCode — they serve the last known catalog and let a stale one be
// refreshed in the background. A poll that had to wait for `opencode models`
// was measured at 516 ms (the exec is allowed 10 s), which is what a 5-minute
// TTL on a 2-second poll turns into: one poll in 150 stalling on a subprocess.
//
// The cost of that: for the first poll or two of a daemon's life the OpenCode
// model list is empty, and after an `opencode` upgrade it stays stale for up to
// one TTL. Neither can mis-launch anything — ValidateModel/ValidateEffort below
// REFUSE on this catalog and therefore still load it synchronously.
func (openCodeModels) Models() []string {
	return slices.Clone(openCodeCatalogForDisclosure().models)
}

func (openCodeModels) EffortLevels() []string {
	return slices.Clone(openCodeCatalogForDisclosure().efforts)
}

// openCodeCatalogForDisclosure returns the last known catalog without ever
// executing OpenCode, kicking off a single background refresh when what it has
// is stale or absent. Returning a zero catalog is deliberate and honest: the
// daemon genuinely does not know the model list yet, and a disclosure surface
// showing an empty list briefly is better than a polled request forking a
// subprocess.
//
// Refresh is triggered by a READ rather than a timer so an idle daemon — no
// dashboard open — never execs at all.
func openCodeCatalogForDisclosure() openCodeCatalog {
	openCodeModelCache.Lock()
	defer openCodeModelCache.Unlock()
	value := openCodeModelCache.value
	stale := value.at.IsZero() ||
		time.Since(value.at) >= openCodeModelCacheTTL
	rateAllows := openCodeModelCache.lastRefreshStart.IsZero() ||
		time.Since(openCodeModelCache.lastRefreshStart) >= openCodeModelRefreshMinInterval
	if stale && rateAllows {
		openCodeModelCache.lastRefreshStart = time.Now()
		openCodeRefreshesInFlight.Add(1)
		go func() {
			defer func() {
				openCodeRefreshesInFlight.Add(-1)
				// This goroutine no longer runs inside an HTTP handler, so a
				// panic here would take the daemon down rather than produce a
				// 500. A stale model list is not worth that.
				if r := recover(); r != nil {
					slog.Error("opencode model catalog refresh panicked",
						"panic", r, "module", "harness")
				}
			}()
			refreshOpenCodeCatalog()
		}()
	}
	return value
}

// loadOpenCodeCatalog is the synchronous, authoritative read behind
// ValidateModel/ValidateEffort. Those REFUSE a launch on this answer, so they
// must not run on a stale or absent catalog the way the disclosure above may.
func loadOpenCodeCatalog() openCodeCatalog {
	openCodeModelCache.Lock()
	if !openCodeModelCache.value.at.IsZero() &&
		time.Since(openCodeModelCache.value.at) < openCodeModelCacheTTL {
		value := openCodeModelCache.value
		openCodeModelCache.Unlock()
		return value
	}
	openCodeModelCache.Unlock()
	return refreshOpenCodeCatalog()
}

// refreshOpenCodeCatalog executes OpenCode and stores the result, joining any
// refresh already running instead of starting a second one.
//
// The exec runs OUTSIDE the cache lock. Holding it across a subprocess — which
// is what this function used to do — meant one slow or wedged `opencode models`
// blocked every other reader for up to its timeout, including the dashboard
// poll. The flight group replaces that accidental serialization with a
// deliberate one: concurrent callers still produce a single subprocess, but
// they wait on its RESULT rather than on a mutex, so the disclosure path can
// decline to wait at all.
func refreshOpenCodeCatalog() openCodeCatalog {
	shared, _, _ := openCodeCatalogFlight.Do("catalog", func() (any, error) {
		value := computeOpenCodeCatalog()
		openCodeModelCache.Lock()
		defer openCodeModelCache.Unlock()
		openCodeModelCache.value = value
		return value, nil
	})
	value, _ := shared.(openCodeCatalog)
	return value
}

func computeOpenCodeCatalog() openCodeCatalog {
	path, err := resolveOpenCodeExecutable()
	if err != nil {
		return openCodeCatalog{err: err, at: time.Now()}
	}
	output, err := runOpenCodeModels(path)
	if err != nil {
		return openCodeCatalog{err: err, at: time.Now()}
	}
	models, efforts := parseOpenCodeModelsVerbose(string(output))
	if len(models) == 0 {
		err = fmt.Errorf("`opencode models openai --verbose` returned no models")
	}
	return openCodeCatalog{
		models: models, efforts: efforts, err: err, at: time.Now(),
	}
}

// parseOpenCodeModelsVerbose parses OpenCode 1.18's stream of alternating
// `provider/model` lines and pretty-printed JSON metadata. Variant names are
// discovered from each metadata object's `variants` map; no model or effort
// list is hard-coded in tclaude.
func parseOpenCodeModelsVerbose(output string) ([]string, []string) {
	var models, efforts []string
	effortSeen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	inVariants := false
	variantDepth := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "openai/") && !strings.ContainsAny(line, " \t{}") {
			models = append(models, line)
			continue
		}
		if line == `"variants": {` {
			inVariants = true
			variantDepth = 1
			continue
		}
		if !inVariants {
			continue
		}
		if variantDepth == 1 && strings.HasPrefix(line, `"`) {
			if end := strings.Index(line[1:], `"`); end >= 0 &&
				strings.HasSuffix(strings.TrimSpace(line[end+2:]), "{") {
				name := line[1 : end+1]
				if !effortSeen[name] {
					effortSeen[name] = true
					efforts = append(efforts, name)
				}
			}
		}
		variantDepth += strings.Count(line, "{") - strings.Count(line, "}")
		if variantDepth <= 0 {
			inVariants = false
		}
	}
	return models, efforts
}
