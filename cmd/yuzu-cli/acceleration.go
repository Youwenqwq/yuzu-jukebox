package main

import (
	"context"
	"fmt"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/client"
)

func cmdAccelerationRequests(ctx context.Context, accelerationID, state string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	requests, err := client.RESTAccelerationRequests(ctx, *server, token, accelerationID, state, *limit)
	if err != nil {
		return err
	}
	if len(requests) == 0 {
		fmt.Println("(no requests)")
		return nil
	}
	for _, request := range requests {
		detail := request.PendingReason
		if request.Progress != nil {
			detail = request.Progress.Phase
		}
		if detail == "" {
			detail = "-"
		}
		fmt.Printf("%-18s %-40s %-20s attempts=%d updated=%s\n",
			request.State, request.TrackRef, detail, request.Attempts,
			formatAccelerationTime(request.UpdatedAt))
	}
	return nil
}

func cmdAccelerationRequest(ctx context.Context, accelerationID, trackRef string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	request, err := client.RESTAccelerationRequest(ctx, *server, token, accelerationID, trackRef)
	if err != nil {
		return err
	}
	printAccelerationRequest(request)
	return nil
}

func cmdAccelerationCancel(ctx context.Context, accelerationID, trackRef string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	request, err := client.RESTCancelAccelerationRequest(ctx, *server, token, accelerationID, trackRef)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", request.TrackRef, request.State)
	return nil
}

func cmdAccelerationInventoryStatus(ctx context.Context, accelerationID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	status, err := client.RESTAccelerationInventoryStatus(ctx, *server, token, accelerationID)
	if err != nil {
		return err
	}
	freshness := "fresh"
	if status.Storage.Stale {
		freshness = "stale"
	}
	fmt.Printf("inventory: %s, observed=%s\n", freshness, formatAccelerationTime(status.Storage.ObservedAt))
	fmt.Printf("objects: managed=%d observed=%d orphan=%d missing=%d\n",
		status.Storage.ManagedObjectCount, status.Storage.ObservedObjectCount,
		status.Storage.OrphanCount, status.Storage.MissingCount)
	fmt.Printf("bytes: managed=%s reserved=%s observed=%s\n",
		humanBytes(status.Storage.AccountedBytes), humanBytes(status.Storage.ReservedBytes),
		humanBytes(status.Storage.ObservedBytes))
	if status.Storage.ReconciliationError != "" {
		fmt.Printf("error: %s\n", status.Storage.ReconciliationError)
	}
	if status.Scan != nil {
		fmt.Printf("scan: %s %s attempts=%d requested=%s\n",
			status.Scan.ID, status.Scan.State, status.Scan.Attempts,
			formatAccelerationTime(status.Scan.RequestedAt))
		if status.Scan.LastError != "" {
			fmt.Printf("scan error: %s\n", status.Scan.LastError)
		}
	}
	return nil
}

func cmdAccelerationInventoryRefresh(ctx context.Context, accelerationID string) error {
	token, err := restToken(ctx)
	if err != nil {
		return err
	}
	scan, err := client.RESTRefreshAccelerationInventory(ctx, *server, token, accelerationID)
	if err != nil {
		return err
	}
	fmt.Printf("scan: %s %s\n", scan.ID, scan.State)
	return nil
}

func printAccelerationRequest(request client.AccelerationRequest) {
	fmt.Printf("track: %s\nstate: %s\n", request.TrackRef, request.State)
	if request.PendingReason != "" {
		fmt.Printf("pending: %s\n", request.PendingReason)
	}
	fmt.Printf("attempts: %d\nrequested: %s\nupdated: %s\n", request.Attempts,
		formatAccelerationTime(request.RequestedAt), formatAccelerationTime(request.UpdatedAt))
	if request.NextAttemptAt > 0 {
		fmt.Printf("next attempt: %s\n", formatAccelerationTime(request.NextAttemptAt))
	}
	if request.Lease != nil {
		fmt.Printf("lease: %s owner=%s expires=%s\n", request.Lease.ID,
			request.Lease.Owner, formatAccelerationTime(request.Lease.ExpiresAt))
	}
	if request.Progress != nil {
		fmt.Printf("progress: %s source=%s upload=%s total=%s\n", request.Progress.Phase,
			humanBytes(request.Progress.SourceBytes), humanBytes(request.Progress.UploadBytes),
			humanBytes(request.Progress.TotalBytes))
	}
	if request.LastError != "" {
		fmt.Printf("error: %s\n", request.LastError)
	}
}

func formatAccelerationTime(milliseconds int64) string {
	if milliseconds <= 0 {
		return "never"
	}
	return time.UnixMilli(milliseconds).Local().Format(time.RFC3339)
}
