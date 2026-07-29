package edgeonepublisher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPublisherCompleteObject(t *testing.T) {
	content := bytes.Repeat([]byte("yuzu-audio-"), 1024)
	hash := sha256.Sum256(content)
	version := hex.EncodeToString(hash[:])
	fake := &publisherTransport{content: content, expectedVersion: version}
	state, err := OpenState(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	cfg := testPublisherConfig()
	client := &http.Client{Transport: fake, Timeout: time.Minute}
	publisher := newPublisher(cfg, state, client)
	if err := publisher.PublishOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !bytes.Equal(fake.uploaded, content) {
		t.Fatalf("uploaded content mismatch: got %d bytes", len(fake.uploaded))
	}
	if fake.uploadContentType != "audio/mpeg" {
		t.Fatalf("upload content type = %q", fake.uploadContentType)
	}
	if !strings.Contains(fake.uploadCacheControl, "immutable") {
		t.Fatalf("upload cache-control = %q", fake.uploadCacheControl)
	}
	if fake.completed.ContentVersion != version || fake.completed.Locator != "media/"+version+"/object" {
		t.Fatalf("completed candidate = %#v", fake.completed)
	}
	if fake.completed.SizeBytes != int64(len(content)) || fake.completed.ContentType != "audio/mpeg" {
		t.Fatalf("completed candidate = %#v", fake.completed)
	}
	var states int
	if err := state.db.QueryRow(`SELECT COUNT(*) FROM uploads`).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != 0 {
		t.Fatalf("local upload states = %d, want 0", states)
	}
}

func TestPublisherRejectsOversizedObject(t *testing.T) {
	fake := &publisherTransport{content: bytes.Repeat([]byte("x"), 128)}
	state, err := OpenState(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { state.Close() })
	fake.maxObjectBytes = 64
	cfg := testPublisherConfig()
	publisher := newPublisher(cfg, state, &http.Client{Transport: fake, Timeout: time.Minute})
	err = publisher.PublishOnce(context.Background())
	var tooLarge objectTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %v, want objectTooLargeError", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.failRetrySeconds != int((7*24*time.Hour)/time.Second) {
		t.Fatalf("retry seconds = %d", fake.failRetrySeconds)
	}
	if len(fake.uploaded) != 0 {
		t.Fatal("oversized object was uploaded")
	}
}

func testPublisherConfig() Config {
	return Config{
		CoreURL: "https://core.test", PublisherToken: "publisher-token",
		StatePath: "publisher.db", Owner: "publisher-1",
		PollIntervalSeconds: 1, HTTPTimeoutSeconds: 60,
	}
}

type publisherTransport struct {
	mu sync.Mutex

	content            []byte
	expectedVersion    string
	uploaded           []byte
	uploadContentType  string
	uploadCacheControl string
	completed          Candidate
	failRetrySeconds   int
	maxObjectBytes     int64
}

