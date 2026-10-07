package levelcore

// Entry is one committed history record: the applied (possibly merged)
// command plus bookkeeping for explainability.
type Entry struct {
	Seq        int     `json:"seq"`
	Kind       string  `json:"kind"`
	MergeCount int     `json:"merge_count"`
	Clock      int     `json:"clock"`
	Command    Command `json:"-"`
}

const defaultHistoryLimit = 256

// history is the bounded undo/redo stack.
//
// Push semantics:
//   - any push truncates the redo stack first (standard undo/redo);
//   - if the new command is compatible with the tip of the undo stack
//     (same merge key, clock gap <= mergeWindow and seq gap <= 1), the
//     two are merged into one entry whose MergeCount grows;
//   - when len(done) exceeds the cap the OLDEST entries are evicted
//     (most recent edits always remain undoable).
type history struct {
	done    []*Entry
	undone  []*Entry
	max     int
	nextSeq int
}

func newHistory(max int) *history {
	if max <= 0 {
		max = defaultHistoryLimit
	}
	return &history{max: max, nextSeq: 1}
}

// push records cmd committed at logical clock value clock. mergeWindow
// of 0 disables merging. Returns the stored entry.
func (h *history) push(cmd Command, clock, mergeWindow int) *Entry {
	// New edits always truncate the redo history.
	h.undone = h.undone[:0]

	if mergeWindow > 0 && len(h.done) > 0 {
		tip := h.done[len(h.done)-1]
		if clock-tip.Clock <= mergeWindow && tip.Seq == h.nextSeq-1 {
			if merged, ok := tip.Command.mergeWith(cmd); ok {
				tip.Command = merged
				tip.MergeCount++
				tip.Clock = clock
				return tip
			}
		}
	}

	entry := &Entry{
		Seq:        h.nextSeq,
		Kind:       cmd.name(),
		MergeCount: 1,
		Clock:      clock,
		Command:    cmd,
	}
	h.nextSeq++
	h.done = append(h.done, entry)
	if len(h.done) > h.max {
		// Evict oldest records; seq gaps are preserved so later
		// entries keep their stable identity.
		extra := len(h.done) - h.max
		h.done = h.done[extra:]
	}
	return entry
}

func (h *history) canUndo() bool { return len(h.done) > 0 }
func (h *history) canRedo() bool { return len(h.undone) > 0 }

func (h *history) undo() *Entry {
	if len(h.done) == 0 {
		return nil
	}
	e := h.done[len(h.done)-1]
	h.done = h.done[:len(h.done)-1]
	h.undone = append(h.undone, e)
	return e
}

func (h *history) redo() *Entry {
	if len(h.undone) == 0 {
		return nil
	}
	e := h.undone[len(h.undone)-1]
	h.undone = h.undone[:len(h.undone)-1]
	h.done = append(h.done, e)
	return e
}

// Entries returns a defensive copy of committed (non-undone) entries.
func (h *history) Entries() []Entry {
	out := make([]Entry, 0, len(h.done))
	for _, e := range h.done {
		out = append(out, *e)
	}
	return out
}
