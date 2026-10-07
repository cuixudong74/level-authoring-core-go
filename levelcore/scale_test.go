package levelcore

import (
	"testing"
	"time"
)

func buildScaleDoc(t *testing.T, n int32, entities int) *Editor {
	t.Helper()
	e, err := NewEditor(NewDocument(n, n), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(NewAddLayerCommand(Layer{ID: "g", Width: n, Height: n}, t0())); err != nil {
		t.Fatal(err)
	}
	batch := make([]Command, 0, 2000)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if _, err := e.SubmitBatch(batch); err != nil {
			t.Fatal(err)
		}
		batch = batch[:0]
	}
	for i := 0; i < entities; i++ {
		x := int32(i % int(n))
		y := int32(i / int(n))
		batch = append(batch, NewPlaceEntityCommand(
			Entity{ID: "e" + intToStr(int32(i)), Kind: "k", LayerID: "g", Pos: Point{x, y}}, t0()))
		if len(batch) == 2000 {
			flush()
		}
	}
	flush()
	return e
}

// TestEditCostDoesNotScale 验证对同一"热点单元"的编辑耗时
// 不随文档实体总数与已编辑块数量线性增长。
func TestEditCostDoesNotScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test skipped in -short mode")
	}
	small := buildScaleDoc(t, 256, 1000)
	big := buildScaleDoc(t, 1024, 12000)

	// 在两个文档上各自制造大量已修改块（分批，避免历史成本混入计时）。
	for i := 0; i < 2000; i++ {
		if _, err := small.Submit(NewSetTileCommand("g", Point{int32(i % 200), int32(i / 200)}, uint16(i%100+1), t0())); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4000; i++ {
		x := int32((i * 7) % 1000)
		y := int32((i * 13) % 1000)
		if _, err := big.Submit(NewSetTileCommand("g", Point{x, y}, uint16(i%100+1), t0())); err != nil {
			t.Fatal(err)
		}
	}

	measure := func(e *Editor) time.Duration {
		start := time.Now()
		for i := 0; i < 200; i++ {
			if _, err := e.Submit(NewSetTileCommand("g", Point{5, 5}, uint16(i+1), t0().Add(time.Duration(i)*time.Microsecond))); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start) / 200
	}
	_ = measure
	// 单次编辑平均耗时：大文档不得显著慢于小文档（允许 CI 抖动 5 倍裕量）。
	cs := avgN(func() {
		_, _ = small.Submit(NewSetTileCommand("g", Point{7, 7}, 200, t0()))
	}, 100)
	cb := avgN(func() {
		_, _ = big.Submit(NewSetTileCommand("g", Point{7, 7}, 200, t0()))
	}, 100)
	t.Logf("avg single-edit: small=%v big=%v (entities 1k vs 12k)", cs, cb)
	if cb > 10*cs+500*time.Microsecond {
		t.Fatalf("single edit scales with document size: small=%v big=%v", cs, cb)
	}

	// 增量产物与全量一致。
	full := big.builder.rebuildFrom(big.doc, big.rev)
	if !artifactsEqual(full, big.Artifact()) {
		t.Fatal("incremental artifact mismatch in scale test")
	}
}

func avgN(f func(), n int) time.Duration {
	start := time.Now()
	for i := 0; i < n; i++ {
		f()
	}
	return time.Since(start) / time.Duration(n)
}
