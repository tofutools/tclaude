package nativeguidance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

const credentialFilename = "credential"

type CallbackEvidence struct {
	RegistrationID   string `json:"registration_id"`
	CredentialDigest string `json:"credential_digest"`
}

// CallbackResource retains a provider-private callback secret on disk. Only
// its digest crosses the provider/ingress boundary or enters durable evidence.
type CallbackResource struct {
	root     string
	evidence CallbackEvidence
	binding  ports.CallbackBinding
}

func PrepareCallback(root string) (*CallbackResource, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("native callback root must be absolute")
	}
	registrationRaw := make([]byte, 16)
	secretRaw := make([]byte, 32)
	if _, err := rand.Read(registrationRaw); err != nil {
		return nil, fmt.Errorf("create native callback registration id: %w", err)
	}
	if _, err := rand.Read(secretRaw); err != nil {
		return nil, fmt.Errorf("create native callback credential: %w", err)
	}
	registrationID := hex.EncodeToString(registrationRaw)
	secret := []byte(base64.RawURLEncoding.EncodeToString(secretRaw))
	digest := sha256.Sum256(secret)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create native callback root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("protect native callback root: %w", err)
	}
	directory := filepath.Join(root, registrationID)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("reserve native callback resource: %w", err)
	}
	path := filepath.Join(directory, credentialFilename)
	if err := os.WriteFile(path, secret, 0o600); err != nil {
		_ = os.RemoveAll(directory)
		return nil, fmt.Errorf("write native callback credential: %w", err)
	}
	return &CallbackResource{root: root, evidence: CallbackEvidence{RegistrationID: registrationID, CredentialDigest: hex.EncodeToString(digest[:])}}, nil
}

func RecoverCallback(root string, evidence CallbackEvidence) (*CallbackResource, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) || len(evidence.RegistrationID) != 32 {
		return nil, fmt.Errorf("native callback evidence is invalid")
	}
	if _, err := hex.DecodeString(evidence.RegistrationID); err != nil {
		return nil, fmt.Errorf("native callback registration id is invalid: %w", err)
	}
	path := filepath.Join(root, evidence.RegistrationID, credentialFilename)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect native callback credential: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("native callback credential is not a protected regular file")
	}
	secret, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read native callback credential: %w", err)
	}
	digest := sha256.Sum256(secret)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), evidence.CredentialDigest) {
		return nil, fmt.Errorf("native callback credential digest does not match evidence")
	}
	return &CallbackResource{root: root, evidence: evidence}, nil
}

func (r *CallbackResource) Evidence() CallbackEvidence { return r.evidence }

// Endpoint is the host-owned registered Unix ingress, for exact confinement
// resource preparation. It is not supplied by an authored sandbox profile.
func (r *CallbackResource) Endpoint() string { return r.binding.Endpoint }
func (r *CallbackResource) CredentialPath() string {
	return filepath.Join(r.root, r.evidence.RegistrationID, credentialFilename)
}

func (r *CallbackResource) Register(ctx context.Context, ingress ports.CallbackIngress, executionID model.ExecutionID, attempt model.AttemptGeneration, handler ports.NativeCallbackHandler) error {
	if ingress == nil || handler == nil || executionID == "" || attempt == 0 {
		return fmt.Errorf("native callback registration is incomplete")
	}
	digestRaw, err := hex.DecodeString(r.evidence.CredentialDigest)
	if err != nil || len(digestRaw) != sha256.Size {
		return fmt.Errorf("native callback credential digest is invalid")
	}
	var digest ports.CallbackCredentialDigest
	copy(digest[:], digestRaw)
	binding, err := ingress.RegisterCallback(ctx, ports.CallbackRegistration{
		RegistrationID: r.evidence.RegistrationID, ExecutionID: executionID, Attempt: attempt,
		CredentialDigest: digest, MaxRequestBytes: MaxPayloadBytes, MaxResponseBytes: MaxGuidanceBytes + 4096, Handler: handler,
	})
	if err != nil {
		return fmt.Errorf("register native callback: %w", err)
	}
	if binding.RegistrationID != r.evidence.RegistrationID || binding.ExecutionID != executionID || binding.Attempt != attempt ||
		binding.Cleanup == nil || binding.Cleanup.RegistrationID() != binding.RegistrationID ||
		binding.Cleanup.ExecutionID() != executionID || binding.Cleanup.Attempt() != attempt ||
		strings.TrimSpace(binding.Endpoint) == "" || strings.TrimSpace(binding.Route) == "" {
		if binding.Cleanup != nil {
			_ = binding.Cleanup.Close(context.Background())
		}
		return fmt.Errorf("native callback ingress returned a mismatched binding")
	}
	r.binding = binding
	return nil
}

// Command returns a fixed callback client command with only provider-generated
// paths and route metadata shell-quoted. The secret is read from its 0600 file.
func (r *CallbackResource) Command(executable string) (string, error) {
	if r.binding.Cleanup == nil {
		return "", fmt.Errorf("native callback is not registered")
	}
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("native callback client executable must be absolute")
	}
	url := "http://localhost" + r.binding.Route
	return "set -eu\ncredential=$(cat " + shellQuote(r.CredentialPath()) + ")\n" +
		"exec " + shellQuote(executable) + " --silent --show-error --fail-with-body --unix-socket " + shellQuote(r.binding.Endpoint) +
		" -H \"Authorization: Native $credential\" -H 'Content-Type: application/json' --data-binary @- " + shellQuote(url), nil
}

func (r *CallbackResource) WriteCommandScript(command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", fmt.Errorf("native callback command is empty")
	}
	path := filepath.Join(r.root, r.evidence.RegistrationID, "callback")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+command+"\n"), 0o700); err != nil {
		return "", fmt.Errorf("write native callback script: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return "", fmt.Errorf("protect native callback script: %w", err)
	}
	return path, nil
}

func (r *CallbackResource) CloseRegistration(ctx context.Context) error {
	if r.binding.Cleanup == nil {
		return nil
	}
	err := r.binding.Cleanup.Close(ctx)
	if err == nil {
		r.binding = ports.CallbackBinding{}
	}
	return err
}

func (r *CallbackResource) Remove(ctx context.Context) error {
	if err := r.CloseRegistration(ctx); err != nil {
		return err
	}
	directory := filepath.Join(r.root, r.evidence.RegistrationID)
	return os.RemoveAll(directory)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
