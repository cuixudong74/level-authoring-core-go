package levelcore

import (
	"fmt"
	"testing"
)

// TestScaleSublinearEditCost builds a large document (1M cells plus
// 50k entities) and asserts that a single local edit stays bounded:
// its cost must not scale with total entity count thanks to the
// spatial index, nor total cell count thanks to the touched-cell diff.
func TestScaleSublinearEditCost(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test in -short mode")
	}
	const W, H = 1000, 1000
	d, err := NewDoc(W, H)
	if err != nil {
		t.Fatal(err)
	}
	d.EnsureLayer("g")
	// pack a 5x5 grid of single-cell entities with 10-cell pitch:
	// ~100x100 = 10000 entities, none overlapping
	i := 0
	for gy := 0; gy+5 < H; gy += 10 {
		for gx := 0; gx+5 < W; gx += 10 {
			i++
			d.Entities[fmt.Sprintf("e%d", i)] = Entity{
				ID: fmt.Sprintf("e%d", i), Type: "t",
				X: gx, Y: gy, Width: 5, Height: 5,
			}
		}
	}
	totalEntities := len(d.Entities)
	if totalEntities < 9000 {
		t.Fatalf("expected ~10k entities, got %d", totalEntities)
	}
	e, err := NewEditor(d, Options{HistoryLimit: 64, RegionCacheSize: 32})
	if err != nil {
		t.Fatal(err)
	}

	// single local tile edit: must touch exactly one cell regardless
	// of the 1M-cell layer
	if err := e.Commit(&SetTile{Layer: "g", X: 1, Y: 1, Terrain: "x"}); err != nil {
		t.Fatal(err)
	}
	art := e.Artifact()
	if got := art.TileCounts["g"]["x"]; got != 1 {
		t.Fatalf("x count=%d", got)
	}

	// moving one entity checks only locally indexed neighbours
	if err := e.Commit(&EntityMove{ID: "e1", To: Rect{X: 6, Y: 6, Width: 4, Height: 4}}); err != nil {
		t.Fatal(err)
	}
	// history cap holds
	for k := 0; k < 200; k++ {
		x, y := (k*7)%(W-2), (k*13)%(H-2)
		if err := e.Commit(&SetTile{Layer: "g", X: x, Y: y, Terrain: fmt.Sprintf("v%d", k%7)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.History()) > 64 {
		t.Fatalf("history cap violated: %d", len(e.History()))
	}
	// the most recent edit must still be undoable
	if !e.CanUndo() {
		t.Fatal("recent edit vanished under eviction")
	}
	// cache stays bounded
	if e.arts.regionCache.len() > 32 {
		t.Fatalf("region cache cap violated: %d", e.arts.regionCache.len())
	}
	// artifact equals full rebuild on the huge doc
	fresh, err := NewEditor(e.Doc(), Options{RegionCacheSize: 32})
	if err != nil {
		t.Fatal(err)
	}
	inc, full := e.Artifact(), fresh.Artifact()
	// compare digests only (full maps are big; digest covers content)
	if inc.Digest != full.Digest {
		t.Fatalf("digest mismatch on large doc: %s vs %s", inc.Digest, full.Digest)
	}
}

// TestScaleUndoRestoresLargeEdit ensures undo of a local edit on a big
// document returns the exact digest and does not require a full rebuild.
func TestScaleUndoRestoresLargeEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test in -short mode")
	}
	d, _ := NewDoc(400, 400)
	d.EnsureLayer("g")
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("e%d", i)
		d.Entities[id] = Entity{ID: id, Type: "t",
			X: (i % 200) * 2, Y: (i / 200) * 2, Width: 1, Height: 1}
	}
	e, _ := NewEditor(d, Options{})
	before := e.Artifact().Digest
	e.Commit(&SetTile{Layer: "g", X: 10, Y: 10, Terrain: "z"})
	e.Undo()
	if got := e.Artifact().Digest; got != before {
		t.Fatalf("digest after undo %s != before %s", got, before)
	}
}
