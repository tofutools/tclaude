package harness

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
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

type openCodeCatalog struct {
	models  []string
	efforts []string
	err     error
	at      time.Time
}

var openCodeModelCache struct {
	sync.Mutex
	value openCodeCatalog
	// refreshing single-flights the background refresh, so a burst of polls
	// arriving on a stale cache starts one subprocess rather than one each.
	// At most one goroutine and therefore at most one `opencode models` process
	// exists at any moment, however long that process takes to answer.
	refreshing bool
	// lastRefreshStart is when a background refresh was last STARTED (not
	// finished), which is what openCodeModelRefreshMinInterval bounds.
	lastRefreshStart time.Time
}

// resolveOpenCodeExecutable is indirected so a test can describe an installed
// OpenCode without owning the machine it runs on.
var resolveOpenCodeExecutable = OpenCodeExecutable

var runOpenCodeModels = func(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, path, "models", "openai", "--verbose").Output()
}

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
	if stale && rateAllows && !openCodeModelCache.refreshing {
		openCodeModelCache.refreshing = true
		openCodeModelCache.lastRefreshStart = time.Now()
		go func() {
			defer func() {
				openCodeModelCache.Lock()
				openCodeModelCache.refreshing = false
				openCodeModelCache.Unlock()
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

// refreshOpenCodeCatalog executes OpenCode and stores the result.
//
// The exec runs OUTSIDE the lock. Holding it across a subprocess — which is
// what this function used to do — meant one slow or wedged `opencode models`
// blocked every other reader for up to its 10-second timeout, including the
// dashboard poll. Two callers racing a stale cache may both exec; that is
// bounded and far cheaper than serializing everyone behind one of them.
func refreshOpenCodeCatalog() openCodeCatalog {
	value := computeOpenCodeCatalog()
	openCodeModelCache.Lock()
	defer openCodeModelCache.Unlock()
	openCodeModelCache.value = value
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
