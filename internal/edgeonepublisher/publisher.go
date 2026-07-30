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
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const immutableCacheControl = "public, max-age=31536000, immutable"

type Publisher struct {
	cfg    Config
	state  *State
	client *http.Client
	core   *CoreClient

	heartbeatMu sync.RWMutex
	heartbeat   PublisherHeartbeat
}

func New(cfg Config, state *State) *Publisher {
	client := &http.Client{Timeout: time.Duration(cfg.HTTPTimeoutSeconds) * time.Second}
	return newPublisher(cfg, state, client)
}

func newPublisher(cfg Config, state *State, client *http.Client) *Publisher {
	publisher := &Publisher{
		cfg: cfg, state: state, client: client,
		core: NewCoreClient(cfg.CoreURL, cfg.PublisherToken, client),
	}
	publisher.heartbeat = PublisherHeartbeat{
		Owner: cfg.Owner, Version: buildVersion(), State: "idle",
		Capabilities: []string{"object.publish", "storage.inventory", "object.delete"},
	}
	return publisher
}

func (p *Publisher) Run(ctx context.Context) error {
	if err := p.recoverInterrupted(ctx); err != nil {
		return err
	}
	go p.runHeartbeats(ctx)
	go p.runStorageLifecycle(ctx)
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

func (p *Publisher) recoverInterrupted(ctx context.Context) error {
	states, err := p.state.List(ctx)
	if err != nil {
		return fmt.Errorf("list interrupted uploads: %w", err)
	}
	for _, state := range states {
		lease := Lease{
			ID: state.LeaseID, TrackRef: state.TrackRef, Owner: state.Owner,
			ExpiresAt: state.ExpiresAt,
		}
		restartErr := fmt.Errorf("adapter restarted during %s", state.Status)
		err := p.core.Fail(ctx, lease, restartErr, 0)
		if err != nil && !errors.Is(err, ErrLeaseInactive) {
			return fmt.Errorf("release interrupted lease %s: %w", state.LeaseID, err)
		}
		if err := p.state.Delete(ctx, state.LeaseID); err != nil {
			return fmt.Errorf("clean interrupted upload %s: %w", state.LeaseID, err)
		}
		log.Printf("[edgeone] recovered interrupted lease %s (%s)", state.LeaseID, state.Status)
	}
	return nil
}

func (p *Publisher) runHeartbeats(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		p.sendHeartbeat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Publisher) runStorageLifecycle(ctx context.Context) {
	inventoryTicker := time.NewTicker(5 * time.Second)
	deleteTicker := time.NewTicker(5 * time.Second)
	defer inventoryTicker.Stop()
	defer deleteTicker.Stop()
	if err := p.ReconcileStorage(ctx); err != nil && !errors.Is(err, ErrNoWork) && ctx.Err() == nil {
		log.Printf("[edgeone] inventory: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-inventoryTicker.C:
			if err := p.ReconcileStorage(ctx); err != nil && !errors.Is(err, ErrNoWork) && ctx.Err() == nil {
				log.Printf("[edgeone] inventory: %v", err)
			}
		case <-deleteTicker.C:
			if err := p.DeleteOnce(ctx); err != nil && !errors.Is(err, ErrNoWork) && ctx.Err() == nil {
				log.Printf("[edgeone] delete: %v", err)
			}
		}
	}
}

func (p *Publisher) sendHeartbeat(ctx context.Context) {
	p.heartbeatMu.RLock()
	heartbeat := p.heartbeat
	heartbeat.Capabilities = append([]string(nil), p.heartbeat.Capabilities...)
	p.heartbeatMu.RUnlock()
	heartbeatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := p.core.Heartbeat(heartbeatCtx, heartbeat); err != nil && ctx.Err() == nil {
		log.Printf("[edgeone] heartbeat: %v", err)
	}
}

func (p *Publisher) setHeartbeat(state string, lease Lease, healthy bool, lastError string) {
	p.heartbeatMu.Lock()
	p.heartbeat.State = state
	p.heartbeat.LeaseID = lease.ID
	p.heartbeat.TrackRef = lease.TrackRef
	p.heartbeat.BackendHealthy = healthy
	p.heartbeat.LastError = lastError
	p.heartbeatMu.Unlock()
}

func (p *Publisher) PublishOnce(ctx context.Context) error {
	managed, err := p.core.ManagedConfig(ctx)
	if err != nil {
		p.setHeartbeat("degraded", Lease{}, false, err.Error())
		return err
	}
	if !managed.Enabled {
		p.setHeartbeat("idle", Lease{}, true, "")
		return ErrNoWork
	}
	backend := NewBackendClient(managed.BackendBaseURL, managed.BackendToken, p.client)
	if err := backend.Health(ctx); err != nil {
		p.setHeartbeat("degraded", Lease{}, false, err.Error())
		return err
	}
	lease, err := p.core.Claim(ctx, p.cfg.Owner, managed.LeaseTTLSeconds)
	if err != nil {
		if errors.Is(err, ErrNoWork) {
			p.setHeartbeat("idle", Lease{}, true, "")
		}
		return err
	}
	p.setHeartbeat("busy", lease, true, "")
	state := UploadState{
		LeaseID: lease.ID, TrackRef: lease.TrackRef, Owner: lease.Owner,
		ExpiresAt: lease.ExpiresAt, Status: "claimed",
	}
	if err := p.state.Put(ctx, state); err != nil {
		return p.fail(ctx, lease, state, err, time.Minute)
	}

	workCtx, cancelWork := context.WithCancelCause(ctx)
	defer cancelWork(nil)
	watchCtx, stopWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		p.watchCancellation(watchCtx, lease, cancelWork)
	}()

	reporter := newProgressReporter(p.core, lease)
	candidate, publishErr := p.publish(workCtx, managed, backend, lease, &state, reporter)
	stopWatch()
	<-watchDone
	if errors.Is(context.Cause(workCtx), ErrCancellationRequested) {
		return p.finishCancellation(lease, state)
	}
	if publishErr != nil {
		retry := retryDelay(publishErr)
		return p.fail(ctx, lease, state, publishErr, retry)
	}
	current, err := p.core.LeaseStatus(ctx, lease)
	if err != nil {
		return p.fail(ctx, lease, state, err, time.Minute)
	}
	if current.CancelRequested {
		return p.finishCancellation(lease, state)
	}
	_ = reporter.report(ctx, "completing", true)
	if err := p.core.Complete(ctx, lease, candidate); err != nil {
		if errors.Is(err, ErrCancellationRequested) {
			return p.finishCancellation(lease, state)
		}
		return p.fail(ctx, lease, state, err, time.Minute)
	}
	if err := p.state.Delete(ctx, lease.ID); err != nil {
		return fmt.Errorf("clean local state: %w", err)
	}
	p.setHeartbeat("idle", Lease{}, true, "")
	log.Printf("[edgeone] ready %s -> %s (%d bytes)", lease.TrackRef, candidate.Locator, candidate.SizeBytes)
	return nil
}

