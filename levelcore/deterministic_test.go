package levelcore

import (
	"bytes"
	"testing"
	"time"
)

// 属性键乱序插入必须产生相同的规范化 JSON。
func TestMarshalDeterministicProps(t *testing.T) {
	mk := func(order ...string) *Document {
		d := NewDocument(8, 8)
		p := map[string]string{}
		for _, k := range order {
			p[k] = "v" + k
		}
		d.Layers["l"] = &Layer{ID: "l", Width: 8, Height: 8}
		d.LayerOrder = []string{"l"}
		d.tiles["l"] = &tileState{pages: map[uint64]*tilePage{}}
		d.Zones["z"] = &Zone{ID: "z", Kind: "s", Bounds: Rect{0, 0, 2, 2}, Props: p}
		d.ZoneOrder = []string{"z"}
		d.rebuildIndex()
		return d
	}
	a, _ := MarshalDocument(mk("a", "b", "c"))
	b, _ := MarshalDocument(mk("c", "a", "b"))
	if !bytes.Equal(a, b) {
		t.Fatalf("non-deterministic:\n%s\n%s", a, b)
	}
}

// 合并撤销之后产生新编辑，必须截断重做历史且状态正确。
func TestCoalesceUndoThenEditTruncates(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	ts := t0()
	e.Submit(NewSetTileCommand("ground", Point{1, 1}, 1, ts))
	e.Submit(NewSetTileCommand("ground", Point{1, 1}, 2, ts.Add(10*time.Millisecond)))
	e.Submit(NewSetTileCommand("ground", Point{1, 1}, 3, ts.Add(20*time.Millisecond)))
	if len(e.History().Entries) != 2 { // AddLayer + coalesced tile
		t.Fatalf("entries=%d", len(e.History().Entries))
	}
	e.Undo()
	if v, _ := e.Document().Tile("ground", Point{1, 1}); v != 0 {
		t.Fatalf("after undo v=%d", v)
	}
	if !e.CanRedo() {
		t.Fatal("redo expected")
	}
	e.Submit(NewSetTileCommand("ground", Point{2, 2}, 8, ts.Add(1000*time.Millisecond)))
	if e.CanRedo() {
		t.Fatal("redo must be truncated")
	}
	if v, _ := e.Document().Tile("ground", Point{1, 1}); v != 0 {
		t.Fatalf("stale value at (1,1): %d", v)
	}
	if v, _ := e.Document().Tile("ground", Point{2, 2}); v != 8 {
		t.Fatalf("new value missing: %d", v)
	}
}
