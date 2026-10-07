package levelcore

import (
	"testing"
	"time"
)

func TestSmoke(t *testing.T) {
	d := NewDocument(32, 32)
	e, err := NewEditor(d, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Unix(1000, 0)
	if _, err := e.Submit(NewAddLayerCommand(Layer{ID: "ground", Name: "Ground", Width: 32, Height: 32}, ts)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(NewSetTileCommand("ground", Point{X: 1, Y: 1}, 7, ts.Add(10*time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(NewPlaceEntityCommand(Entity{ID: "e1", Kind: "npc", LayerID: "ground", Pos: Point{X: 2, Y: 2}, Solid: true}, ts)); err != nil {
		t.Fatal(err)
	}
	// overlap reject
	_, err = e.Submit(NewPlaceEntityCommand(Entity{ID: "e2", Kind: "npc", LayerID: "ground", Pos: Point{X: 2, Y: 2}, Solid: true}, ts))
	if re, ok := AsReject(err); !ok || re.Code != RejectOverlap || re.CommandIndex != 0 {
		t.Fatalf("want overlap at index 0, got %v", err)
	}
	// undo
	if !e.Undo() {
		t.Fatal("undo failed")
	}
	// pack roundtrip
	pk, err := Pack(e.Document(), e.Artifact())
	if err != nil {
		t.Fatal(err)
	}
	d2, a2, err := Unpack(pk)
	if err != nil {
		t.Fatal(err)
	}
	if d2.MapWidth != 32 {
		t.Fatalf("map width: %d", d2.MapWidth)
	}
	if v, _ := d2.Tile("ground", Point{1, 1}); v != 7 {
		t.Fatalf("tile after unpack: %d", v)
	}
	if a2 == nil || a2.Revision != e.Revision() {
		t.Fatalf("artifact revision mismatch: %+v", a2)
	}
}