func (p *Publisher) watchCancellation(
	ctx context.Context,
	lease Lease,
	cancel context.CancelCauseFunc,
) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		current, err := p.core.LeaseStatus(ctx, lease)
		if err == nil && current.CancelRequested {
			cancel(ErrCancellationRequested)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Publisher) finishCancellation(lease Lease, state UploadState) error {
	cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.core.Cancel(cancelCtx, lease); err != nil && !errors.Is(err, ErrLeaseInactive) {
		p.setHeartbeat("degraded", Lease{}, false, err.Error())
		return fmt.Errorf("confirm canceled lease: %w", err)
	}
	if state.TempPath != "" {
		_ = os.Remove(state.TempPath)
	}
	if err := p.state.Delete(cancelCtx, lease.ID); err != nil {
		return fmt.Errorf("clean canceled lease state: %w", err)
	}
	p.setHeartbeat("idle", Lease{}, true, "")
	log.Printf("[edgeone] canceled %s", lease.TrackRef)
	return nil
}

func (p *Publisher) ReconcileStorage(ctx context.Context) error {
	managed, err := p.core.ManagedConfig(ctx)
	if err != nil {
		return err
	}
	scan, err := p.core.ClaimInventory(ctx, p.cfg.Owner, 30*60)
	if err != nil {
		return err
	}
	backend := NewBackendClient(managed.BackendBaseURL, managed.BackendToken, p.client)
	observedAt := time.Now().UnixMilli()
	cursor := ""
	for {
		objects, nextCursor, scanErr := backend.Inventory(ctx, cursor)
		if scanErr != nil {
			return p.failInventory(scan, scanErr)
		}
		if len(objects) == 0 {
			if scanErr := p.core.Inventory(ctx, scan, observedAt, nil,
				nextCursor == ""); scanErr != nil {
				return p.failInventory(scan, scanErr)
			}
		}
		for start := 0; start < len(objects); start += 200 {
			end := min(start+200, len(objects))
			complete := nextCursor == "" && end == len(objects)
			if scanErr := p.core.Inventory(ctx, scan, observedAt,
				objects[start:end], complete); scanErr != nil {
				return p.failInventory(scan, scanErr)
			}
		}
		if nextCursor == "" {
			return nil
		}
		if nextCursor == cursor {
			return p.failInventory(scan, errors.New("backend inventory cursor did not advance"))
		}
		cursor = nextCursor
	}
}

