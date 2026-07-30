package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrQueueRevisionMismatch = errors.New("queue revision mismatch")
	ErrQueuePartSequence     = errors.New("invalid queue part sequence")
	ErrQueuePatchInvalid     = errors.New("invalid queue patch")
)

type QueueSnapshotPart struct {
	Revision uint64       `json:"revision"`
	Part     int          `json:"part"`
	Items    []QueueEntry `json:"items"`
	Done     bool         `json:"done"`
}

type QueuePatchOp struct {
	Op      string      `json:"op"`
	Index   int         `json:"index,omitempty"`
	Item    *QueueEntry `json:"item,omitempty"`
	EntryID string      `json:"entry_id,omitempty"`
	ToIndex int         `json:"to_index,omitempty"`
}

type QueuePatchPart struct {
	BaseRevision uint64         `json:"base_revision"`
	Revision     uint64         `json:"revision"`
	Part         int            `json:"part"`
	Ops          []QueuePatchOp `json:"ops"`
	Done         bool           `json:"done"`
}

func ParseQueueSnapshot(m Message) (QueueSnapshotPart, error) {
	if m.Type != "queue.snapshot" {
		return QueueSnapshotPart{}, fmt.Errorf("expected queue.snapshot, got %q", m.Type)
	}
	var part QueueSnapshotPart
	if err := json.Unmarshal(m.Data, &part); err != nil {
		return QueueSnapshotPart{}, err
	}
	if part.Part < 0 {
		return QueueSnapshotPart{}, ErrQueuePartSequence
	}
	return part, nil
}

func ParseQueuePatch(m Message) (QueuePatchPart, error) {
	if m.Type != "queue.patch" {
		return QueuePatchPart{}, fmt.Errorf("expected queue.patch, got %q", m.Type)
	}
	var part QueuePatchPart
	if err := json.Unmarshal(m.Data, &part); err != nil {
		return QueuePatchPart{}, err
	}
	if part.Part < 0 || part.Revision != part.BaseRevision+1 {
		return QueuePatchPart{}, ErrQueuePartSequence
	}
	return part, nil
}

// QueueState is an immutable copy of the client's atomically maintained queue
// replica. ResyncRequired is set as soon as a patch cannot be safely applied.
type QueueState struct {
	Revision       uint64
	Items          []QueueEntry
	Ready          bool
	ResyncRequired bool
}

type queueSnapshotAssembly struct {
	revision uint64
	nextPart int
	items    []QueueEntry
}

type queuePatchAssembly struct {
	baseRevision uint64
	revision     uint64
	nextPart     int
	ops          []QueuePatchOp
}

type queueReplica struct {
	revision       uint64
	items          []QueueEntry
	ready          bool
	resyncRequired bool
	snapshot       *queueSnapshotAssembly
	patch          *queuePatchAssembly
}

func (c *Client) QueueState() QueueState {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	return QueueState{
		Revision:       c.queue.revision,
		Items:          cloneQueueEntries(c.queue.items),
		Ready:          c.queue.ready,
		ResyncRequired: c.queue.resyncRequired,
	}
}

// QueueSync requests a fresh chunked baseline for the currently joined room.
func (c *Client) QueueSync(ctx context.Context, roomID string) error {
	_, err := c.call(ctx, "queue.sync", map[string]any{"room_id": roomID})
	return err
}

func (c *Client) setQueueRoom(roomID string) string {
	c.qmu.Lock()
	previous := c.queueRoomID
	c.queueRoomID = roomID
	c.qmu.Unlock()
	return previous
}

func (c *Client) resetQueueRoom(roomID string) {
	c.qmu.Lock()
	c.queueRoomID = roomID
	c.queue = queueReplica{}
	c.queueResyncing = false
	c.qmu.Unlock()
}

func (c *Client) handleQueueMessage(m Message) {
	if m.Type != "queue.snapshot" && m.Type != "queue.patch" {
		return
	}
	var needsResync bool
	c.qmu.Lock()
	if c.queueRoomID == "" {
		c.qmu.Unlock()
		return
	}
	switch m.Type {
	case "queue.snapshot":
		part, err := ParseQueueSnapshot(m)
		if err != nil {
			needsResync = c.markQueueResyncLocked()
			break
		}
		if err := c.acceptQueueSnapshotLocked(part); err != nil {
			needsResync = c.markQueueResyncLocked()
		}
	case "queue.patch":
		part, err := ParseQueuePatch(m)
		if err != nil {
			needsResync = c.markQueueResyncLocked()
			break
		}
		if err := c.acceptQueuePatchLocked(part); err != nil {
			needsResync = c.markQueueResyncLocked()
		}
	}
	c.qmu.Unlock()
	if needsResync {
		c.startQueueResync()
	}
}

func (c *Client) acceptQueueSnapshotLocked(part QueueSnapshotPart) error {
	if part.Part == 0 {
		c.queue.snapshot = &queueSnapshotAssembly{revision: part.Revision}
		c.queue.patch = nil
	}
	assembly := c.queue.snapshot
	if assembly == nil || assembly.revision != part.Revision || assembly.nextPart != part.Part {
		return ErrQueuePartSequence
	}
	assembly.items = append(assembly.items, part.Items...)
	assembly.nextPart++
	if !part.Done {
		return nil
	}
	c.queue.items = cloneQueueEntries(assembly.items)
	c.queue.revision = assembly.revision
	c.queue.ready = true
	c.queue.resyncRequired = false
	c.queue.snapshot = nil
	c.queue.patch = nil
	return nil
}

