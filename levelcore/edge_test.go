package levelcore

import "testing"

func TestSetTilesDuplicateCellsLastWins(t *testing.T) {
	e := newTestEditor(t, 4, 4)
	err := e.Commit(&SetTiles{Layer: "g", Cells: []Cell{
		{X: 0, Y: 0, Terrain: "a"},
		{X: 0, Y: 0, Terrain: "b"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if e.Doc().Layers["g"].Tiles[0].Terrain != "b" {
		t.Fatal("last write must win within one batch")
	}
	e.Undo()
	if _, ok := e.Doc().Layers["g"]; ok {
		t.Fatal("born layer must disappear on undo")
	}
	f, _ := NewEditor(e.Doc(), Options{})
	if e.Artifact().Digest != f.Artifact().Digest {
		t.Fatal("undo digest mismatch")
	}
}

func TestSetTilesMergeAcrossCommits(t *testing.T) {
	e := newTestEditor(t, 4, 4)
	e.Commit(&SetTiles{Layer: "g", Cells: []Cell{{0, 0, "a"}, {1, 1, "b"}}})
	e.Commit(&SetTiles{Layer: "g", Cells: []Cell{{2, 2, "c"}, {0, 0, "d"}}})
	h := e.History()
	if len(h) != 1 || h[0].MergeCount != 2 {
		t.Fatalf("expected merged SetTiles: %+v", h)
	}
	l := e.Doc().Layers["g"]
	if l.Tiles[0].Terrain != "d" || l.Tiles[5].Terrain != "b" || l.Tiles[10].Terrain != "c" {
		t.Fatal("merged SetTiles content wrong")
	}
	e.Undo()
	if _, ok := e.Doc().Layers["g"]; ok {
		t.Fatal("merged SetTiles undo must drop born layer")
	}
	e.Redo()
	l = e.Doc().Layers["g"]
	if l.Tiles[0].Terrain != "d" || l.Tiles[5].Terrain != "b" || l.Tiles[10].Terrain != "c" {
		t.Fatal("merged SetTiles redo wrong")
	}
}

func TestNewEditorRejectsInvalidDocuments(t *testing.T) {
	cases := []struct {
		name string
		doc  *Doc
		kind Kind
	}{
		{"nil", nil, KindInvalidArgument},
		{"zero size", &Doc{Width: 0, Height: 0, Layers: map[string]Layer{}, Entities: map[string]Entity{}, Regions: map[string]Region{}}, KindInvalidArgument},
		{"bad tile count", func() *Doc {
			d, _ := NewDoc(4, 4)
			d.Layers["g"] = Layer{Tiles: make([]Tile, 3)}
			return d
		}(), KindInvalidArgument},
		{"entity bad owner", func() *Doc {
			d, _ := NewDoc(4, 4)
			d.Entities["a"] = Entity{ID: "a", Type: "t", X: 0, Y: 0, Width: 1, Height: 1, Region: "x"}
			return d
		}(), KindBadReference},
		{"initial overlap", func() *Doc {
			d, _ := NewDoc(4, 4)
			d.Entities["a"] = Entity{ID: "a", Type: "t", X: 0, Y: 0, Width: 2, Height: 2}
			d.Entities["b"] = Entity{ID: "b", Type: "t", X: 1, Y: 1, Width: 2, Height: 2}
			return d
		}(), KindOverlap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewEditor(tc.doc, Options{})
			if !IsKind(err, tc.kind) {
				t.Fatalf("want %s, got %v", tc.kind, err)
			}
		})
	}
}

func TestNewDocDefensiveClone(t *testing.T) {
	d, _ := NewDoc(4, 4)
	e, _ := NewEditor(d, Options{})
	d.Width = 99 // mutate the caller's doc
	if got := e.Doc().Width; got != 4 {
		t.Fatalf("editor aliased caller doc: %d", got)
	}
}

func TestMergeThenNewEditTruncatesRedo(t *testing.T) {
	e := newTestEditor(t, 4, 4)
	e.Commit(mk2("g", 0, 0, "a"))
	e.Commit(mk2("g", 0, 0, "b")) // merged
	e.Undo()
	if !e.CanRedo() {
		t.Fatal("redo should be available after undo")
	}
	e.Commit(mk2("g", 2, 2, "c"))
	if e.CanRedo() {
		t.Fatal("redo must be truncated")
	}
	// The merged entry lived on the redo stack and was truncated; the
	// new, different-key edit is the sole remaining entry.
	if len(e.History()) != 1 || e.History()[0].Kind != "set_tile" {
		t.Fatalf("history=%v", e.History())
	}
	if e.Doc().Layers["g"].Tiles[10].Terrain != "c" {
		t.Fatal("new edit not present")
	}
}

func TestRegionMembershipIncremental(t *testing.T) {
	e := newTestEditor(t, 12, 12)
	e.Commit(&RegionAdd{Region: Region{ID: "r", Area: Rect{0, 0, 5, 5}}})
	e.Commit(&EntityAdd{Entity: Entity{ID: "in", Type: "t", X: 1, Y: 1, Width: 1, Height: 1, Region: "r"}})
	e.Commit(&EntityAdd{Entity: Entity{ID: "out", Type: "t", X: 10, Y: 10, Width: 1, Height: 1}})
	if got := e.Artifact().RegionIndex["r"]; len(got) != 1 || got[0] != "in" {
		t.Fatalf("members=%v", got)
	}
	// move "out" into the region
	e.Commit(&EntityMove{ID: "out", To: Rect{X: 2, Y: 2, Width: 1, Height: 1}})
	if got := e.Artifact().RegionIndex["r"]; len(got) != 2 {
		t.Fatalf("members after move=%v", got)
	}
	// grow region to cover nothing new, shrink it to exclude (1,1)
	e.Commit(&RegionUpdate{ID: "r", Next: Region{ID: "r", Area: Rect{2, 2, 3, 3}}})
	if got := e.Artifact().RegionIndex["r"]; len(got) != 1 || got[0] != "out" {
		t.Fatalf("members after resize=%v", got)
	}
	f, _ := NewEditor(e.Doc(), Options{})
	if e.Artifact().Digest != f.Artifact().Digest {
		t.Fatal("membership incremental mismatch")
	}
}

func TestFailedCommitKeepsRedoStack(t *testing.T) {
	e := newTestEditor(t, 6, 6)
	e.Commit(&SetTile{Layer: "g", X: 0, Y: 0, Terrain: "a"})
	e.Undo()
	// failed commit must not truncate the redo stack nor advance clock
	beforeHist := len(e.History())
	err := e.Commit(&SetTile{Layer: "g", X: 99, Y: 0, Terrain: "b"})
	if !IsKind(err, KindOutOfBounds) {
		t.Fatalf("want oob, got %v", err)
	}
	if !e.CanRedo() {
		t.Fatal("failed commit must not clear redo stack")
	}
	if len(e.History()) != beforeHist {
		t.Fatal("failed commit altered history")
	}
	if !e.Redo() {
		t.Fatal("redo unavailable after failed commit")
	}
	if e.Doc().Layers["g"].Tiles[0].Terrain != "a" {
		t.Fatal("redo restored wrong state")
	}
}
