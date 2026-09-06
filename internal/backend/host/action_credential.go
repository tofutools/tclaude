package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const actionCredentialFilename = "credential"
const actionCredentialBindingFilename = "binding.json"

type actionCredentialBinding struct {
	ExecutionID model.ExecutionID      `json:"execution_id"`
	Generation  model.AccessGeneration `json:"generation"`
	DeliveryID  string                 `json:"delivery_id"`
}

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

func (h ActionCredentialHost) Prepare(deliveryID string, bearer []byte) (*ActionCredentialResource, error) {
	if strings.TrimSpace(deliveryID) == "" {
		return nil, fmt.Errorf("action credential delivery id is required")
	}
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
	directory := credentialDirectory(root, deliveryID)
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

func (h ActionCredentialHost) PrepareActionCredential(_ context.Context, material ports.ActionCredentialMaterial) (ports.ActionCredentialReceipt, error) {
	if err := validateCredentialMaterial(material); err != nil {
		return ports.ActionCredentialReceipt{}, err
	}
	resource, err := h.Prepare(material.DeliveryID, material.Secret)
	if err != nil {
		return ports.ActionCredentialReceipt{}, err
	}
	if err := resource.writeBinding(actionCredentialBinding{
		ExecutionID: material.ExecutionID, Generation: material.Generation, DeliveryID: material.DeliveryID,
	}); err != nil {
		_ = resource.Remove()
		return ports.ActionCredentialReceipt{}, err
	}
	receipt, err := resource.receipt(material.ExecutionID, material.Generation, material.DeliveryID, time.Now().UTC())
	if err != nil {
		_ = resource.Remove()
		return ports.ActionCredentialReceipt{}, err
	}
	return receipt, nil
}

func (h ActionCredentialHost) RotateActionCredential(_ context.Context, current ports.ActionCredentialReceipt, material ports.ActionCredentialMaterial) (ports.ActionCredentialReceipt, error) {
	if err := validateCredentialMaterial(material); err != nil {
		return ports.ActionCredentialReceipt{}, err
	}
	if current.ExecutionID != material.ExecutionID || current.DeliveryID != material.DeliveryID ||
		material.Generation <= current.Generation {
		return ports.ActionCredentialReceipt{}, fmt.Errorf("action credential rotation does not match current delivery")
	}
	resource, err := h.Recover(current.Resource)
	if err != nil {
		return ports.ActionCredentialReceipt{}, err
	}
	identity, err := resource.fileIdentity()
	if err != nil {
		return ports.ActionCredentialReceipt{}, err
	}
	if identity != current.FileIdentity {
		return ports.ActionCredentialReceipt{}, fmt.Errorf("action credential rotation has stale file identity")
	}
	if err := resource.Replace(material.Secret); err != nil {
		return ports.ActionCredentialReceipt{}, err
	}
	if err := resource.writeBinding(actionCredentialBinding{
		ExecutionID: material.ExecutionID, Generation: material.Generation, DeliveryID: material.DeliveryID,
	}); err != nil {
		return ports.ActionCredentialReceipt{}, err
	}
	return resource.receipt(material.ExecutionID, material.Generation, material.DeliveryID, time.Now().UTC())
}

func (h ActionCredentialHost) InspectActionCredential(_ context.Context, binding model.ExecutionAccessBinding) (ports.ActionCredentialRecoveryProof, error) {
	if binding.ExecutionID == "" || binding.Generation == 0 || strings.TrimSpace(binding.DeliveryID) == "" {
		return ports.ActionCredentialRecoveryProof{}, fmt.Errorf("action credential recovery binding is incomplete")
	}
	path := filepath.Join(credentialDirectory(filepath.Clean(h.PrivateRoot), binding.DeliveryID), actionCredentialFilename)
	resource, err := h.Recover(path)
	if err != nil {
		return ports.ActionCredentialRecoveryProof{}, err
	}
	stored, err := resource.readBinding()
	if err != nil {
		return ports.ActionCredentialRecoveryProof{}, err
	}
	if stored.ExecutionID != binding.ExecutionID || stored.Generation != binding.Generation || stored.DeliveryID != binding.DeliveryID {
		return ports.ActionCredentialRecoveryProof{}, fmt.Errorf("action credential resource does not match recovery binding")
	}
	identity, err := resource.fileIdentity()
	if err != nil {
		return ports.ActionCredentialRecoveryProof{}, err
	}
	return ports.ActionCredentialRecoveryProof{
		ExecutionID: binding.ExecutionID, Generation: binding.Generation, DeliveryID: binding.DeliveryID,
		Resource: resource.Path(), FileIdentity: identity, InspectedAt: time.Now().UTC(),
	}, nil
}

func (h ActionCredentialHost) RemoveActionCredential(_ context.Context, receipt ports.ActionCredentialReceipt) error {
	if receipt.Resource == "" || receipt.DeliveryID == "" {
		return fmt.Errorf("action credential receipt is incomplete")
	}
	expected := filepath.Join(credentialDirectory(filepath.Clean(h.PrivateRoot), receipt.DeliveryID), actionCredentialFilename)
	if filepath.Clean(receipt.Resource) != expected {
		return fmt.Errorf("action credential receipt does not match delivery id")
	}
	resource, err := h.Recover(receipt.Resource)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return resource.Remove()
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

func (r *ActionCredentialResource) receipt(executionID model.ExecutionID, generation model.AccessGeneration, deliveryID string, at time.Time) (ports.ActionCredentialReceipt, error) {
	identity, err := r.fileIdentity()
	if err != nil {
		return ports.ActionCredentialReceipt{}, err
	}
	return ports.ActionCredentialReceipt{
		ExecutionID: executionID, Generation: generation, DeliveryID: deliveryID,
		Resource: r.path, FileIdentity: identity, DeliveredAt: at,
	}, nil
}

func (r *ActionCredentialResource) fileIdentity() (string, error) {
	info, err := os.Lstat(r.path)
	if err != nil {
		return "", fmt.Errorf("inspect action credential file identity: %w", err)
	}
	return fileIdentity(info)
}

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

func (r *ActionCredentialResource) writeBinding(binding actionCredentialBinding) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.verifyDirectoryLocked(); err != nil {
		return err
	}
	value, err := json.Marshal(binding)
	if err != nil {
		return fmt.Errorf("encode action credential binding: %w", err)
	}
	return replaceProtectedFile(filepath.Join(filepath.Dir(r.path), actionCredentialBindingFilename), value)
}

func (r *ActionCredentialResource) readBinding() (actionCredentialBinding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	path := filepath.Join(filepath.Dir(r.path), actionCredentialBindingFilename)
	info, err := os.Lstat(path)
	if err != nil {
		return actionCredentialBinding{}, fmt.Errorf("inspect action credential binding: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 64<<10 {
		return actionCredentialBinding{}, fmt.Errorf("action credential binding is not a protected bounded regular file")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return actionCredentialBinding{}, fmt.Errorf("read action credential binding: %w", err)
	}
	var binding actionCredentialBinding
	if err := json.Unmarshal(value, &binding); err != nil {
		return actionCredentialBinding{}, fmt.Errorf("decode action credential binding: %w", err)
	}
	return binding, nil
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
	return replaceProtectedFile(r.path, bearer)
}

func replaceProtectedFile(path string, value []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".replacement-*")
	if err != nil {
		return fmt.Errorf("create action credential replacement: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect action credential replacement: %w", err)
	}
	if _, err := temporary.Write(value); err != nil {
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
	if err := os.Rename(temporaryPath, path); err != nil {
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

func validateCredentialMaterial(material ports.ActionCredentialMaterial) error {
	if material.ExecutionID == "" || material.Generation == 0 || strings.TrimSpace(material.DeliveryID) == "" ||
		material.ExpiresAt.IsZero() {
		return fmt.Errorf("action credential material is incomplete")
	}
	return validateBearer(material.Secret)
}

func credentialDirectory(root, deliveryID string) string {
	digest := sha256.Sum256([]byte(deliveryID))
	return filepath.Join(root, "action-credential-"+hex.EncodeToString(digest[:16]))
}

var _ ports.ActionCredentialDelivery = ActionCredentialHost{}