func (f *publisherTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	switch {
	case request.URL.Host == "core.test" && request.URL.Path == "/internal/v1/distribution/publisher/config":
		maxObjectBytes := f.maxObjectBytes
		if maxObjectBytes == 0 {
			maxObjectBytes = 1 << 20
		}
		return jsonHTTPResponse(request, http.StatusOK, map[string]any{
			"acceleration_id": "edgeone-main", "enabled": true, "kind": "edgeone",
			"signer_base_url": "https://signer.test/yuzu-blob",
			"signer_token":    "signer-token", "lease_ttl_seconds": 600,
			"upload_rate_bytes_per_second": 0, "max_object_bytes": maxObjectBytes,
		})
	case request.URL.Host == "core.test" && request.URL.Path == "/internal/v1/distribution/leases":
		return jsonHTTPResponse(request, http.StatusCreated, map[string]any{
			"lease": map[string]any{
				"id": "lease-1", "acceleration_id": "edgeone-main", "track_ref": "local:song",
				"owner": "publisher-1", "expires_at": time.Now().Add(10 * time.Minute).UnixMilli(),
				"created_at": time.Now().UnixMilli(),
			},
			"source_url": "/internal/v1/distribution/leases/lease-1/source",
		})
	case request.URL.Host == "core.test" && strings.HasSuffix(request.URL.Path, "/source"):
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":   []string{"audio/mpeg"},
				"Content-Length": []string{strconv.Itoa(len(f.content))},
			},
			Body:          io.NopCloser(bytes.NewReader(f.content)),
			ContentLength: int64(len(f.content)), Request: request,
		}, nil
	case request.URL.Host == "core.test" && strings.HasSuffix(request.URL.Path, "/progress"):
		return jsonHTTPResponse(request, http.StatusOK, map[string]any{"lease": map[string]any{"id": "lease-1"}})
	case request.URL.Host == "signer.test" && strings.HasSuffix(request.URL.Path, "/health"):
		return jsonHTTPResponse(request, http.StatusOK, map[string]any{"ok": true})
	case request.URL.Host == "signer.test" && strings.HasSuffix(request.URL.Path, "/put-urls"):
		locator := "media/" + f.expectedVersion + "/object"
		if f.expectedVersion == "" {
			var body struct {
				Objects []struct {
					Locator string `json:"locator"`
				} `json:"objects"`
			}
			_ = json.NewDecoder(request.Body).Decode(&body)
			locator = body.Objects[0].Locator
		}
		return jsonHTTPResponse(request, http.StatusOK, map[string]any{
			"objects": []map[string]any{{
				"locator": locator, "url": "https://blob.test/object", "expires_at": 9999999999,
			}},
		})
	case request.URL.Host == "blob.test" && request.Method == http.MethodPut:
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.uploaded = body
		f.uploadContentType = request.Header.Get("Content-Type")
		f.uploadCacheControl = request.Header.Get("Cache-Control")
		f.mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"ETag": []string{"etag-upload"}},
			Body: io.NopCloser(strings.NewReader("")), Request: request,
		}, nil
	case request.URL.Host == "signer.test" && strings.HasSuffix(request.URL.Path, "/metadata"):
		f.mu.Lock()
		size := len(f.uploaded)
		f.mu.Unlock()
		locator := "media/" + f.expectedVersion + "/object"
		return jsonHTTPResponse(request, http.StatusOK, map[string]any{
			"objects": []map[string]any{{
				"locator": locator, "exists": true, "size_bytes": size,
				"content_type": "audio/mpeg", "cache_control": immutableCacheControl,
				"etag": "etag-metadata",
			}},
		})
	case request.URL.Host == "core.test" && strings.HasSuffix(request.URL.Path, "/complete"):
		var body struct {
			ContentVersion string `json:"content_version"`
			Locator        string `json:"locator"`
			Layout         string `json:"layout"`
			SizeBytes      int64  `json:"size_bytes"`
			ContentType    string `json:"content_type"`
			ETag           string `json:"etag"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.completed = Candidate{
			ContentVersion: body.ContentVersion, Locator: body.Locator, Layout: body.Layout,
			SizeBytes: body.SizeBytes, ContentType: body.ContentType, ETag: body.ETag,
		}
		f.mu.Unlock()
		return jsonHTTPResponse(request, http.StatusOK, map[string]any{"ready": true})
	case request.URL.Host == "core.test" && strings.HasSuffix(request.URL.Path, "/fail"):
		var body struct {
			RetryAfterSeconds int `json:"retry_after_seconds"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.failRetrySeconds = body.RetryAfterSeconds
		f.mu.Unlock()
		return jsonHTTPResponse(request, http.StatusOK, map[string]any{"failed": true})
	default:
		return jsonHTTPResponse(request, http.StatusNotFound, map[string]any{"path": request.URL.String()})
	}
}

func jsonHTTPResponse(request *http.Request, status int, value any) (*http.Response, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data)), Request: request,
	}, nil
}
