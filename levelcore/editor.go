package levelcore

import "sort"

// Default tuning parameters.
const (
	// DefaultHistoryLimit bounds retained undo entries.
	DefaultHistoryLimit = 512
	// DefaultMergeWindow is the logical-commit gap within which
	// consecutive compatible micro-edits merge into one entry.
	DefaultMergeWindow = 8
	// DefaultRegionCacheSize bounds the LRU of region member lists.
	DefaultRegionCacheSize = 1024
)

// Options configures an Editor.
type Options struct {
	HistoryLimit    int
	MergeWindow     int
	RegionCacheSize int
}

func (o Options) withDefaults() Options {
	if o.HistoryLimit == 0 {
		o.HistoryLimit = DefaultHistoryLimit
	}
	if o.MergeWindow == 0 {
		o.MergeWindow = DefaultMergeWindow
	}
	if o.RegionCacheSize == 0 {
		o.RegionCacheSize = DefaultRegionCacheSize
	}
	return o
}

// Editor is the in-process editing kernel around one document. It is
// not safe for concurrent use; wrap it with external synchronization
// if required.
type Editor struct {
	doc      *Doc
	hist     *history
	arts     *artifacts
	clock    int
	mergeWin int
}

// NewEditor wraps an initial document (which is cloned defensively),
// verifies its structural consistency and builds the initial artifact.
func NewEditor(d *Doc, opts Options) (*Editor, error) {
	if d == nil {
		return nil, newError(KindInvalidArgument, "NewEditor", "nil document", -1)
	}
	if err := validateInitialDoc(d); err != nil {
		return nil, err
	}
	o := opts.withDefaults()
	doc := d.Clone()
	return &Editor{
		doc:      doc,
		hist:     newHistory(o.HistoryLimit),
		arts:     newArtifacts(doc, o.RegionCacheSize),
		mergeWin: o.MergeWindow,
	}, nil
}

// Doc returns a defensive copy of the current document.
func (e *Editor) Doc() *Doc { return e.doc.Clone() }

// Commit applies one or more commands as a single atomic history
// entry. Every command is validated before mutation; if any command
// fails validation or application, all earlier commands of the batch
// are rolled back in reverse and the document and artifacts are
// byte-identical to the pre-commit state. The returned *Error locates
// the failing command by its index in the argument list.
func (e *Editor) Commit(cmds ...Command) error {
	if len(cmds) == 0 {
		return newError(KindInvalidArgument, "Commit", "no commands", -1)
	}
	for i, c := range cmds {
		if c == nil {
			return newError(KindInvalidArgument, "Commit", "nil command", i)
		}
	}

	// A single argument that is itself a Composite is stored as-is;
	// otherwise the arguments form one atomic composite entry.
	var entry Command
	var subs []Command
	if len(cmds) == 1 {
		if comp, ok := cmds[0].(*Composite); ok {
			entry = comp
			subs = comp.Commands
		} else {
			entry = cmds[0]
			subs = cmds
		}
	} else {
		entry = &Composite{Commands: cmds}
		subs = cmds
	}
	if len(subs) == 0 {
		return newError(KindInvalidArgument, "Commit", "empty composite", -1)
	}

	applied := make([]Command, 0, len(subs))
	rollback := func(cause *Error) *Error {
		for j := len(applied) - 1; j >= 0; j-- {
			c := applied[j]
			t := c.touches(e.doc)
			e.arts.prepare(t)
			if err := c.undo(e.doc); err != nil {
				// undo of a previously applied command cannot fail for
				// a primed command; surface as an internal invariant.
				return newError(KindUnknown, "Commit.rollback", err.Error(), cause.CommandIndex)
			}
			e.arts.commit()
		}
		return cause
	}

	for i, sub := range subs {
		if err := validate(e.doc, e.arts, sub, i); err != nil {
			return rollback(asKernelError(err, i))
		}
		t := sub.touches(e.doc)
		e.arts.prepare(t)
		if err := sub.apply(e.doc); err != nil {
			return rollback(asKernelError(err, i))
		}
		e.arts.commit()
		applied = append(applied, sub)
	}

	e.clock++
	e.hist.push(entry, e.clock, e.mergeWin)
	return nil
}

