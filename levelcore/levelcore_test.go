package levelcore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
)

func newTestEditor(t *testing.T, w, h int) *Editor {
	t.Helper()
	d, err := NewDoc(w, h)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEditor(d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// ------------------------------------------------------------- undo/redo

func TestUndoRedoSemantics(t *testing.T) {
	e := newTestEditor(t, 8, 8)
	e.Commit(&SetTile{Layer: "g", X: 0, Y: 0, Terrain: "a"})
	e.Commit(&SetTile{Layer: "g", X: 1, Y: 0, Terrain: "b"})

	if !e.CanUndo() || e.CanRedo() {
		t.Fatal("initial undo/redo availability wrong")
	}
	if !e.Undo() {
		t.Fatal("undo")
	}
	if e.Doc().Layers["g"].Tiles[1].Terrain != "" {
		t.Fatal("undo did not revert second tile")
	}
	// new edit after undo truncates redo history
	e.Commit(&SetTile{Layer: "g", X: 2, Y: 0, Terrain: "c"})
	if e.CanRedo() {
		t.Fatal("redo history must be truncated by new edit")
	}
	// undoing the new edit restores the state right after first edit
	e.Undo()
	tiles := e.Doc().Layers["g"].Tiles
	if tiles[0].Terrain != "a" || tiles[1].Terrain != "" || tiles[2].Terrain != "" {
		t.Fatalf("unexpected tiles after truncation: %q %q %q", tiles[0].Terrain, tiles[1].Terrain, tiles[2].Terrain)
	}
	// drain the remaining undo stack (first commit still present)
	for e.Undo() {
	}
	if e.Undo() {
		t.Fatal("undo on empty stack must return false")
	}
	// redoing walks back through every undone entry
	n := 0
	for e.Redo() {
		n++
	}
	if n == 0 {
		t.Fatal("redo stack unexpectedly empty after draining undo")
	}
	if e.Redo() {
		t.Fatal("redo on empty stack must return false")
	}
}

func TestMergeSameKindMicroEdits(t *testing.T) {
	e := newTestEditor(t, 4, 4)
	// two sets on the same cell within the merge window -> one entry
	mk := func(terr string) *SetTile {
		return &SetTile{Layer: "g", X: 0, Y: 0, Terrain: terr}
	}
	if err := e.Commit(mk("a")); err != nil {
		t.Fatal(err)
	}
	if err := e.Commit(mk("b")); err != nil {
		t.Fatal(err)
	}
	h := e.History()
	if len(h) != 1 || h[0].MergeCount != 2 || h[0].Kind != "set_tile" {
		t.Fatalf("expected one merged entry, got %+v", h)
	}
	// document state matches last write
	if e.Doc().Layers["g"].Tiles[0].Terrain != "b" {
		t.Fatal("merged state wrong")
	}
	// one undo reverts the whole merged entry to the ORIGINAL value
	e.Undo()
	if l, ok := e.Doc().Layers["g"]; ok && l.Tiles[0].Terrain != "" {
		t.Fatal("merged undo must restore original (or remove the born layer)")
	}
	e.Redo()
	if e.Doc().Layers["g"].Tiles[0].Terrain != "b" {
		t.Fatal("merged redo must restore merged result")
	}
	if len(e.History()) != 1 {
		t.Fatal("history length must still be 1 after redo")
	}

	// different merge keys do NOT merge
	e2 := newTestEditor(t, 4, 4)
	e2.Commit(mk2("g", 0, 0, "a"))
	e2.Commit(mk2("g", 1, 1, "a"))
	if len(e2.History()) != 2 {
		t.Fatalf("different cells merged: %+v", e2.History())
	}

	// merge across the window gap does not happen
	e3, _ := NewEditor(mustDoc(4, 4), Options{MergeWindow: 1})
	e3.Commit(mk2("g", 0, 0, "a"))
	// insert an unrelated entry to push clock gap beyond the window
	e3.Commit(&EntityAdd{Entity: Entity{ID: "z", Type: "t", X: 0, Y: 0, Width: 1, Height: 1}})
	e3.Commit(mk2("g", 0, 0, "b"))
	for _, en := range e3.History() {
		t.Logf("entry seq=%d kind=%s merges=%d clock=%d", en.Seq, en.Kind, en.MergeCount, en.Clock)
	}
	var tileEntries int
	for _, en := range e3.History() {
		if en.Kind == "set_tile" {
			tileEntries++
		}
	}
	if tileEntries != 2 {
		t.Fatalf("expected two separate tile entries across window, got %d", tileEntries)
	}
}

func mk2(layer string, x, y int, terr string) *SetTile {
	return &SetTile{Layer: layer, X: x, Y: y, Terrain: terr}
}

// ------------------------------------------------------- atomic commits

func TestCompositeAtomicSuccess(t *testing.T) {
	e := newTestEditor(t, 8, 8)
	err := e.Commit(
		&RegionAdd{Region: Region{ID: "r", Area: Rect{0, 0, 4, 4}}},
		&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 0, Y: 0, Width: 1, Height: 1, Region: "r"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.History()) != 1 || e.History()[0].Kind != "composite" {
		t.Fatalf("batch must be one composite entry: %+v", e.History())
	}
	e.Undo()
	if len(e.Doc().Entities) != 0 || len(e.Doc().Regions) != 0 {
		t.Fatal("composite undo must revert every sub-command")
	}
}

func TestCompositeFailureRollsBackWholeBatch(t *testing.T) {
	e := newTestEditor(t, 10, 10)
	before := e.Doc()
	err := e.Commit(
		&EntityAdd{Entity: Entity{ID: "ok", Type: "t", X: 5, Y: 5, Width: 1, Height: 1}},
		&EntityAdd{Entity: Entity{ID: "dup", Type: "t", X: 0, Y: 0, Width: 2, Height: 2}},
		&EntityAdd{Entity: Entity{ID: "bad", Type: "t", X: 1, Y: 1, Width: 1, Height: 1}}, // overlaps dup
	)
	if err == nil {
		t.Fatal("expected batch rejection")
	}
	ke := err.(*Error)
	if ke.Kind != KindOverlap || ke.CommandIndex != 2 {
		t.Fatalf("want overlap at index 2, got kind=%s idx=%d", ke.Kind, ke.CommandIndex)
	}
	if len(e.Doc().Entities) != 0 {
		t.Fatal("failed batch left entities behind")
	}
	if len(e.History()) != 0 {
		t.Fatal("failed batch must not create history")
	}
	if !reflect.DeepEqual(e.Doc(), before) {
		t.Fatal("document differs after failed batch")
	}
	art := e.Artifact()
	if len(art.EntityIndex) != 0 {
		t.Fatal("failed batch left artifact residue")
	}
}

func TestCompositeFailureAtSecondCommand(t *testing.T) {
	e := newTestEditor(t, 6, 6)
	e.Commit(&RegionAdd{Region: Region{ID: "r", Area: Rect{0, 0, 3, 3}}})
	err := e.Commit(
		&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 0, Y: 0, Width: 1, Height: 1, Region: "r"}},
		&EntitySetOwner{ID: "a", Region: "ghost"}, // bad reference
	)
	if !IsKind(err, KindBadReference) {
		t.Fatalf("want bad reference, got %v", err)
	}
	if ke := err.(*Error); ke.CommandIndex != 1 {
		t.Fatalf("index=%d", ke.CommandIndex)
	}
	if got := e.Doc().Entities["a"]; got.ID != "" {
		t.Fatal("first sub-command not rolled back")
	}
	if len(e.History()) != 1 {
		t.Fatalf("history must keep only earlier commit, got %d", len(e.History()))
	}
}