func (p *Publisher) failInventory(scan InventoryScan, scanErr error) error {
	reportCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.core.FailInventory(reportCtx, scan, scanErr); err != nil {
		return fmt.Errorf("%v; report inventory failure: %w", scanErr, err)
	}
	return scanErr
}

func (p *Publisher) DeleteOnce(ctx context.Context) error {
	managed, err := p.core.ManagedConfig(ctx)
	if err != nil {
		return err
	}
	deletion, err := p.core.ClaimDeletion(ctx, p.cfg.Owner, 600)
	if err != nil {
		return err
	}
	backend := NewBackendClient(managed.BackendBaseURL, managed.BackendToken, p.client)
	if err := backend.Delete(ctx, deletion.Locator); err != nil {
		reportErr := p.core.FailDeletion(ctx, deletion, err, time.Minute)
		if reportErr != nil {
			return fmt.Errorf("%v; report deletion failure: %w", err, reportErr)
		}
		return err
	}
	if err := p.core.CompleteDeletion(ctx, deletion); err != nil {
		return err
	}
	return nil
}

func (p *Publisher) publish(
	ctx context.Context,
	managed ManagedConfig,
	backend *BackendClient,
	lease Lease,
	state *UploadState,
	reporter *progressReporter,
) (Candidate, error) {
	tempPath, contentVersion, size, contentType, err := p.downloadSource(ctx, managed, lease, reporter)
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
	reservation, err := p.core.Reserve(ctx, lease, state.Locator, size)
	if err != nil {
		return Candidate{}, fmt.Errorf("reserve storage: %w", err)
	}
	if reservation.AlreadyPresent {
		metadata, err := backend.Metadata(ctx, state.Locator)
		if err != nil {
			return Candidate{}, fmt.Errorf("verify existing object: %w", err)
		}
		if err := verifyMetadata(metadata, state.Locator, size, contentType); err != nil {
			return Candidate{}, err
		}
		return Candidate{
			ContentVersion: contentVersion, Locator: state.Locator, Layout: "object",
			SizeBytes: size, ContentType: contentType, ETag: metadata.ETag,
		}, nil
	}

	signed, err := backend.SignPUT(ctx, state.Locator, contentType)
	if err != nil {
		return Candidate{}, fmt.Errorf("sign PUT: %w", err)
	}
	state.Status = "uploading"
	if err := p.state.Put(ctx, *state); err != nil {
		return Candidate{}, err
	}
	reporter.total = size
	_ = reporter.report(ctx, "uploading", true)
	etag, err := p.upload(ctx, managed, signed.URL, tempPath, size, contentType, reporter)
	if err != nil {
		return Candidate{}, err
	}
	_ = reporter.report(ctx, "verifying", true)
	metadata, err := backend.Metadata(ctx, state.Locator)
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
		ContentVersion: contentVersion, Locator: state.Locator, Layout: "object",
		SizeBytes: size, ContentType: contentType, ETag: etag,
	}, nil
}

