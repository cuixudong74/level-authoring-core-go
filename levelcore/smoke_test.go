package levelcore

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSmokeCore(t *testing.T) {
	d, _ := NewDoc(10, 10)
	e, err := NewEditor(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st := &SetTile{Layer: "terrain", X: 1, Y: 1, Terrain: "grass"}
	if err := e.Commit(st); err != nil {
		t.Fatal(err)
	}
	// merge: second set on same cell within window
	if err := e.Commit(&SetTile{Layer: "terrain", X: 1, Y: 1, Terrain: "water"}); err != nil {
		t.Fatal(err)
	}
	h := e.History()
	if len(h) != 1 || h[0].MergeCount != 2 {
		t.Fatalf("merge: %+v", h)
	}
	if got := e.Doc().Layers["terrain"].Tiles[11].Terrain; got != "water" {
		t.Fatalf("tile=%s", got)
	}
	// undo merged entry restores original
	if !e.Undo() {
		t.Fatal("undo")
	}
	if l, ok := e.Doc().Layers["terrain"]; ok {
		if got := l.Tiles[11].Terrain; got != "" {
			t.Fatalf("after undo tile=%q", got)
		}
	}
	if e.CanRedo() != true || !e.Redo() {
		t.Fatal("redo")
	}
	if got := e.Doc().Layers["terrain"].Tiles[11].Terrain; got != "water" {
		t.Fatalf("after redo tile=%q", got)
	}

	// entities + overlap rejection
	if err := e.Commit(&EntityAdd{Entity: Entity{ID: "a", Type: "tree", X: 0, Y: 0, Width: 2, Height: 2}}); err != nil {
		t.Fatal(err)
	}
	err = e.Commit(&EntityAdd{Entity: Entity{ID: "b", Type: "rock", X: 1, Y: 1, Width: 2, Height: 2}})
	if !IsKind(err, KindOverlap) {
		t.Fatalf("want overlap, got %v", err)
	}
	if ke := err.(*Error); ke.CommandIndex != 0 {
		t.Fatalf("idx=%d", ke.CommandIndex)
	}
	if _, ok := e.Doc().Entities["b"]; ok {
		t.Fatal("rejected edit left state")
	}

	// batch atomicity: second command bad
	err = e.Commit(
		&EntityAdd{Entity: Entity{ID: "c", Type: "rock", X: 5, Y: 5, Width: 1, Height: 1}},
		&EntityAdd{Entity: Entity{ID: "d", Type: "rock", X: 0, Y: 0, Width: 1, Height: 1}},
	)
	if !IsKind(err, KindOverlap) {
		t.Fatalf("want overlap in batch, got %v", err)
	}
	if ke := err.(*Error); ke.CommandIndex != 1 {
		t.Fatalf("idx=%d", ke.CommandIndex)
	}
	if _, ok := e.Doc().Entities["c"]; ok {
		t.Fatal("half-applied batch")
	}

	// bad reference owner
	err = e.Commit(&EntityAdd{Entity: Entity{ID: "z", Type: "x", X: 8, Y: 8, Width: 1, Height: 1, Region: "nope"}})
	if !IsKind(err, KindBadReference) {
		t.Fatalf("want bad ref, got %v", err)
	}

	// region + ownership rule
	if err := e.Commit(&RegionAdd{Region: Region{ID: "r1", Name: "R", Area: Rect{X: 0, Y: 0, Width: 6, Height: 6}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Commit(&EntitySetOwner{ID: "c", Region: "r1"}); err == nil {
		t.Fatal("c missing expected")
	}
	if err := e.Commit(&EntityAdd{Entity: Entity{ID: "c", Type: "rock", X: 5, Y: 5, Width: 1, Height: 1, Region: "r1"}}); err != nil {
		t.Fatal(err)
	}
	err = e.Commit(&RegionRemove{ID: "r1"})
	if !IsKind(err, KindIllegalOwner) {
		t.Fatalf("want illegal owner, got %v", err)
	}
}

func TestSmokeArtifactFullEqIncremental(t *testing.T) {
	d, _ := NewDoc(8, 8)
	e, _ := NewEditor(d, Options{})
	cmds := []Command{
		&SetTiles{Layer: "t", Cells: []Cell{{1, 1, "a"}, {2, 2, "b"}, {1, 1, "c"}}},
		&RegionAdd{Region: Region{ID: "r", Name: "r", Area: Rect{0, 0, 4, 4}}},
		&EntityAdd{Entity: Entity{ID: "e1", Type: "t", X: 1, Y: 1, Width: 1, Height: 1, Region: "r"}},
		&EntityAdd{Entity: Entity{ID: "e2", Type: "t", X: 6, Y: 6, Width: 1, Height: 1}},
		&EntityMove{ID: "e2", To: Rect{X: 2, Y: 2, Width: 1, Height: 1}},
		&EntitySetOwner{ID: "e2", Region: "r"},
		&SetTile{Layer: "t", X: 0, Y: 0, Terrain: "x"},
	}
	for _, c := range cmds {
		if err := e.Commit(c); err != nil {
			t.Fatal(err)
		}
	}
	inc := e.Artifact()
	// fresh full rebuild from current doc
	e2, _ := NewEditor(e.Doc(), Options{RegionCacheSize: 1})
	full := e2.Artifact()
	if !reflect.DeepEqual(inc, full) {
		t.Fatalf("mismatch:\\ninc=%#v\\nfull=%#v", inc, full)
	}
	// undo everything; artifact must track back to initial
	for e.Undo() {
	}
	afterUndo := e.Artifact()
	e3, _ := NewEditor(d, Options{})
	if !reflect.DeepEqual(afterUndo, e3.Artifact()) {
		t.Fatalf("undo artifact mismatch:\\n%#v\\n%#v", afterUndo, e3.Artifact())
	}
}

func TestSmokeBundle(t *testing.T) {
	d, _ := NewDoc(6, 6)
	e, _ := NewEditor(d, Options{})
	e.Commit(&SetTile{Layer: "t", X: 0, Y: 0, Terrain: "g"})
	e.Commit(&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 1, Y: 1, Width: 1, Height: 1}})
	b, err := MarshalBundle(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Artifact(), e.Artifact()) {
		t.Fatal("artifact differs after import")
	}
	if !reflect.DeepEqual(got.History(), e.History()) {
		t.Fatal("history differs after import")
	}
	// determinism: marshal again byte-equal
	b2, _ := MarshalBundle(got)
	if !reflect.DeepEqual(b, b2) {
		t.Fatal("nondeterministic bundle")
	}
	// undo after import still works
	if !got.Undo() {
		t.Fatal("undo after import")
	}
	if _, ok := got.Doc().Entities["a"]; ok {
		t.Fatal("imported undo wrong")
	}
	// corrupt input
	bad := []byte(`{"version":1,"sha256":"00","payload":"00"}`)
	if _, err := UnmarshalBundle(bad); !IsKind(err, KindCorrupt) {
		t.Fatalf("want corrupt, %v", err)
	}
	if _, err := UnmarshalBundle([]byte(`{"version":99}`)); !IsKind(err, KindUnsupportedVersion) {
		t.Fatalf("want version err, %v", err)
	}
	if _, err := UnmarshalBundle([]byte(`{not json`)); !IsKind(err, KindCorrupt) {
		t.Fatalf("want corrupt json, %v", err)
	}
}

func TestSmokeDeterministicAcrossEditors(t *testing.T) {
	base := func() *Doc {
		d, _ := NewDoc(12, 12)
		d.EnsureLayer("g")
		d.EnsureLayer("d")
		return d
	}
	script := []Command{
		&SetTiles{Layer: "g", Cells: []Cell{{0, 0, "a"}, {1, 1, "b"}}},
		&RegionAdd{Region: Region{ID: "r", Name: "r", Area: Rect{0, 0, 5, 5}}},
		&EntityAdd{Entity: Entity{ID: "x", Type: "tree", X: 1, Y: 1, Width: 2, Height: 2, Region: "r"}},
		&SetTile{Layer: "d", X: 3, Y: 3, Terrain: "x"},
		&EntityMove{ID: "x", To: Rect{X: 2, Y: 2, Width: 2, Height: 2}},
		&EntitySetType{ID: "x", Type: "rock"},
	}
	run := func() (*Editor, []byte) {
		e, err := NewEditor(base(), Options{})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range script {
			if err := e.Commit(c); err != nil {
				t.Fatal(err)
			}
		}
		e.Undo()
		e.Undo()
		b, _ := MarshalBundle(e)
		return e, b
	}
	e1, b1 := run()
	e2, b2 := run()
	if !reflect.DeepEqual(e1.Doc(), e2.Doc()) {
		t.Fatal("docs differ across machines")
	}
	if !reflect.DeepEqual(e1.Artifact(), e2.Artifact()) {
		t.Fatal("artifacts differ")
	}
	if !reflect.DeepEqual(e1.History(), e2.History()) {
		t.Fatal("history differs")
	}
	if !reflect.DeepEqual(b1, b2) {
		t.Fatal("bundles differ byte-for-byte")
	}
}

func TestSmokeV0Migration(t *testing.T) {
	// legacy replay-only bundle
	v0 := []byte(`{
	  "version":0,"width":5,"height":5,
	  "commands":[
	    {"type":"set_tile","data":{"layer":"g","x":1,"y":1,"terrain":"grass"}},
	    {"type":"entity_add","data":{"entity":{"id":"a","type":"tree","x":0,"y":0,"width":1,"height":1,"region":""}}}
	  ]
	}`)
	e, err := UnmarshalBundle(v0)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Doc().Layers["g"].Tiles[6].Terrain; got != "grass" {
		t.Fatalf("v0 tile=%q", got)
	}
	if _, ok := e.Doc().Entities["a"]; !ok {
		t.Fatal("v0 entity missing")
	}
	if len(e.History()) != 2 {
		t.Fatalf("v0 history=%d", len(e.History()))
	}
	// undo works after migration
	if !e.Undo() {
		t.Fatal("v0 undo")
	}
	if _, ok := e.Doc().Entities["a"]; ok {
		t.Fatal("v0 undo left entity")
	}
	if !e.Redo() {
		t.Fatal("v0 redo")
	}
	if _, ok := e.Doc().Entities["a"]; !ok {
		t.Fatal("v0 redo missing entity")
	}

	// corrupt v0 must be rejected
	if _, err := UnmarshalBundle([]byte(`{"version":0,"width":0,"height":0,"commands":[]}`)); !IsKind(err, KindCorrupt) {
		t.Fatalf("want corrupt, got %v", err)
	}
	// invalid command type
	bad := []byte(`{"version":0,"width":3,"height":3,"commands":[{"type":"nope","data":{}}]}`)
	if _, err := UnmarshalBundle(bad); !IsKind(err, KindCorrupt) {
		t.Fatalf("want corrupt cmd, got %v", err)
	}
}

// TestDeterministicSerialization checks the bundle is canonical JSON
// with sorted keys and does not depend on map iteration order: marshal
// 20 times and compare every byte.
func TestDeterministicSerialization(t *testing.T) {
	d, _ := NewDoc(20, 20)
	e, _ := NewEditor(d, Options{})
	for i := 0; i < 15; i++ {
		if err := e.Commit(&SetTile{Layer: "L" + string(rune('a'+i%5)),
			X: i, Y: i % 20, Terrain: []string{"", "a", "b", "c"}[i%4]}); err != nil {
			t.Fatal(err)
		}
		if err := e.Commit(&EntityAdd{Entity: Entity{
			ID: "id-" + string(rune('a'+i)), Type: "t",
			X: (i * 3) % 19, Y: (i * 7) % 19, Width: 1, Height: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := MarshalBundle(e)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		b, err := MarshalBundle(e)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(b, first) {
			t.Fatalf("marshal iteration %d differs", i)
		}
	}
	// envelope must be valid JSON and contain the version field
	var env map[string]any
	if err := json.Unmarshal(first, &env); err != nil {
		t.Fatal(err)
	}
	if env["version"].(float64) != float64(BundleVersionV1) {
		t.Fatal("version field wrong")
	}
}

// TestCompositeRoundTrip exercises encode/decode of a nested composite
// through a bundle and verifies undo afterwards.
func TestCompositeRoundTrip(t *testing.T) {
	e, _ := NewEditor(mustDoc(10, 10), Options{})
	err := e.Commit(
		&SetTiles{Layer: "g", Cells: []Cell{{0, 0, "x"}, {1, 1, "y"}}},
		&RegionAdd{Region: Region{ID: "r", Area: Rect{0, 0, 5, 5}}},
		&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 2, Y: 2, Width: 1, Height: 1, Region: "r"}},
		&EntityMove{ID: "a", To: Rect{X: 3, Y: 3, Width: 1, Height: 1}},
		&EntitySetType{ID: "a", Type: "rock"},
		&EntitySetOwner{ID: "a", Region: ""},
	)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalBundle(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Doc(), e.Doc()) {
		t.Fatal("composite doc round-trip mismatch")
	}
	if len(got.History()) != 1 || got.History()[0].Kind != "composite" ||
		got.History()[0].MergeCount != 1 {
		t.Fatalf("composite history wrong: %+v", got.History())
	}
	if !got.Undo() {
		t.Fatal("undo after import")
	}
	f, _ := NewEditor(mustDoc(10, 10), Options{})
	if !reflect.DeepEqual(got.Doc(), f.Doc()) {
		t.Fatal("composite undo did not restore empty document")
	}
	if !got.Redo() {
		t.Fatal("redo after import")
	}
	if !reflect.DeepEqual(got.Doc(), e.Doc()) {
		t.Fatal("composite redo mismatch")
	}
}