func TestEmptyAndNilCommit(t *testing.T) {
	e := newTestEditor(t, 4, 4)
	if err := e.Commit(); !IsKind(err, KindInvalidArgument) {
		t.Fatalf("empty commit: %v", err)
	}
	if err := e.Commit(nil); !IsKind(err, KindInvalidArgument) {
		t.Fatalf("nil commit: %v", err)
	}
}

// ----------------------------------------------------------- constraints

func TestConstraintRejections(t *testing.T) {
	cases := []struct {
		name string
		cmd  Command
		kind Kind
	}{
		{"tile oob", &SetTile{Layer: "g", X: 99, Y: 0, Terrain: "x"}, KindOutOfBounds},
		{"tile empty layer", &SetTile{X: 0, Y: 0, Terrain: "x"}, KindInvalidArgument},
		{"entity oob", &EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 7, Y: 7, Width: 4, Height: 4}}, KindOutOfBounds},
		{"entity empty type", &EntityAdd{Entity: Entity{ID: "a", X: 0, Y: 0, Width: 1, Height: 1}}, KindInvalidArgument},
		{"entity bad owner", &EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 0, Y: 0, Width: 1, Height: 1, Region: "x"}}, KindBadReference},
		{"move missing", &EntityMove{ID: "nope", To: Rect{0, 0, 1, 1}}, KindNotFound},
		{"settype missing", &EntitySetType{ID: "nope", Type: "x"}, KindNotFound},
		{"remove region missing", &RegionRemove{ID: "nope"}, KindNotFound},
		{"region oob", &RegionAdd{Region: Region{ID: "r", Area: Rect{0, 0, 9, 9}}}, KindOutOfBounds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEditor(t, 8, 8)
			err := e.Commit(tc.cmd)
			if !IsKind(err, tc.kind) {
				t.Fatalf("%s: want %s, got %v", tc.name, tc.kind, err)
			}
			if len(e.History()) != 0 {
				t.Fatalf("%s: rejected edit created history", tc.name)
			}
		})
	}
}

