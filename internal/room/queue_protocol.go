package room

import (
	"encoding/json"
	"errors"
	"fmt"
)

// QueueEnvelopeBudget is the target upper bound for every server-to-client
// queue.snapshot and queue.patch JSON envelope. It remains below coder/websocket's
// default 32768-byte read limit, including the complete type/data envelope.
const QueueEnvelopeBudget = 24 * 1024

var ErrQueueEnvelopeTooLarge = errors.New("queue entry exceeds websocket envelope budget")

type QueuePatchOpKind string

const (
	QueueOpAdd    QueuePatchOpKind = "add"
	QueueOpRemove QueuePatchOpKind = "remove"
	QueueOpMove   QueuePatchOpKind = "move"
	QueueOpClear  QueuePatchOpKind = "clear"
)

// QueuePatchOp is one ordered operation in a logical queue mutation. Add uses
// Index and Item, remove uses EntryID, move uses EntryID and ToIndex, and clear
// carries no additional fields.
type QueuePatchOp struct {
	Op      QueuePatchOpKind `json:"op"`
	Index   int              `json:"index,omitempty"`
	Item    *QueueEntry      `json:"item,omitempty"`
	EntryID string           `json:"entry_id,omitempty"`
	ToIndex int              `json:"to_index,omitempty"`
}

type QueueSnapshotData struct {
	Revision uint64       `json:"revision"`
	Part     int          `json:"part"`
	Items    []QueueEntry `json:"items"`
	Done     bool         `json:"done"`
}

type QueueSnapshotMessage struct {
	Type string            `json:"type"`
	Data QueueSnapshotData `json:"data"`
}

type QueuePatchData struct {
	BaseRevision uint64         `json:"base_revision"`
	Revision     uint64         `json:"revision"`
	Part         int            `json:"part"`
	Ops          []QueuePatchOp `json:"ops"`
	Done         bool           `json:"done"`
}

type QueuePatchMessage struct {
	Type string         `json:"type"`
	Data QueuePatchData `json:"data"`
}

// QueueSnapshotMessages splits items using the actual encoded envelope size.
// Part numbers are zero-based and the final part alone has done=true.
func QueueSnapshotMessages(revision uint64, items []QueueEntry) ([]QueueSnapshotMessage, error) {
	message := func(part int, chunk []QueueEntry, done bool) QueueSnapshotMessage {
		return QueueSnapshotMessage{
			Type: "queue.snapshot",
			Data: QueueSnapshotData{Revision: revision, Part: part, Items: chunk, Done: done},
		}
	}
	if len(items) == 0 {
		m := message(0, []QueueEntry{}, true)
		if err := queueEnvelopeWithinBudget(m); err != nil {
			return nil, err
		}
		return []QueueSnapshotMessage{m}, nil
	}

	messages := make([]QueueSnapshotMessage, 0, 1)
	for start, part := 0, 0; start < len(items); part++ {
		end := start
		for end < len(items) {
			candidateEnd := end + 1
			candidate := message(part, items[start:candidateEnd], candidateEnd == len(items))
			if err := queueEnvelopeWithinBudget(candidate); err != nil {
				if end == start {
					return nil, fmt.Errorf("%w: snapshot item %d", ErrQueueEnvelopeTooLarge, start)
				}
				break
			}
			end = candidateEnd
		}
		messages = append(messages, message(part, items[start:end], end == len(items)))
		start = end
	}
	return messages, nil
}

// QueuePatchMessages splits a logical mutation without changing its revision
// boundary. A client must collect all parts before applying any operation.
func QueuePatchMessages(baseRevision, revision uint64, ops []QueuePatchOp) ([]QueuePatchMessage, error) {
	message := func(part int, chunk []QueuePatchOp, done bool) QueuePatchMessage {
		return QueuePatchMessage{
			Type: "queue.patch",
			Data: QueuePatchData{
				BaseRevision: baseRevision,
				Revision:     revision,
				Part:         part,
				Ops:          chunk,
				Done:         done,
			},
		}
	}
	if len(ops) == 0 {
		m := message(0, []QueuePatchOp{}, true)
		if err := queueEnvelopeWithinBudget(m); err != nil {
			return nil, err
		}
		return []QueuePatchMessage{m}, nil
	}

	messages := make([]QueuePatchMessage, 0, 1)
	for start, part := 0, 0; start < len(ops); part++ {
		end := start
		for end < len(ops) {
			candidateEnd := end + 1
			candidate := message(part, ops[start:candidateEnd], candidateEnd == len(ops))
			if err := queueEnvelopeWithinBudget(candidate); err != nil {
				if end == start {
					return nil, fmt.Errorf("%w: patch operation %d", ErrQueueEnvelopeTooLarge, start)
				}
				break
			}
			end = candidateEnd
		}
		messages = append(messages, message(part, ops[start:end], end == len(ops)))
		start = end
	}
	return messages, nil
}

func queueEnvelopeWithinBudget(message any) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(encoded) > QueueEnvelopeBudget {
		return ErrQueueEnvelopeTooLarge
	}
	return nil
}
