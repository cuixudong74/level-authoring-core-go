package levelcore

import (
	"reflect"
	"testing"
)

// TestHistoryIdenticalForSameScript verifies requirement 1 directly:
// identical initial doc + identical command stream must produce
// identical history records (seq/clock/merge counts/kinds) on two
// independent editors.
func TestHistoryIdenticalForSameScript(t *testing.T) {
	mk := func() *Editor {
		d, _ := NewDoc(10, 10)
		e, err := NewEditor(d, Options{})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	script := func(e *Editor) {
		e.Commit(&SetTile{Layer: "g", X: 0, Y: 0, Terrain: "a"})
		e.Commit(&SetTile{Layer: "g", X: 0, Y: 0, Terrain: "b"}) // merges
		e.Commit(&RegionAdd{Region: Region{ID: "r", Area: Rect{0, 0, 4, 4}}})
		e.Commit(
			&EntityAdd{Entity: Entity{ID: "e", Type: "t", X: 1, Y: 1, Width: 1, Height: 1, Region: "r"}},
			&SetTile{Layer: "g", X: 2, Y: 2, Terrain: "c"},
		) // one composite
		e.Undo()
	}
	a, b := mk(), mk()
	script(a)
	script(b)
	ha, hb := a.History(), b.History()
	if len(ha) != len(hb) {
		t.Fatalf("history length %d != %d", len(ha), len(hb))
	}
	for i := range ha {
		if ha[i].Seq != hb[i].Seq || ha[i].Clock != hb[i].Clock ||
			ha[i].MergeCount != hb[i].MergeCount || ha[i].Kind != hb[i].Kind {
			t.Fatalf("entry %d differs: %+v vs %+v", i, ha[i], hb[i])
		}
	}
	if !reflect.DeepEqual(a.Doc(), b.Doc()) ||
		!reflect.DeepEqual(a.Artifact(), b.Artifact()) {
		t.Fatal("doc/artifact differ for identical scripts")
	}
}

// TestImportedEditorUndoRedoStability walks undo/redo to both ends on
// an imported editor several times and verifies artifacts each time.
func TestImportedEditorUndoRedoStability(t *testing.T) {
	e, _ := NewEditor(mustDoc(20, 20), Options{HistoryLimit: 3})
	for i := 0; i < 10; i++ {
		if err := e.Commit(&EntityAdd{Entity: Entity{
			ID: "e" + itoa(i), Type: "t",
			X: (i * 2) % 18, Y: (i * 2) / 18, Width: 1, Height: 1,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := MarshalBundle(e)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		got, err := UnmarshalBundle(data)
		if err != nil {
			t.Fatal(err)
		}
		tip := got.Artifact().Digest
		for got.Undo() {
			f, _ := NewEditor(got.Doc(), Options{})
			if got.Artifact().Digest != f.Artifact().Digest {
				t.Fatal("artifact diverged on imported undo")
			}
		}
		for got.Redo() {
			f, _ := NewEditor(got.Doc(), Options{})
			if got.Artifact().Digest != f.Artifact().Digest {
				t.Fatal("artifact diverged on imported redo")
			}
		}
		if got.Artifact().Digest != tip {
			t.Fatal("re-walking history did not return to the tip state")
		}
		if !reflect.DeepEqual(got.Doc(), e.Doc()) {
			t.Fatal("tip doc changed after undo/redo walk")
		}
	}
}

// TestBatchErrorIndexUnderComposite checks index attribution when a
// pre-wrapped Composite is committed directly.
func TestBatchErrorIndexUnderComposite(t *testing.T) {
	e := newTestEditor(t, 8, 8)
	err := e.Commit(&Composite{Commands: []Command{
		&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 0, Y: 0, Width: 1, Height: 1}},
		&EntityAdd{Entity: Entity{ID: "b", Type: "t", X: 5, Y: 5, Width: 1, Height: 1}},
		&EntityMove{ID: "ghost", To: Rect{X: 0, Y: 0, Width: 1, Height: 1}},
	}})
	if !IsKind(err, KindNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if ke := err.(*Error); ke.CommandIndex != 2 {
		t.Fatalf("index=%d", ke.CommandIndex)
	}
	if len(e.Doc().Entities) != 0 {
		t.Fatal("rolled-back composite left entities")
	}
}