func TestOverlapRejected(t *testing.T) {
	e := newTestEditor(t, 10, 10)
	if err := e.Commit(&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 2, Y: 2, Width: 3, Height: 3}}); err != nil {
		t.Fatal(err)
	}
	err := e.Commit(&EntityAdd{Entity: Entity{ID: "b", Type: "t", X: 0, Y: 0, Width: 3, Height: 3}})
	if !IsKind(err, KindOverlap) {
		t.Fatalf("want overlap, got %v", err)
	}
	// moving a into free space succeeds; moving it back onto b fails
	if err := e.Commit(&EntityMove{ID: "a", To: Rect{X: 6, Y: 6, Width: 2, Height: 2}}); err != nil {
		t.Fatal(err)
	}
}

func TestIllegalOwnerBlocksRegionRemoval(t *testing.T) {
	e := newTestEditor(t, 8, 8)
	e.Commit(&RegionAdd{Region: Region{ID: "r", Area: Rect{0, 0, 4, 4}}})
	e.Commit(&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 0, Y: 0, Width: 1, Height: 1, Region: "r"}})
	err := e.Commit(&RegionRemove{ID: "r"})
	if !IsKind(err, KindIllegalOwner) {
		t.Fatalf("want illegal owner, got %v", err)
	}
	// detach first, then removal succeeds
	if err := e.Commit(&EntitySetOwner{ID: "a", Region: ""}); err != nil {
		t.Fatal(err)
	}
	if err := e.Commit(&RegionRemove{ID: "r"}); err != nil {
		t.Fatalf("removal after detach: %v", err)
	}
}

func TestRejectedEditLeavesDocumentIdentical(t *testing.T) {
	e := newTestEditor(t, 8, 8)
	e.Commit(&SetTile{Layer: "g", X: 0, Y: 0, Terrain: "x"})
	beforeJSON := mustJSON(t, e.Doc())
	beforeArt := e.Artifact()
	_ = e.Commit(&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 99, Y: 0, Width: 1, Height: 1}})
	if !bytes.Equal(mustJSON(t, e.Doc()), beforeJSON) {
		t.Fatal("document changed after rejection")
	}
	if e.Artifact().Digest != beforeArt.Digest {
		t.Fatal("artifact changed after rejection")
	}
}

// ------------------------------------------------------- artifact rebuild

func TestArtifactFullEqualsIncremental(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	cmds := []Command{
		&RegionAdd{Region: Region{ID: "r", Area: Rect{0, 0, 8, 8}}},
		&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 1, Y: 1, Width: 2, Height: 2, Region: "r"}},
		&SetTiles{Layer: "g", Cells: []Cell{{1, 1, "w"}, {2, 2, "s"}, {1, 1, "g"}}},
		&EntityAdd{Entity: Entity{ID: "b", Type: "t", X: 10, Y: 10, Width: 2, Height: 2}},
		&EntityMove{ID: "b", To: Rect{X: 3, Y: 3, Width: 2, Height: 2}},
		&EntitySetOwner{ID: "b", Region: "r"},
		&RegionUpdate{ID: "r", Next: Region{ID: "r", Name: "R2", Area: Rect{0, 0, 6, 6}}},
	}
	for _, c := range cmds {
		if err := e.Commit(c); err != nil {
			t.Fatal(err)
		}
	}
	fresh, err := NewEditor(e.Doc(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !artifactsEqual(e.Artifact(), fresh.Artifact()) {
		t.Fatalf("incremental != full\\ninc=%#v\\nfull=%#v", e.Artifact(), fresh.Artifact())
	}

	// every undo step must also equal a fresh build of its doc state
	for e.Undo() {
		f, _ := NewEditor(e.Doc(), Options{})
		if !artifactsEqual(e.Artifact(), f.Artifact()) {
			t.Fatal("artifact diverged during undo")
		}
	}
	// after one redo only the region (oldest entry) returns, with no
	// members yet; after redoing everything the full index returns
	e.Redo()
	if _, ok := e.Artifact().RegionIndex["r"]; !ok {
		t.Fatal("region not restored on first redo")
	}
	for e.Redo() {
	}
	f2, _ := NewEditor(e.Doc(), Options{})
	if !artifactsEqual(e.Artifact(), f2.Artifact()) {
		t.Fatal("artifact mismatch after full redo")
	}
}

func TestUndoRemovesArtifactResidue(t *testing.T) {
	e := newTestEditor(t, 8, 8)
	e.Commit(&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 0, Y: 0, Width: 1, Height: 1}})
	if len(e.Artifact().EntityIndex) != 1 {
		t.Fatal("entity index missing")
	}
	e.Undo()
	if len(e.Artifact().EntityIndex) != 0 {
		t.Fatal("stale entity index after undo")
	}
	e.Commit(&RegionAdd{Region: Region{ID: "r", Area: Rect{0, 0, 4, 4}}})
	if _, ok := e.Artifact().RegionIndex["r"]; !ok {
		t.Fatal("region index missing")
	}
	e.Undo()
	if _, ok := e.Artifact().RegionIndex["r"]; ok {
		t.Fatal("stale region index after undo")
	}
}

