package selfupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const apiRoot = "https://api.github.com/repos/tofutools/tclaude"
const downloadRoot = "https://github.com/tofutools/tclaude/releases/download/"
const archiveLimit int64 = 256 << 20
const binaryLimit int64 = 512 << 20

type Release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
	} `json:"assets"`
}

func releaseClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return fmt.Errorf("too many release redirects")
		}
		if r.URL.Scheme != "https" {
			return fmt.Errorf("release download requires HTTPS")
		}
		switch r.URL.Hostname() {
		case "github.com", "api.github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
			return nil
		}
		return fmt.Errorf("release redirected outside official GitHub hosts")
	}}
}
func fetch(ctx context.Context, client *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tclaude-self-update")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("official release request returned HTTP %d", resp.StatusCode)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("release response exceeds size limit")
	}
	return out, nil
}
func loadRelease(ctx context.Context, version string) (Release, error) {
	tail := "/releases/latest"
	if version != "" {
		tail = "/releases/tags/" + url.PathEscape(version)
	}
	raw, err := fetch(ctx, releaseClient(), apiRoot+tail, 2<<20)
	if err != nil {
		return Release{}, err
	}
	var r Release
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if !semver.IsValid(r.Tag) || semver.Canonical(r.Tag) != r.Tag || r.Draft || version == "" && r.Prerelease {
		return r, fmt.Errorf("official release has invalid or unstable version")
	}
	if version != "" && r.Tag != version {
		return r, fmt.Errorf("release version does not match requested pin")
	}
	return r, nil
}
func archiveName(name string) (string, error) {
	platform := ""
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		platform = "linux_amd64_v1"
	case "linux/arm64":
		platform = "linux_arm64_v8.0"
	case "darwin/arm64":
		platform = "darwin_arm64_v8.0"
	default:
		return "", fmt.Errorf("no official release artifact for this platform")
	}
	id := name
	if name != "tclaude-hub" {
		if runtime.GOOS == "linux" {
			id += "-no-cgo"
		} else {
			id += "-darwin"
		}
	}
	return id + "_" + platform + ".tar.gz", nil
}
func checksumFor(raw []byte, name string) ([]byte, error) {
	var found []byte
	s := bufio.NewScanner(bytes.NewReader(raw))
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			if found != nil {
				return nil, fmt.Errorf("duplicate release checksum")
			}
			var err error
			found, err = hex.DecodeString(fields[0])
			if err != nil || len(found) != sha256.Size {
				return nil, fmt.Errorf("invalid release checksum")
			}
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("release checksum missing for %s", name)
	}
	return found, nil
}
func extractBinary(raw []byte, name, dest string) error {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(io.LimitReader(gz, binaryLimit+(16<<20)))
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(h.Name)
		if filepath.IsAbs(h.Name) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("unsafe release archive path")
		}
		if filepath.Base(clean) != name {
			continue
		}
		if found || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > binaryLimit {
			return fmt.Errorf("invalid binary entry in release archive")
		}
		found = true
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyN(f, tr, h.Size)
		syncErr := f.Sync()
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if !found {
		return fmt.Errorf("release binary missing")
	}
	return nil
}
func stageRelease(ctx context.Context, r Release, name, dir string) (string, error) {
	artifact, err := archiveName(name)
	if err != nil {
		return "", err
	}
	exists := false
	for _, a := range r.Assets {
		exists = exists || a.Name == artifact
	}
	if !exists {
		return "", fmt.Errorf("official release artifact missing: %s", artifact)
	}
	root := downloadRoot + url.PathEscape(r.Tag) + "/"
	checksums, err := fetch(ctx, releaseClient(), root+"checksums.txt", 1<<20)
	if err != nil {
		return "", err
	}
	sum, err := checksumFor(checksums, artifact)
	if err != nil {
		return "", err
	}
	raw, err := fetch(ctx, releaseClient(), root+artifact, archiveLimit)
	if err != nil {
		return "", err
	}
	actual := sha256.Sum256(raw)
	if !bytes.Equal(sum, actual[:]) {
		return "", fmt.Errorf("official release checksum mismatch")
	}
	path := filepath.Join(dir, name)
	if err := extractBinary(raw, name, path); err != nil {
		return "", err
	}
	return path, nil
}

type limitedBuffer struct {
	data  []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > b.limit {
		return 0, fmt.Errorf("subprocess output exceeds limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func stageGoInstall(ctx context.Context, name, version, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "install", binaryPackages[name]+"@"+version)
	cmd.Env = append(filteredGoEnv(os.Environ()), "GOBIN="+dir, "GOWORK=off")
	cmd.WaitDelay = time.Second
	// Do not log inherited environment or compiler output, which can contain
	// private paths and credential-bearing proxy URLs.
	var output limitedBuffer
	output.limit = 64 << 10
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go install failed: %w", err)
	}
	return filepath.Join(dir, name), nil
}
func filteredGoEnv(env []string) []string {
	out := []string{}
	for _, e := range env {
		if !strings.HasPrefix(e, "GOBIN=") && !strings.HasPrefix(e, "GOWORK=") {
			out = append(out, e)
		}
	}
	return out
}