func (c *Client) acceptQueuePatchLocked(part QueuePatchPart) error {
	if part.Part == 0 {
		c.queue.patch = &queuePatchAssembly{
			baseRevision: part.BaseRevision,
			revision:     part.Revision,
		}
	}
	assembly := c.queue.patch
	if assembly == nil || assembly.baseRevision != part.BaseRevision ||
		assembly.revision != part.Revision || assembly.nextPart != part.Part {
		return ErrQueuePartSequence
	}
	assembly.ops = append(assembly.ops, part.Ops...)
	assembly.nextPart++
	if !part.Done {
		return nil
	}
	c.queue.patch = nil
	if !c.queue.ready || c.queue.revision != assembly.baseRevision {
		return fmt.Errorf("%w: have %d, patch base %d", ErrQueueRevisionMismatch, c.queue.revision, assembly.baseRevision)
	}
	next, err := applyQueuePatch(c.queue.items, assembly.ops)
	if err != nil {
		return err
	}
	c.queue.items = next
	c.queue.revision = assembly.revision
	c.queue.resyncRequired = false
	return nil
}

func (c *Client) markQueueResyncLocked() bool {
	c.queue.resyncRequired = true
	c.queue.snapshot = nil
	c.queue.patch = nil
	return c.queueRoomID != "" && !c.queueResyncing
}

func (c *Client) startQueueResync() {
	c.qmu.Lock()
	if c.queueRoomID == "" || c.queueResyncing || !c.queue.resyncRequired {
		c.qmu.Unlock()
		return
	}
	roomID := c.queueRoomID
	c.queueResyncing = true
	c.qmu.Unlock()

	// Never wait for a response in readPump: call runs in its own goroutine so
	// readPump remains available to receive both snapshot parts and the ack.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := c.QueueSync(ctx, roomID)
		cancel()
		c.qmu.Lock()
		c.queueResyncing = false
		if err != nil {
			c.queue.resyncRequired = true
		}
		c.qmu.Unlock()
	}()
}

func applyQueuePatch(current []QueueEntry, ops []QueuePatchOp) ([]QueueEntry, error) {
	next := cloneQueueEntries(current)
	for _, op := range ops {
		switch op.Op {
		case "add":
			if op.Item == nil || op.Index < 0 || op.Index > len(next) {
				return nil, ErrQueuePatchInvalid
			}
			if queueEntryIndex(next, op.Item.EntryID) >= 0 {
				return nil, ErrQueuePatchInvalid
			}
			item := cloneQueueEntry(*op.Item)
			next = append(next, QueueEntry{})
			copy(next[op.Index+1:], next[op.Index:])
			next[op.Index] = item
		case "remove":
			idx := queueEntryIndex(next, op.EntryID)
			if idx < 0 {
				return nil, ErrQueuePatchInvalid
			}
			copy(next[idx:], next[idx+1:])
			next = next[:len(next)-1]
		case "move":
			idx := queueEntryIndex(next, op.EntryID)
			if idx < 0 || op.ToIndex < 0 || op.ToIndex >= len(next) {
				return nil, ErrQueuePatchInvalid
			}
			item := next[idx]
			if idx < op.ToIndex {
				copy(next[idx:op.ToIndex], next[idx+1:op.ToIndex+1])
			} else if idx > op.ToIndex {
				copy(next[op.ToIndex+1:idx+1], next[op.ToIndex:idx])
			}
			next[op.ToIndex] = item
		case "clear":
			next = []QueueEntry{}
		default:
			return nil, ErrQueuePatchInvalid
		}
	}
	return next, nil
}

func queueEntryIndex(items []QueueEntry, entryID string) int {
	if entryID == "" {
		return -1
	}
	for i := range items {
		if items[i].EntryID == entryID {
			return i
		}
	}
	return -1
}

func cloneQueueEntry(entry QueueEntry) QueueEntry {
	entry.Contributors = append([]Contributor(nil), entry.Contributors...)
	return entry
}

func cloneQueueEntries(items []QueueEntry) []QueueEntry {
	cloned := make([]QueueEntry, len(items))
	for i, entry := range items {
		cloned[i] = cloneQueueEntry(entry)
	}
	return cloned
}

type queueSnapshotCollector struct {
	assembly *queueSnapshotAssembly
}

func (c *queueSnapshotCollector) add(part QueueSnapshotPart) ([]QueueEntry, uint64, bool, error) {
	if part.Part == 0 {
		c.assembly = &queueSnapshotAssembly{revision: part.Revision}
	}
	if c.assembly == nil || c.assembly.revision != part.Revision || c.assembly.nextPart != part.Part {
		return nil, 0, false, ErrQueuePartSequence
	}
	c.assembly.items = append(c.assembly.items, part.Items...)
	c.assembly.nextPart++
	if !part.Done {
		return nil, 0, false, nil
	}
	items := cloneQueueEntries(c.assembly.items)
	revision := c.assembly.revision
	c.assembly = nil
	return items, revision, true, nil
}
