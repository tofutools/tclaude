// Package client is the thin Unix-socket client for replacement backend callers.
// It has no database or provider dependency and never retries an effect.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const maxResponseBytes = 8 << 20

// Client reads its credential resource for each call, allowing host-owned atomic
// credential rotation without handing renewal authority to the bearer itself.
type Client struct {
	credentialFile string
	http           *http.Client
	transport      *http.Transport
}

func New(socketPath, credentialFile string) (*Client, error) {
	if !filepath.IsAbs(socketPath) || !filepath.IsAbs(credentialFile) {
		return nil, errors.New("absolute socket and credential paths are required")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	}}
	return &Client{credentialFile: credentialFile, transport: transport, http: &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *Client) Close() { c.transport.CloseIdleConnections() }

type Error struct {
	Status     int
	Code       string
	WriteProof *DirectoryWriteProof
}

func (e *Error) Error() string {
	return fmt.Sprintf("backend request failed: %s (HTTP %d)", e.Code, e.Status)
}

// Call performs exactly one request. The caller owns request identity and retry
// decisions, including after an uncertain outcome or a transport interruption.
func (c *Client) Call(ctx context.Context, method, path string, body, result any) error {
	if !strings.HasPrefix(path, "/v2/") || strings.ContainsAny(path, "#\r\n") {
		return errors.New("a relative backend API path without a fragment is required")
	}
	if _, err := url.ParseRequestURI(path); err != nil {
		return errors.New("invalid backend API request path")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	credential, err := readCredential(c.credentialFile)
	if err != nil {
		return err
	}
	var data []byte
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode backend request: %w", err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://backend"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(content) > maxResponseBytes {
		return errors.New("backend response exceeds client size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Code       string               `json:"code"`
			WriteProof *DirectoryWriteProof `json:"write_proof"`
		}
		_ = json.Unmarshal(content, &failure)
		if failure.Code == "" {
			failure.Code = "invalid_response"
		}
		return &Error{Status: response.StatusCode, Code: failure.Code, WriteProof: failure.WriteProof}
	}
	if result == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(content, result); err != nil {
		return fmt.Errorf("decode backend response: %w", err)
	}
	return nil
}

func readCredential(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read backend credential resource: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", err
	}
	credential := string(data)
	if len(data) < 32 || len(data) > 4096 || strings.ContainsAny(credential, " \t\r\n") {
		return "", errors.New("invalid backend credential resource")
	}
	return credential, nil
}

// DirectoryWriteProof is a pre-admission challenge, not permission to retry an uncertain effect.
type DirectoryWriteProof struct {
	Token       string   `json:"token"`
	Filename    string   `json:"filename"`
	Directories []string `json:"dirs"`
}