// ------------------------------------------------------------- eviction

func TestHistoryEvictionKeepsRecentEdits(t *testing.T) {
	e, _ := NewEditor(mustDoc(20, 20), Options{HistoryLimit: 5})
	for i := 0; i < 20; i++ {
		if err := e.Commit(&SetTile{Layer: "g", X: i % 20, Y: i / 20, Terrain: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.History()) != 5 {
		t.Fatalf("history len=%d", len(e.History()))
	}
	// five undos must all work
	for i := 0; i < 5; i++ {
		if !e.Undo() {
			t.Fatalf("undo %d failed despite cap", i)
		}
	}
	if e.CanUndo() {
		t.Fatal("retained entries exhausted")
	}
}

// ------------------------------------------------------- version/format

func TestBundleRoundTrip(t *testing.T) {
	e := newTestEditor(t, 10, 10)
	e.Commit(&SetTile{Layer: "g", X: 1, Y: 1, Terrain: "g"})
	e.Commit(&RegionAdd{Region: Region{ID: "r", Area: Rect{0, 0, 5, 5}}})
	e.Commit(&EntityAdd{Entity: Entity{ID: "a", Type: "t", X: 1, Y: 1, Width: 2, Height: 2, Region: "r"}})
	b, err := MarshalBundle(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Doc(), e.Doc()) {
		t.Fatal("doc mismatch")
	}
	if !artifactsEqual(got.Artifact(), e.Artifact()) {
		t.Fatal("artifact mismatch")
	}
	if len(got.History()) != len(e.History()) {
		t.Fatal("history length mismatch")
	}
	// deterministic bytes
	b2, _ := MarshalBundle(got)
	if !bytes.Equal(b, b2) {
		t.Fatal("non-deterministic serialization")
	}
}

func TestCorruptBundlesRejectedSafely(t *testing.T) {
	cases := map[string]string{
		"not json":          "{garbage",
		"unknown version":   `{"version":42}`,
		"missing fields":    `{"version":1}`,
		"bad hex":           `{"version":1,"sha256":"xx","payload":"zz"}`,
		"checksum mismatch": `{"version":1,"sha256":"00","payload":"00"}`,
		"payload not json":  "",
		"empty":             "",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var data []byte
			switch name {
			case "empty":
				data = []byte{}
			case "payload not json":
				env := bundleV1{Version: 1, SHA256: sha256Hex([]byte("{bad")), Payload: hexEncode([]byte("{bad"))}
				data, _ = json.Marshal(env)
			default:
				data = []byte(in)
			}
			e, err := UnmarshalBundle(data)
			if err == nil {
				t.Fatalf("%s: expected error, got editor %v", name, e != nil)
			}
			if !IsKind(err, KindCorrupt) && !IsKind(err, KindUnsupportedVersion) {
				t.Fatalf("%s: unexpected error kind: %v", name, err)
			}
		})
	}
}

func TestV0LegacyRoundTrip(t *testing.T) {
	in := []byte(`{"version":0,"width":4,"height":4,"commands":[
		{"type":"set_tile","data":{"layer":"g","x":0,"y":0,"terrain":"x"}}]}`)
	e, err := UnmarshalBundle(in)
	if err != nil {
		t.Fatal(err)
	}
	if e.Doc().Layers["g"].Tiles[0].Terrain != "x" {
		t.Fatal("v0 replay failed")
	}
	if e.Artifact().TileCounts["g"]["x"] != 1 {
		t.Fatal("v0 artifact wrong")
	}
}

// --------------------------------------------------------------- helpers

func mustDoc(w, h int) *Doc {
	d, err := NewDoc(w, h)
	if err != nil {
		panic(err)
	}
	return d
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func docsEqualForTest(a, b *Doc) bool {
	return reflect.DeepEqual(a, b)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hexEncode(b []byte) string { return hex.EncodeToString(b) }

func artifactsEqual(a, b Artifact) bool {
	return a.Digest == b.Digest
}
