package levelcore

import (
	"fmt"
	"testing"
)

func makeBigEditor(b *testing.B, n int) *Editor {
	b.Helper()
	d, _ := NewDoc(2000, 2000)
	d.EnsureLayer("g")
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("e%d", i)
		d.Entities[id] = Entity{ID: id, Type: "t",
			X: (i % 1000) * 2, Y: (i / 1000) * 2, Width: 1, Height: 1}
	}
	e, err := NewEditor(d, Options{HistoryLimit: 128})
	if err != nil {
		b.Fatal(err)
	}
	return e
}

// BenchmarkLocalTileEdit measures one-cell terrain edits in documents
// with increasing entity counts. Time per op should stay essentially
// constant as n grows.
func BenchmarkLocalTileEdit(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("entities=%d", n), func(b *testing.B) {
			e := makeBigEditor(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := e.Commit(&SetTile{Layer: "g",
					X: (i * 37) % 1900, Y: (i * 53) % 1900,
					Terrain: fmt.Sprintf("t%d", i%4)}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLocalEntityMove measures one entity move in documents of
// increasing total entity count.
func BenchmarkLocalEntityMove(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("entities=%d", n), func(b *testing.B) {
			e := makeBigEditor(b, n)
			b.ResetTimer()
			// entities are packed at pitch 2 on each row; move e0
			// along the entity-free lane at y=1000+delta
			for i := 0; i < b.N; i++ {
				if err := e.Commit(&EntityMove{ID: "e0",
					To: Rect{X: 5 + (i % 20), Y: 1900, Width: 1, Height: 1}}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
