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
	ID        string `json:"id"`
	Backend   string `json:"backend"`
	TrackRef  string `json:"track_ref"`
	Owner     string `json:"owner"`
	ExpiresAt int64  `json:"expires_at"`
	CreatedAt int64  `json:"created_at"`
	SourceURL string `json:"-"`
}

type Candidate struct {
	ContentVersion string `json:"content_version"`
	Locator        string `json:"locator"`
	Layout         string `json:"layout"`
	SizeBytes      int64  `json:"size_bytes"`
	ContentType    string `json:"content_type"`
	ETag           string `json:"etag,omitempty"`
}

type CoreClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewCoreClient(baseURL, token string, client *http.Client) *CoreClient {
	return &CoreClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: client}
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
	sourceURL, err := resolveURL(c.baseURL, lease.SourceURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.client.Do(req)
}

func (c *CoreClient) Complete(ctx context.Context, lease Lease, candidate Candidate) error {
	body := map[string]any{
		"owner": lease.Owner, "content_version": candidate.ContentVersion,
		"locator": candidate.Locator, "layout": candidate.Layout,
		"size_bytes": candidate.SizeBytes, "content_type": candidate.ContentType,
		"etag": candidate.ETag,
	}
	status, err := c.json(
		ctx, http.MethodPost,
		"/internal/v1/distribution/leases/"+url.PathEscape(lease.ID)+"/complete",
		body, nil,
	)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("complete lease: status %d", status)
	}
	return nil
}

func (c *CoreClient) Fail(ctx context.Context, lease Lease, publishErr error, retryAfter time.Duration) error {
	message := "publish failed"
	if publishErr != nil {
		message = publishErr.Error()
	}
	status, err := c.json(
		ctx, http.MethodPost,
		"/internal/v1/distribution/leases/"+url.PathEscape(lease.ID)+"/fail",
		map[string]any{
			"owner": lease.Owner, "error": message,
			"retry_after_seconds": int(retryAfter / time.Second),
		}, nil,
	)
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
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return resp.StatusCode, fmt.Errorf("core %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if result != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
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
	CacheControl string `json:"cache_control"`
	ETag         string `json:"etag"`
}

func NewSignerClient(baseURL, token string, client *http.Client) *SignerClient {
	return &SignerClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: client}
}

func (c *SignerClient) SignPUT(ctx context.Context, locator, contentType string) (SignedPUT, error) {
	var response struct {
		Objects []SignedPUT `json:"objects"`
	}
	if err := c.post(ctx, "/put-urls", map[string]any{
		"objects": []map[string]any{{"locator": locator, "content_type": contentType}},
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("signer %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result)
}

func resolveURL(baseURL, reference string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(reference)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}
