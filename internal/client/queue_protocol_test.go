package client

import (
	"encoding/json"
	"errors"
	"testing"
)

func queueProtocolMessage(t *testing.T, typ string, data any) Message {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return Message{Type: typ, Data: encoded}
}

func TestQueueReplicaAppliesChunkedMessagesAtomically(t *testing.T) {
	client := &Client{queueRoomID: "room-a"}
	first := QueueEntry{EntryID: "first", TrackRef: "local:first"}
	second := QueueEntry{EntryID: "second", TrackRef: "local:second"}
	third := QueueEntry{EntryID: "third", TrackRef: "local:third"}

	client.handleQueueMessage(queueProtocolMessage(t, "queue.snapshot", QueueSnapshotPart{
		Revision: 10,
		Part:     0,
		Items:    []QueueEntry{first},
		Done:     false,
	}))
	state := client.QueueState()
	if state.Ready || state.Revision != 0 || len(state.Items) != 0 {
		t.Fatalf("partial snapshot became visible: %#v", state)
	}
	client.handleQueueMessage(queueProtocolMessage(t, "queue.snapshot", QueueSnapshotPart{
		Revision: 10,
		Part:     1,
		Items:    []QueueEntry{second},
		Done:     true,
	}))
	state = client.QueueState()
	if !state.Ready || state.Revision != 10 || len(state.Items) != 2 ||
		state.Items[0].EntryID != first.EntryID || state.Items[1].EntryID != second.EntryID {
		t.Fatalf("assembled snapshot state = %#v", state)
	}

	client.handleQueueMessage(queueProtocolMessage(t, "queue.patch", QueuePatchPart{
		BaseRevision: 10,
		Revision:     11,
		Part:         0,
		Ops: []QueuePatchOp{{
			Op: "add", Index: 2, Item: &third,
		}},
		Done: false,
	}))
	state = client.QueueState()
	if state.Revision != 10 || len(state.Items) != 2 {
		t.Fatalf("partial patch became visible: %#v", state)
	}
	client.handleQueueMessage(queueProtocolMessage(t, "queue.patch", QueuePatchPart{
		BaseRevision: 10,
		Revision:     11,
		Part:         1,
		Ops: []QueuePatchOp{{
			Op: "remove", EntryID: first.EntryID,
		}},
		Done: true,
	}))
	state = client.QueueState()
	if state.Revision != 11 || len(state.Items) != 2 ||
		state.Items[0].EntryID != second.EntryID || state.Items[1].EntryID != third.EntryID {
		t.Fatalf("atomic patch state = %#v", state)
	}
}

func TestQueueRevisionMismatchKeepsStateAndRequiresResync(t *testing.T) {
	client := &Client{
		queueRoomID:    "room-a",
		queueResyncing: true,
		queue: queueReplica{
			revision: 7,
			items:    []QueueEntry{{EntryID: "kept", TrackRef: "local:kept"}},
			ready:    true,
		},
	}
	client.handleQueueMessage(queueProtocolMessage(t, "queue.patch", QueuePatchPart{
		BaseRevision: 5,
		Revision:     6,
		Part:         0,
		Ops:          []QueuePatchOp{{Op: "clear"}},
		Done:         true,
	}))
	state := client.QueueState()
	if !state.ResyncRequired || state.Revision != 7 || len(state.Items) != 1 ||
		state.Items[0].EntryID != "kept" {
		t.Fatalf("mismatched patch changed or trusted stale state: %#v", state)
	}

	client.qmu.Lock()
	client.queueResyncing = false
	shouldStart := client.markQueueResyncLocked()
	client.qmu.Unlock()
	if !shouldStart {
		t.Fatal("revision mismatch did not request queue.sync resynchronization")
	}

	client.qmu.Lock()
	err := client.acceptQueuePatchLocked(QueuePatchPart{
		BaseRevision: 5,
		Revision:     6,
		Part:         0,
		Ops:          []QueuePatchOp{{Op: "clear"}},
		Done:         true,
	})
	client.qmu.Unlock()
	if !errors.Is(err, ErrQueueRevisionMismatch) {
		t.Fatalf("revision mismatch error = %v, want %v", err, ErrQueueRevisionMismatch)
	}
}
