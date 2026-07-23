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

type openCodeCatalog struct {
	models  []string
	efforts []string
	err     error
	at      time.Time
}

var openCodeModelCache struct {
	sync.Mutex
	value openCodeCatalog
}

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

func (openCodeModels) Models() []string {
	return slices.Clone(loadOpenCodeCatalog().models)
}

func (openCodeModels) EffortLevels() []string {
	return slices.Clone(loadOpenCodeCatalog().efforts)
}

func loadOpenCodeCatalog() openCodeCatalog {
	openCodeModelCache.Lock()
	defer openCodeModelCache.Unlock()
	if !openCodeModelCache.value.at.IsZero() &&
		time.Since(openCodeModelCache.value.at) < openCodeModelCacheTTL {
		return openCodeModelCache.value
	}
	path, err := OpenCodeExecutable()
	if err != nil {
		openCodeModelCache.value = openCodeCatalog{err: err, at: time.Now()}
		return openCodeModelCache.value
	}
	output, err := runOpenCodeModels(path)
	if err != nil {
		openCodeModelCache.value = openCodeCatalog{err: err, at: time.Now()}
		return openCodeModelCache.value
	}
	models, efforts := parseOpenCodeModelsVerbose(string(output))
	if len(models) == 0 {
		err = fmt.Errorf("`opencode models openai --verbose` returned no models")
	}
	openCodeModelCache.value = openCodeCatalog{
		models: models, efforts: efforts, err: err, at: time.Now(),
	}
	return openCodeModelCache.value
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
