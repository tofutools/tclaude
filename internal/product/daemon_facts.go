package product

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/ports"
	githubsource "github.com/tofutools/tclaude/internal/backend/sources/github"
	"golang.org/x/sys/unix"
)

// Sources are operator-selected composition, never rule-authored URLs or secrets.
func configuredGitHubSources(specs []string, tokenFile string) ([]ports.AutomationFactSource, error) {
	if len(specs) > 4 {
		return nil, errors.New("at most four GitHub sources may be configured")
	}
	if len(specs) == 0 && tokenFile != "" {
		return nil, errors.New("GitHub credential requires a configured source")
	}
	var token string
	if tokenFile != "" {
		if !filepath.IsAbs(tokenFile) {
			return nil, errors.New("GitHub credential file must be absolute")
		}
		f, err := os.OpenFile(tokenFile, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, errors.New("cannot open GitHub credential file")
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("GitHub credential file must be a private regular file")
		}
		b, err := io.ReadAll(io.LimitReader(f, 8193))
		if err != nil || len(b) > 8192 {
			return nil, errors.New("cannot read bounded GitHub credential")
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			return nil, errors.New("GitHub credential file is empty")
		}
	}
	sources := make([]ports.AutomationFactSource, 0, len(specs))
	seen := map[string]bool{}
	for _, spec := range specs {
		name, target, ok := strings.Cut(spec, "=")
		repo, number, found := strings.Cut(target, "#")
		n, err := strconv.ParseUint(number, 10, 64)
		if !ok || !found || err != nil || seen[name] {
			return nil, errors.New("GitHub source must have a unique name and exact name=owner/repository#number")
		}
		source, err := githubsource.New(githubsource.Config{Name: name, Repository: repo, PullRequest: n, Token: token})
		if err != nil {
			return nil, err
		}
		seen[name] = true
		sources = append(sources, source)
	}
	return sources, nil
}
