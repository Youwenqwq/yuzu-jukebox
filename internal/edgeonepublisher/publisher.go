package edgeonepublisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const immutableCacheControl = "public, max-age=31536000, immutable"

type Publisher struct {
	cfg    Config
	core   *CoreClient
	signer *SignerClient
	state  *State
	client *http.Client
}

func New(cfg Config, state *State) *Publisher {
	client := &http.Client{Timeout: time.Duration(cfg.HTTPTimeoutSeconds) * time.Second}
	return newPublisher(cfg, state, client)
}

func newPublisher(cfg Config, state *State, client *http.Client) *Publisher {
	return &Publisher{
		cfg: cfg, state: state, client: client,
		core:   NewCoreClient(cfg.CoreURL, cfg.PublisherToken, client),
		signer: NewSignerClient(cfg.SignerBaseURL, cfg.SignerToken, client),
	}
}

func (p *Publisher) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Duration(p.cfg.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		err := p.PublishOnce(ctx)
		if err != nil && !errors.Is(err, ErrNoWork) && !errors.Is(err, context.Canceled) {
			log.Printf("[edgeone] publish: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *Publisher) PublishOnce(ctx context.Context) error {
	lease, err := p.core.Claim(ctx, p.cfg.Owner, p.cfg.LeaseSeconds)
	if err != nil {
		return err
	}
	state := UploadState{
		LeaseID: lease.ID, TrackRef: lease.TrackRef, Owner: lease.Owner,
		ExpiresAt: lease.ExpiresAt, Status: "claimed",
	}
	if err := p.state.Put(ctx, state); err != nil {
		return p.fail(ctx, lease, state, err, time.Minute)
	}

	candidate, err := p.publish(ctx, lease, &state)
	if err != nil {
		retry := retryDelay(err)
		return p.fail(ctx, lease, state, err, retry)
	}
	if err := p.core.Complete(ctx, lease, candidate); err != nil {
		return p.fail(ctx, lease, state, err, time.Minute)
	}
	if err := p.state.Delete(ctx, lease.ID); err != nil {
		return fmt.Errorf("clean local state: %w", err)
	}
	log.Printf("[edgeone] ready %s -> %s (%d bytes)", lease.TrackRef, candidate.Locator, candidate.SizeBytes)
	return nil
}

func (p *Publisher) publish(ctx context.Context, lease Lease, state *UploadState) (Candidate, error) {
	tempPath, contentVersion, size, contentType, err := p.downloadSource(ctx, lease)
	if err != nil {
		return Candidate{}, err
	}
	state.Status = "downloaded"
	state.TempPath = tempPath
	state.ContentVersion = contentVersion
	state.Locator = "media/" + contentVersion + "/object"
	state.SizeBytes = size
	state.ContentType = contentType
	if err := p.state.Put(ctx, *state); err != nil {
		return Candidate{}, err
	}

	signed, err := p.signer.SignPUT(ctx, state.Locator, contentType)
	if err != nil {
		return Candidate{}, fmt.Errorf("sign PUT: %w", err)
	}
	state.Status = "uploading"
	if err := p.state.Put(ctx, *state); err != nil {
		return Candidate{}, err
	}
	etag, err := p.upload(ctx, signed.URL, tempPath, size, contentType)
	if err != nil {
		return Candidate{}, err
	}
	metadata, err := p.signer.Metadata(ctx, state.Locator)
	if err != nil {
		return Candidate{}, fmt.Errorf("verify metadata: %w", err)
	}
	if err := verifyMetadata(metadata, state.Locator, size, contentType); err != nil {
		return Candidate{}, err
	}
	if metadata.ETag != "" {
		etag = metadata.ETag
	}
	return Candidate{
		ContentVersion: contentVersion,
		Locator:        state.Locator,
		Layout:         "object",
		SizeBytes:      size,
		ContentType:    contentType,
		ETag:           etag,
	}, nil
}

func (p *Publisher) downloadSource(ctx context.Context, lease Lease) (string, string, int64, string, error) {
	resp, err := p.core.Source(ctx, lease)
	if err != nil {
		return "", "", 0, "", fmt.Errorf("open source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return "", "", 0, "", fmt.Errorf("source status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	if resp.ContentLength > p.cfg.MaxObjectBytes {
		return "", "", 0, "", objectTooLargeError{size: resp.ContentLength, max: p.cfg.MaxObjectBytes}
	}
	if err := os.MkdirAll(p.state.ObjectDir(), 0o755); err != nil {
		return "", "", 0, "", err
	}
	temp, err := os.CreateTemp(p.state.ObjectDir(), "publish-*.part")
	if err != nil {
		return "", "", 0, "", err
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	hash := sha256.New()
	limited := io.LimitReader(resp.Body, p.cfg.MaxObjectBytes+1)
	size, err := io.Copy(io.MultiWriter(temp, hash), limited)
	if err != nil {
		return "", "", 0, "", fmt.Errorf("read source: %w", err)
	}
	if size > p.cfg.MaxObjectBytes {
		return "", "", 0, "", objectTooLargeError{size: size, max: p.cfg.MaxObjectBytes}
	}
	if resp.ContentLength >= 0 && size != resp.ContentLength {
		return "", "", 0, "", fmt.Errorf("source length mismatch: got %d, want %d", size, resp.ContentLength)
	}
	if err := temp.Sync(); err != nil {
		return "", "", 0, "", err
	}
	if err := temp.Close(); err != nil {
		return "", "", 0, "", err
	}
	contentType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ok = true
	return tempPath, hex.EncodeToString(hash.Sum(nil)), size, contentType, nil
}

func (p *Publisher) upload(ctx context.Context, signedURL, path string, size int64, contentType string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	body := io.Reader(f)
	if p.cfg.UploadRateBytesPerSecond > 0 {
		body = newPacedReader(body, p.cfg.UploadRateBytesPerSecond)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, signedURL, body)
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Cache-Control", immutableCacheControl)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload Blob: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return "", fmt.Errorf("upload Blob: status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return resp.Header.Get("ETag"), nil
}

func (p *Publisher) fail(ctx context.Context, lease Lease, state UploadState, publishErr error, retry time.Duration) error {
	state.Status = "failed"
	state.LastError = publishErr.Error()
	_ = p.state.Put(context.Background(), state)
	failCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.core.Fail(failCtx, lease, publishErr, retry); err != nil {
		return fmt.Errorf("%v; report failure: %w", publishErr, err)
	}
	_ = p.state.Delete(context.Background(), lease.ID)
	return publishErr
}

func verifyMetadata(metadata BlobMetadata, locator string, size int64, contentType string) error {
	if !metadata.Exists || metadata.Locator != locator {
		return errors.New("uploaded Blob metadata not found")
	}
	if metadata.SizeBytes != size {
		return fmt.Errorf("Blob size mismatch: got %d, want %d", metadata.SizeBytes, size)
	}
	if metadata.ContentType != contentType {
		return fmt.Errorf("Blob content type mismatch: got %q, want %q", metadata.ContentType, contentType)
	}
	if !strings.Contains(strings.ToLower(metadata.CacheControl), "immutable") {
		return fmt.Errorf("Blob cache-control is not immutable: %q", metadata.CacheControl)
	}
	return nil
}

type objectTooLargeError struct{ size, max int64 }

func (e objectTooLargeError) Error() string {
	return fmt.Sprintf("object is too large for complete-object layout: %d > %d", e.size, e.max)
}

func retryDelay(err error) time.Duration {
	var tooLarge objectTooLargeError
	if errors.As(err, &tooLarge) {
		return 7 * 24 * time.Hour
	}
	return time.Minute
}

type pacedReader struct {
	r       io.Reader
	rate    int64
	started time.Time
	read    int64
}

func newPacedReader(reader io.Reader, bytesPerSecond int64) io.Reader {
	return &pacedReader{r: reader, rate: bytesPerSecond, started: time.Now()}
}

func (r *pacedReader) Read(buffer []byte) (int, error) {
	if len(buffer) > 32<<10 {
		buffer = buffer[:32<<10]
	}
	n, err := r.r.Read(buffer)
	r.read += int64(n)
	if n > 0 && r.rate > 0 {
		target := time.Duration(float64(r.read) / float64(r.rate) * float64(time.Second))
		if wait := target - time.Since(r.started); wait > 0 {
			timer := time.NewTimer(wait)
			<-timer.C
		}
	}
	return n, err
}
