package edgeonepublisher

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestPublisherIdleAndDisabledNeverProbeCloud(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		fake := &publisherTransport{idle: true, disabled: disabled, backendFailure: true}
		state, err := OpenState(filepath.Join(t.TempDir(), "publisher.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { state.Close() })
		publisher := newPublisher(testPublisherConfig(), state, &http.Client{Transport: fake, Timeout: time.Minute})
		for range 3 {
			if err := publisher.PublishOnce(context.Background()); !errors.Is(err, ErrNoWork) {
				t.Fatalf("idle publish: %v", err)
			}
		}
		if disabled {
			if err := publisher.ReconcileStorage(context.Background()); !errors.Is(err, ErrNoWork) {
				t.Fatalf("disabled inventory: %v", err)
			}
			if err := publisher.drainDeletions(context.Background()); !errors.Is(err, ErrNoWork) {
				t.Fatalf("disabled deletion: %v", err)
			}
		}
		if fake.backendCalls != 0 {
			t.Fatalf("idle/disabled publisher made %d cloud requests", fake.backendCalls)
		}
	}
}

func TestPublisherReportsFastBackendFailureWithoutHealthProbe(t *testing.T) {
	fake := &publisherTransport{content: []byte("audio"), backendFailure: true}
	state, err := OpenState(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	publisher := newPublisher(testPublisherConfig(), state, &http.Client{Transport: fake, Timeout: time.Minute})
	if err := publisher.PublishOnce(context.Background()); err == nil {
		t.Fatal("backend failure was lost")
	}
	if fake.healthCalls != 0 || fake.failErrorCode != "publish_failed" {
		t.Fatalf("failure not routed through lease retry policy: probes=%d code=%q", fake.healthCalls, fake.failErrorCode)
	}
}

func TestPublisherUnknownLengthOversizeIsPolicyFailure(t *testing.T) {
	fake := &publisherTransport{content: bytes.Repeat([]byte("x"), 128), maxObjectBytes: 64, unknownLength: true}
	state, err := OpenState(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	publisher := newPublisher(testPublisherConfig(), state, &http.Client{Transport: fake, Timeout: time.Minute})
	var tooLarge objectTooLargeError
	if err := publisher.PublishOnce(context.Background()); !errors.As(err, &tooLarge) {
		t.Fatalf("unknown-length source: %v", err)
	}
	if fake.failErrorCode != "object_too_large" || len(fake.uploaded) != 0 || fake.backendCalls != 0 {
		t.Fatalf("unknown size was not stopped before cloud publishing: code=%q uploaded=%d cloud=%d", fake.failErrorCode, len(fake.uploaded), fake.backendCalls)
	}
}

func TestPublisherRecoveryFinishesCancellationInsteadOfRetrying(t *testing.T) {
	fake := &publisherTransport{cancelRequested: true}
	state, err := OpenState(filepath.Join(t.TempDir(), "publisher.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := state.Put(context.Background(), UploadState{LeaseID: "interrupted", TrackRef: "local:song", Owner: "publisher-1", ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), Status: "uploading"}); err != nil {
		t.Fatal(err)
	}
	publisher := newPublisher(testPublisherConfig(), state, &http.Client{Transport: fake, Timeout: time.Minute})
	if err := publisher.recoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fake.cancelConfirmed || fake.backendCalls != 0 {
		t.Fatal("recovery retried cloud work instead of completing cancellation")
	}
	uploads, err := state.List(context.Background())
	if err != nil || len(uploads) != 0 {
		t.Fatalf("canceled recovery left resumable work: %#v, %v", uploads, err)
	}
}
