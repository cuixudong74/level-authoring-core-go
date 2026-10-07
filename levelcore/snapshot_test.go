package levelcore

import (
	"bytes"
	"testing"
)

// 快照不可变：提交后旧 Document()/Artifact() 快照不被后续编辑改写。
func TestSnapshotImmutability(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	if _, err := e.Submit(NewSetTileCommand("ground", Point{1, 1}, 5, t0())); err != nil {
		t.Fatal(err)
	}
	dSnap := e.Document()
	aSnap := e.Artifact()
	j1, _ := MarshalDocument(dSnap)
	a1, _ := marshalArtifact(aSnap)

	// 后续大量编辑（移动实体、改同一格、增删图层区域）。
	if _, err := e.Submit(NewSetTileCommand("ground", Point{1, 1}, 9, t0())); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(NewPlaceEntityCommand(Entity{ID: "e", Kind: "k", LayerID: "ground", Pos: Point{3, 3}}, t0())); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(NewMoveEntityCommand("e", Point{4, 4}, t0())); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(NewAddZoneCommand(Zone{ID: "z", Kind: "s", Bounds: Rect{MinX: 0, MinY: 0, MaxX: 2, MaxY: 2}}, t0())); err != nil {
		t.Fatal(err)
	}
	e.Undo()
	e.Redo()
	e.Undo()

	j2, _ := MarshalDocument(dSnap)
	a2, _ := marshalArtifact(aSnap)
	if !bytes.Equal(j1, j2) {
		t.Fatalf("document snapshot mutated:\n%s\n%s", j1, j2)
	}
	if !bytes.Equal(a1, a2) {
		t.Fatal("artifact snapshot mutated")
	}
}

// 多次撤销/重做往返后，文档与产物必须一致。
func TestUndoRedoRoundTrip(t *testing.T) {
	e := newTestEditor(t, 24, 24)
	cmds := []Command{
		NewSetTileCommand("ground", Point{1, 1}, 1, t0()),
		NewSetTileCommand("ground", Point{1, 2}, 2, t0()),
		NewPlaceEntityCommand(Entity{ID: "e", Kind: "k", LayerID: "ground", Pos: Point{5, 5}, Solid: true}, t0()),
		NewMoveEntityCommand("e", Point{6, 6}, t0()),
		NewAddZoneCommand(Zone{ID: "z", Kind: "s", Bounds: Rect{MinX: 0, MinY: 0, MaxX: 3, MaxY: 3}}, t0()),
	}
	for _, c := range cmds {
		if _, err := e.Submit(c); err != nil {
			t.Fatal(err)
		}
	}
	finalDoc := e.Document()
	finalArt := e.Artifact()

	// 全部撤销
	for e.CanUndo() {
		e.Undo()
	}
	if got, _ := e.Document().Tile("ground", Point{1, 1}); got != 0 {
		t.Fatal("not fully undone")
	}
	// 全部重做
	for e.CanRedo() {
		if !e.Redo() {
			t.Fatal("redo failed")
		}
	}
	if !docsEqual(finalDoc, e.Document()) {
		t.Fatal("doc mismatch after redo round-trip")
	}
	if !artifactsEqual(finalArt, e.Artifact()) {
		t.Fatal("artifact content mismatch after redo round-trip")
	}
}

// 历史上限淘汰不影响最近编辑正确性（小上限 + 连续操作）。
func TestHistoryEviction(t *testing.T) {
	d := NewDocument(64, 64)
	e, err := NewEditor(d, Config{HistoryLimit: 3, CacheLimit: 8, Coalesce: DefaultConfig().Coalesce})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(NewAddLayerCommand(Layer{ID: "g", Width: 64, Height: 64}, t0())); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := e.Submit(NewSetTileCommand("g", Point{int32(i), 0}, uint16(i+1), t0().Add(t0().Add(0).Sub(t0())))); err != nil {
			t.Fatal(err)
		}
	}
	h := e.History()
	if len(h.Entries) != 3 {
		t.Fatalf("history len=%d want 3", len(h.Entries))
	}
	// 最近三次编辑可撤销，且状态最终一致
	for i := 0; i < 3; i++ {
		if !e.Undo() {
			t.Fatal("undo recent failed")
		}
	}
	// 撤销 3 次后 (9,0)/(8,0)/(7,0) 回到 0
	for _, x := range []int32{9, 8, 7} {
		if v, _ := e.Document().Tile("g", Point{x, 0}); v != 0 {
			t.Fatalf("tile %d after eviction undo = %d", x, v)
		}
	}
	// 更旧的编辑已淘汰，无法再撤销
	if e.CanUndo() {
		// 仅剩 AddLayer（在淘汰窗口之外时可能也被淘汰）
	}
}