func (p *Publisher) downloadSource(
	ctx context.Context,
	managed ManagedConfig,
	lease Lease,
	reporter *progressReporter,
) (string, string, int64, string, error) {
	response, err := p.core.Source(ctx, lease)
	if err != nil {
		return "", "", 0, "", fmt.Errorf("open source: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		return "", "", 0, "", fmt.Errorf("source status %d: %s", response.StatusCode, strings.TrimSpace(string(detail)))
	}
	if response.ContentLength > managed.MaxObjectBytes {
		return "", "", 0, "", objectTooLargeError{size: response.ContentLength, max: managed.MaxObjectBytes}
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
	reporter.total = max(response.ContentLength, 0)
	_ = reporter.report(ctx, "downloading", true)
	hash := sha256.New()
	limited := io.LimitReader(response.Body, managed.MaxObjectBytes+1)
	reader := &progressReader{reader: limited, add: func(bytes int64) {
		reporter.sourceBytes += bytes
		_ = reporter.report(ctx, "downloading", false)
	}}
	size, err := io.Copy(io.MultiWriter(temp, hash), reader)
	if err != nil {
		return "", "", 0, "", fmt.Errorf("read source: %w", err)
	}
	if size > managed.MaxObjectBytes {
		return "", "", 0, "", objectTooLargeError{size: size, max: managed.MaxObjectBytes}
	}
	if response.ContentLength >= 0 && size != response.ContentLength {
		return "", "", 0, "", fmt.Errorf("source length mismatch: got %d, want %d", size, response.ContentLength)
	}
	reporter.total = size
	_ = reporter.report(ctx, "downloading", true)
	if err := temp.Sync(); err != nil {
		return "", "", 0, "", err
	}
	if err := temp.Close(); err != nil {
		return "", "", 0, "", err
	}
	contentType := strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	ok = true
	return tempPath, hex.EncodeToString(hash.Sum(nil)), size, contentType, nil
}

func (p *Publisher) upload(
	ctx context.Context,
	managed ManagedConfig,
	signedURL, path string,
	size int64,
	contentType string,
	reporter *progressReporter,
) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	var body io.Reader = file
	if managed.UploadRateBytesPerSecond > 0 {
		body = newPacedReader(body, managed.UploadRateBytesPerSecond)
	}
	body = &progressReader{reader: body, add: func(bytes int64) {
		reporter.uploadBytes += bytes
		_ = reporter.report(ctx, "uploading", false)
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, signedURL, body)
	if err != nil {
		return "", err
	}
	request.ContentLength = size
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Cache-Control", immutableCacheControl)
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("upload Blob: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		return "", fmt.Errorf("upload Blob: status %d: %s", response.StatusCode, strings.TrimSpace(string(detail)))
	}
	_ = reporter.report(ctx, "uploading", true)
	return response.Header.Get("ETag"), nil
}

func (p *Publisher) fail(ctx context.Context, lease Lease, state UploadState, publishErr error, retry time.Duration) error {
	state.Status = "failed"
	state.LastError = publishErr.Error()
	_ = p.state.Put(context.Background(), state)
	failCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.core.Fail(failCtx, lease, publishErr, retry); err != nil {
		p.setHeartbeat("degraded", Lease{}, false, publishErr.Error())
		return fmt.Errorf("%v; report failure: %w", publishErr, err)
	}
	_ = p.state.Delete(context.Background(), lease.ID)
	p.setHeartbeat("degraded", Lease{}, true, publishErr.Error())
	return publishErr
}

func verifyMetadata(metadata BlobMetadata, locator string, size int64, contentType string) error {
	if !metadata.Exists || metadata.Locator != locator {
		return errors.New("uploaded Blob metadata not found")
	}
	if metadata.SizeBytes != size {
		return fmt.Errorf("uploaded Blob size mismatch: got %d, want %d", metadata.SizeBytes, size)
	}
	if metadata.ContentType != "" && metadata.ContentType != contentType {
		return fmt.Errorf("uploaded Blob content type mismatch: got %q, want %q", metadata.ContentType, contentType)
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

type progressReporter struct {
	core         *CoreClient
	lease        Lease
	sourceBytes  int64
	uploadBytes  int64
	total        int64
	lastBytes    int64
	lastReported time.Time
}

func newProgressReporter(core *CoreClient, lease Lease) *progressReporter {
	return &progressReporter{core: core, lease: lease}
}

func (r *progressReporter) report(ctx context.Context, phase string, force bool) error {
	currentBytes := r.sourceBytes + r.uploadBytes
	if !force && time.Since(r.lastReported) < 2*time.Second && currentBytes-r.lastBytes < 1<<20 {
		return nil
	}
	if err := r.core.Progress(ctx, r.lease, phase, r.sourceBytes, r.uploadBytes, r.total); err != nil {
		return err
	}
	r.lastBytes = currentBytes
	r.lastReported = time.Now()
	return nil
}

type progressReader struct {
	reader io.Reader
	add    func(int64)
}

func (r *progressReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	if count > 0 {
		r.add(int64(count))
	}
	return count, err
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
	count, err := r.r.Read(buffer)
	r.read += int64(count)
	if count > 0 && r.rate > 0 {
		expected := time.Duration(float64(r.read) / float64(r.rate) * float64(time.Second))
		if delay := expected - time.Since(r.started); delay > 0 {
			time.Sleep(delay)
		}
	}
	return count, err
}

func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				return setting.Value
			}
		}
	}
	return "devel"
}
