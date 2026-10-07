package levelcore

import "testing"

// BenchmarkSubmitTileSparse 测量对"稀疏块内单个单元"的编辑耗时。
// 每次编辑落在不同块，块内始终只有 1 个非零单元：
// 成本应与文档实体/块总量无关。
func BenchmarkSubmitTileSparse(b *testing.B) {
	for _, size := range []struct{ n, ent int }{
		{256, 1000},
		{1024, 12000},
	} {
		e := buildScaleDocB(b, int32(size.n), size.ent)
		b.Run("", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				// 每个块只写一个单元：坐标步进一个块（16 格）。
				x := int32(((i * 16) + 1) % size.n)
				y := int32(((i*16/size.n)*16 + 1) % size.n)
				if _, err := e.Submit(NewSetTileCommand("g", Point{x, y}, uint16(i+1), t0())); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func buildScaleDocB(b testing.TB, n int32, entities int) *Editor {
	e, err := NewEditor(NewDocument(n, n), DefaultConfig())
	if err != nil {
		b.Fatal(err)
	}
	if _, err := e.Submit(NewAddLayerCommand(Layer{ID: "g", Width: n, Height: n}, t0())); err != nil {
		b.Fatal(err)
	}
	batch := make([]Command, 0, 2000)
	for i := 0; i < entities; i++ {
		x := int32(i % int(n))
		y := int32(i / int(n))
		batch = append(batch, NewPlaceEntityCommand(
			Entity{ID: "e" + intToStr(int32(i)), Kind: "k", LayerID: "g", Pos: Point{x, y}}, t0()))
		if len(batch) == 2000 {
			if _, err := e.SubmitBatch(batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if _, err := e.SubmitBatch(batch); err != nil {
			b.Fatal(err)
		}
	}
	return e
}
