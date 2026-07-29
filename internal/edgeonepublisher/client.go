package edgeonepublisher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrNoWork = errors.New("no distribution work")

type Lease struct {
	ID             string `json:"id"`
	AccelerationID string `json:"acceleration_id"`
	TrackRef       string `json:"track_ref"`
	Owner          string `json:"owner"`
	ExpiresAt      int64  `json:"expires_at"`
	CreatedAt      int64  `json:"created_at"`
	SourceURL      string `json:"-"`
}

type Candidate struct {
	ContentVersion string `json:"content_version"`
	Locator        string `json:"locator"`
	Layout         string `json:"layout"`
	SizeBytes      int64  `json:"size_bytes"`
	ContentType    string `json:"content_type"`
	ETag           string `json:"etag,omitempty"`
}

type PublisherHeartbeat struct {
	Owner          string   `json:"owner"`
	Version        string   `json:"version"`
	State          string   `json:"state"`
	LeaseID        string   `json:"lease_id"`
	TrackRef       string   `json:"track_ref"`
	Capabilities   []string `json:"capabilities"`
	BackendHealthy bool     `json:"backend_healthy"`
	LastError      string   `json:"last_error"`
}

type CoreClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewCoreClient(baseURL, token string, client *http.Client) *CoreClient {
	return &CoreClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: client}
}

func (c *CoreClient) ManagedConfig(ctx context.Context) (ManagedConfig, error) {
	var config ManagedConfig
	status, err := c.json(ctx, http.MethodGet, "/internal/v1/distribution/publisher/config", nil, &config)
	if err != nil {
		return ManagedConfig{}, err
	}
	if status != http.StatusOK {
		return ManagedConfig{}, fmt.Errorf("publisher config: status %d", status)
	}
	if err := config.Validate(); err != nil {
		return ManagedConfig{}, err
	}
	return config, nil
}

func (c *CoreClient) Heartbeat(ctx context.Context, heartbeat PublisherHeartbeat) error {
	status, err := c.json(ctx, http.MethodPost,
		"/internal/v1/distribution/publishers/heartbeat", heartbeat, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("publisher heartbeat: status %d", status)
	}
	return nil
}

func (c *CoreClient) Claim(ctx context.Context, owner string, leaseSeconds int) (Lease, error) {
	var response struct {
		Lease     Lease  `json:"lease"`
		SourceURL string `json:"source_url"`
	}
	status, err := c.json(ctx, http.MethodPost, "/internal/v1/distribution/leases", map[string]any{
		"owner": owner, "lease_seconds": leaseSeconds,
	}, &response)
	if err != nil {
		return Lease{}, err
	}
	if status == http.StatusNoContent {
		return Lease{}, ErrNoWork
	}
	if status != http.StatusCreated {
		return Lease{}, fmt.Errorf("claim lease: status %d", status)
	}
	response.Lease.SourceURL = response.SourceURL
	return response.Lease, nil
}

func (c *CoreClient) Source(ctx context.Context, lease Lease) (*http.Response, error) {
	target, err := resolveURL(c.baseURL, lease.SourceURL)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	return c.client.Do(request)
}

func (c *CoreClient) Progress(
	ctx context.Context,
	lease Lease,
	phase string,
	sourceBytes, uploadBytes, totalBytes int64,
) error {
	status, err := c.json(ctx, http.MethodPatch,
		"/internal/v1/distribution/leases/"+url.PathEscape(lease.ID)+"/progress",
		map[string]any{
			"owner": lease.Owner, "phase": phase, "source_bytes": sourceBytes,
			"upload_bytes": uploadBytes, "total_bytes": totalBytes,
		}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("update progress: status %d", status)
	}
	return nil
}

func (c *CoreClient) Complete(ctx context.Context, lease Lease, candidate Candidate) error {
	body := map[string]any{
		"owner": lease.Owner, "content_version": candidate.ContentVersion,
		"locator": candidate.Locator, "layout": candidate.Layout,
		"size_bytes": candidate.SizeBytes, "content_type": candidate.ContentType,
		"etag": candidate.ETag,
	}
	status, err := c.json(ctx, http.MethodPost,
		"/internal/v1/distribution/leases/"+url.PathEscape(lease.ID)+"/complete", body, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("complete lease: status %d", status)
	}
	return nil
}

func (c *CoreClient) Fail(ctx context.Context, lease Lease, publishErr error, retryAfter time.Duration) error {
	message := publishErr.Error()
	if len(message) > 2000 {
		message = message[:2000]
	}
	status, err := c.json(ctx, http.MethodPost,
		"/internal/v1/distribution/leases/"+url.PathEscape(lease.ID)+"/fail",
		map[string]any{
			"owner": lease.Owner, "error": message,
			"retry_after_seconds": int(retryAfter / time.Second),
		}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("fail lease: status %d", status)
	}
	return nil
}

func (c *CoreClient) json(ctx context.Context, method, path string, body, result any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		return response.StatusCode, fmt.Errorf("core %s: status %d: %s", path,
			response.StatusCode, strings.TrimSpace(string(detail)))
	}
	if result != nil && response.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result); err != nil {
			return response.StatusCode, err
		}
	}
	return response.StatusCode, nil
}

type SignerClient struct {
	baseURL string
	token   string
	client  *http.Client
}

type SignedPUT struct {
	Locator   string `json:"locator"`
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at"`
}

type BlobMetadata struct {
	Locator      string `json:"locator"`
	Exists       bool   `json:"exists"`
	SizeBytes    int64  `json:"size_bytes"`
	ContentType  string `json:"content_type"`
	ETag         string `json:"etag"`
	CacheControl string `json:"cache_control"`
}

func NewSignerClient(baseURL, token string, client *http.Client) *SignerClient {
	return &SignerClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: client}
}

func (c *SignerClient) Health(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("signer health: status %d", response.StatusCode)
	}
	return nil
}

func (c *SignerClient) SignPUT(ctx context.Context, locator, contentType string) (SignedPUT, error) {
	var response struct {
		Objects []SignedPUT `json:"objects"`
	}
	if err := c.post(ctx, "/put-urls", map[string]any{
		"objects": []map[string]string{{"locator": locator, "content_type": contentType}},
	}, &response); err != nil {
		return SignedPUT{}, err
	}
	if len(response.Objects) != 1 || response.Objects[0].URL == "" {
		return SignedPUT{}, errors.New("signer returned no PUT URL")
	}
	return response.Objects[0], nil
}

func (c *SignerClient) Metadata(ctx context.Context, locator string) (BlobMetadata, error) {
	var response struct {
		Objects []BlobMetadata `json:"objects"`
	}
	if err := c.post(ctx, "/metadata", map[string]any{"locators": []string{locator}}, &response); err != nil {
		return BlobMetadata{}, err
	}
	if len(response.Objects) != 1 {
		return BlobMetadata{}, errors.New("signer returned no metadata")
	}
	return response.Objects[0], nil
}

func (c *SignerClient) post(ctx context.Context, path string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		return fmt.Errorf("signer %s: status %d: %s", path, response.StatusCode,
			strings.TrimSpace(string(detail)))
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result)
}

func resolveURL(baseURL, reference string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	target, err := url.Parse(reference)
	if err != nil {
		return "", err
	}
	resolved := base.ResolveReference(target)
	if resolved.Scheme != base.Scheme || resolved.Host != base.Host {
		return "", errors.New("source URL escaped Core origin")
	}
	return resolved.String(), nil
}