// Undo reverses the most recent committed (possibly merged or
// composite) entry, moving it onto the redo stack.
func (e *Editor) Undo() bool {
	entry := e.hist.undo()
	if entry == nil {
		return false
	}
	if !e.applyHistoryEntry(entry, false) {
		// an undo of a previously committed command is invariant; a
		// failure here signals kernel corruption, not user input.
		e.hist.redo()
		return false
	}
	e.clock--
	return true
}

// Redo reapplies the most recently undone entry.
func (e *Editor) Redo() bool {
	entry := e.hist.redo()
	if entry == nil {
		return false
	}
	if !e.applyHistoryEntry(entry, true) {
		e.hist.undo()
		return false
	}
	e.clock++
	return true
}

// applyHistoryEntry runs one history command in apply (redo) or undo
// direction while keeping the artifact in sync.
func (e *Editor) applyHistoryEntry(entry *Entry, forward bool) bool {
	cmd := entry.Command
	t := cmd.touches(e.doc)
	e.arts.prepare(t)
	var err error
	if forward {
		err = cmd.apply(e.doc)
	} else {
		err = cmd.undo(e.doc)
	}
	if err != nil {
		return false
	}
	e.arts.commit()
	return true
}

func (e *Editor) CanUndo() bool { return e.hist.canUndo() }
func (e *Editor) CanRedo() bool { return e.hist.canRedo() }

// Artifact returns a deep copy of the current derived artifact.
func (e *Editor) Artifact() Artifact { return e.arts.snapshot() }

// History returns a copy of committed history entries (oldest first).
func (e *Editor) History() []Entry { return e.hist.Entries() }

// Clock returns the current logical commit counter.
func (e *Editor) Clock() int { return e.clock }

func asKernelError(err error, idx int) *Error {
	if ke, ok := err.(*Error); ok {
		if ke.CommandIndex < 0 {
			ke.CommandIndex = idx
		}
		return ke
	}
	return newError(KindUnknown, "Commit", err.Error(), idx)
}

// validateInitialDoc checks structural validity of a freshly supplied
// document.
func validateInitialDoc(d *Doc) error {
	const op = "NewEditor"
	if d.Width <= 0 || d.Height <= 0 {
		return newError(KindInvalidArgument, op, "width and height must be positive", -1)
	}
	for id, l := range d.Layers {
		if id == "" {
			return newError(KindInvalidArgument, op, "empty layer id", -1)
		}
		if len(l.Tiles) != d.Width*d.Height {
			return newError(KindInvalidArgument, op,
				"layer "+id+" tile count does not match dimensions", -1)
		}
	}
	for id, e := range d.Entities {
		if e.ID == "" || e.ID != id {
			return newError(KindInvalidArgument, op, "entity map key mismatch", -1)
		}
		if e.Type == "" {
			return newError(KindInvalidArgument, op, "entity "+id+" has empty type", -1)
		}
		if !d.rectInBounds(rectOf(e)) {
			return newError(KindOutOfBounds, op, "entity "+id+" out of bounds", -1)
		}
		if e.Region != "" {
			if _, ok := d.Regions[e.Region]; !ok {
				return newError(KindBadReference, op,
					"entity "+id+" owner region missing: "+e.Region, -1)
			}
		}
	}
	// overlap check via a spatial hash grid: O(n + local density),
	// so importing very large documents stays linear.
	const grid = 32
	cells := map[int64][]string{}
	putCell := func(cx, cy int64) int64 { return cx<<32 | (cy & 0xffffffff) }
	ids := make([]string, 0, len(d.Entities))
	for id := range d.Entities {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := d.Entities[id]
		r := rectOf(e)
		x0, y0 := int64(r.X/grid), int64(r.Y/grid)
		x1, y1 := int64((r.X+r.Width-1)/grid), int64((r.Y+r.Height-1)/grid)
		for cx := x0; cx <= x1; cx++ {
			for cy := y0; cy <= y1; cy++ {
				k := putCell(cx, cy)
				for _, other := range cells[k] {
					if rectsOverlap(r, rectOf(d.Entities[other])) {
						return newError(KindOverlap, op,
							"initial entities overlap: "+id+" / "+other, -1)
					}
				}
				cells[k] = append(cells[k], id)
			}
		}
	}
	for id, r := range d.Regions {
		if r.ID == "" || r.ID != id {
			return newError(KindInvalidArgument, op, "region map key mismatch", -1)
		}
		if !d.rectInBounds(r.Area) {
			return newError(KindOutOfBounds, op, "region "+id+" out of bounds", -1)
		}
	}
	return nil
}
