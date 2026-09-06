package host

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const actionCredentialFilename = "credential"

// ActionCredentialHost owns renewable bearer delivery resources. The bearer is
// written only to a protected file; callers pass Path to the workload as
// metadata so the workload can reread it for every request.
type ActionCredentialHost struct {
	PrivateRoot string
}

// ActionCredentialResource is an exact host-owned delivery resource. Its path
// is safe to retain in provider-private recovery evidence; its contents are
// not.
type ActionCredentialResource struct {
	root string
	path string
	mu   sync.Mutex
}

func (h ActionCredentialHost) Prepare(bearer []byte) (*ActionCredentialResource, error) {
	if err := validateBearer(bearer); err != nil {
		return nil, err
	}
	root := filepath.Clean(h.PrivateRoot)
	if root == "." || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("action credential private root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create action credential private root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("protect action credential private root: %w", err)
	}
	nonce, err := credentialNonce()
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(root, "action-credential-"+nonce)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("reserve action credential resource: %w", err)
	}
	resource := &ActionCredentialResource{root: root, path: filepath.Join(directory, actionCredentialFilename)}
	if err := resource.replaceLocked(bearer); err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	return resource, nil
}

// Recover restores control only for the exact regular credential file below
// this host's private root. It never reads bearer plaintext.
func (h ActionCredentialHost) Recover(path string) (*ActionCredentialResource, error) {
	root := filepath.Clean(h.PrivateRoot)
	path = filepath.Clean(path)
	if root == "." || !filepath.IsAbs(root) || !validCredentialPath(root, path) {
		return nil, fmt.Errorf("action credential resource is outside private storage")
	}
	resource := &ActionCredentialResource{root: root, path: path}
	if err := resource.verifyLocked(); err != nil {
		return nil, err
	}
	return resource, nil
}

func (r *ActionCredentialResource) Path() string { return r.path }

// Replace atomically publishes renewed material at the stable resource path.
// A client reading concurrently observes either the complete predecessor or
// the complete replacement, never a partially-written bearer.
func (r *ActionCredentialResource) Replace(bearer []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validateBearer(bearer); err != nil {
		return err
	}
	if err := r.verifyDirectoryLocked(); err != nil {
		return err
	}
	return r.replaceLocked(bearer)
}

func (r *ActionCredentialResource) Verify() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.verifyLocked()
}

func (r *ActionCredentialResource) Remove() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !validCredentialPath(r.root, r.path) {
		return fmt.Errorf("action credential resource is outside private storage")
	}
	directory := filepath.Dir(r.path)
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove action credential resource: %w", err)
	}
	return nil
}

func (r *ActionCredentialResource) replaceLocked(bearer []byte) error {
	directory := filepath.Dir(r.path)
	temporary, err := os.CreateTemp(directory, ".credential-*")
	if err != nil {
		return fmt.Errorf("create action credential replacement: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect action credential replacement: %w", err)
	}
	if _, err := temporary.Write(bearer); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write action credential replacement: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync action credential replacement: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close action credential replacement: %w", err)
	}
	if err := os.Rename(temporaryPath, r.path); err != nil {
		return fmt.Errorf("publish action credential replacement: %w", err)
	}
	return nil
}

func (r *ActionCredentialResource) verifyLocked() error {
	if err := r.verifyDirectoryLocked(); err != nil {
		return err
	}
	info, err := os.Lstat(r.path)
	if err != nil {
		return fmt.Errorf("inspect action credential resource: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("action credential resource is not a protected regular file")
	}
	return nil
}

func (r *ActionCredentialResource) verifyDirectoryLocked() error {
	if !validCredentialPath(r.root, r.path) {
		return fmt.Errorf("action credential resource is outside private storage")
	}
	info, err := os.Lstat(filepath.Dir(r.path))
	if err != nil {
		return fmt.Errorf("inspect action credential directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("action credential directory is not protected")
	}
	return nil
}

func validCredentialPath(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(relative, string(filepath.Separator))
	return len(parts) == 2 && strings.HasPrefix(parts[0], "action-credential-") &&
		parts[0] != "action-credential-" && parts[1] == actionCredentialFilename
}

func validateBearer(bearer []byte) error {
	if len(bearer) == 0 {
		return fmt.Errorf("action credential bearer is required")
	}
	if len(bearer) > 64<<10 {
		return fmt.Errorf("action credential bearer is too large")
	}
	return nil
}

func credentialNonce() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate action credential resource identity: %w", err)
	}
	return hex.EncodeToString(value), nil
}
