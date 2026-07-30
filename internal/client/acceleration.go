package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

type AccelerationLease struct {
	ID              string `json:"id"`
	Owner           string `json:"owner"`
	ExpiresAt       int64  `json:"expires_at"`
	CancelRequested bool   `json:"cancel_requested"`
}

type AccelerationProgress struct {
	Phase       string `json:"phase"`
	SourceBytes int64  `json:"source_bytes"`
	UploadBytes int64  `json:"upload_bytes"`
	TotalBytes  int64  `json:"total_bytes"`
	UpdatedAt   int64  `json:"updated_at"`
}

type AccelerationRequest struct {
	AccelerationID    string                `json:"acceleration_id"`
	TrackRef          string                `json:"track_ref"`
	State             string                `json:"state"`
	PendingReason     string                `json:"pending_reason"`
	RequestedAt       int64                 `json:"requested_at"`
	UpdatedAt         int64                 `json:"updated_at"`
	NextAttemptAt     int64                 `json:"next_attempt_at"`
	Attempts          int64                 `json:"attempts"`
	LastError         string                `json:"last_error"`
	CancelRequestedAt int64                 `json:"cancel_requested_at"`
	CanceledAt        int64                 `json:"canceled_at"`
	Lease             *AccelerationLease    `json:"lease"`
	Progress          *AccelerationProgress `json:"progress"`
}

type AccelerationStorageStatus struct {
	Managed             bool   `json:"managed"`
	AccountedBytes      int64  `json:"accounted_bytes"`
	ReservedBytes       int64  `json:"reserved_bytes"`
	ObservedBytes       int64  `json:"observed_bytes"`
	ManagedObjectCount  int64  `json:"managed_object_count"`
	ObservedObjectCount int64  `json:"observed_object_count"`
	OrphanCount         int64  `json:"orphan_count"`
	MissingCount        int64  `json:"missing_count"`
	ObservedAt          int64  `json:"observed_at"`
	StaleAfterSeconds   int    `json:"stale_after_seconds"`
	Stale               bool   `json:"stale"`
	ReconciliationError string `json:"reconciliation_error"`
}

type AccelerationInventoryScan struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	Owner          string `json:"owner"`
	Attempts       int64  `json:"attempts"`
	LeaseExpiresAt int64  `json:"lease_expires_at"`
	ObservedAt     int64  `json:"observed_at"`
	LastError      string `json:"last_error"`
	RequestedAt    int64  `json:"requested_at"`
	StartedAt      int64  `json:"started_at"`
	CompletedAt    int64  `json:"completed_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

type AccelerationInventoryStatus struct {
	Storage AccelerationStorageStatus  `json:"storage"`
	Scan    *AccelerationInventoryScan `json:"scan"`
}

func RESTAccelerationRequests(
	ctx context.Context,
	server, token, accelerationID, state string,
	limit int,
) ([]AccelerationRequest, error) {
	query := url.Values{}
	if state != "" {
		query.Set("state", state)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/v1/accelerations/" + url.PathEscape(accelerationID) + "/requests"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out struct {
		Requests []AccelerationRequest `json:"requests"`
	}
	err := restCall(ctx, server, http.MethodGet, path, token, nil, &out)
	return out.Requests, err
}

func RESTAccelerationRequest(
	ctx context.Context,
	server, token, accelerationID, trackRef string,
) (AccelerationRequest, error) {
	var out struct {
		Request AccelerationRequest `json:"request"`
	}
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/accelerations/"+url.PathEscape(accelerationID)+"/requests/"+url.PathEscape(trackRef),
		token, nil, &out)
	return out.Request, err
}

func RESTCancelAccelerationRequest(
	ctx context.Context,
	server, token, accelerationID, trackRef string,
) (AccelerationRequest, error) {
	var out struct {
		Request AccelerationRequest `json:"request"`
	}
	err := restCall(ctx, server, http.MethodDelete,
		"/api/v1/accelerations/"+url.PathEscape(accelerationID)+"/requests/"+url.PathEscape(trackRef),
		token, nil, &out)
	return out.Request, err
}

func RESTAccelerationInventoryStatus(
	ctx context.Context,
	server, token, accelerationID string,
) (AccelerationInventoryStatus, error) {
	var out AccelerationInventoryStatus
	err := restCall(ctx, server, http.MethodGet,
		"/api/v1/accelerations/"+url.PathEscape(accelerationID)+"/inventory/status",
		token, nil, &out)
	return out, err
}

func RESTRefreshAccelerationInventory(
	ctx context.Context,
	server, token, accelerationID string,
) (AccelerationInventoryScan, error) {
	var out struct {
		Scan AccelerationInventoryScan `json:"scan"`
	}
	err := restCall(ctx, server, http.MethodPost,
		"/api/v1/accelerations/"+url.PathEscape(accelerationID)+"/inventory/refresh",
		token, nil, &out)
	return out.Scan, err
}
