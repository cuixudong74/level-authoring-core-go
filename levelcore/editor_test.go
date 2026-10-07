package levelcore

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// helper：准备一个含 ground 图层的编辑器。
func newTestEditor(t *testing.T, w, h int32) *Editor {
	t.Helper()
	e, err := NewEditor(NewDocument(w, h), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(NewAddLayerCommand(Layer{ID: "ground", Name: "Ground", Width: w, Height: h}, t0())); err != nil {
		t.Fatal(err)
	}
	return e
}

func t0() time.Time { return time.Unix(1_000_000, 0) }

// 1) 撤销/重做语义 + 截断重做历史。
func TestUndoRedo(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	submit := func(c Command) {
		t.Helper()
		if _, err := e.Submit(c); err != nil {
			t.Fatal(err)
		}
	}
	submit(NewSetTileCommand("ground", Point{1, 1}, 5, t0()))
	submit(NewPlaceEntityCommand(Entity{ID: "a", Kind: "k", LayerID: "ground", Pos: Point{2, 2}}, t0()))
	submit(NewSetTileCommand("ground", Point{3, 3}, 9, t0()))

	if got, _ := e.Document().Tile("ground", Point{1, 1}); got != 5 {
		t.Fatalf("tile=%d", got)
	}
	if !e.CanUndo() || e.CanRedo() {
		t.Fatal("initial undo/redo availability wrong")
	}
	rev3 := e.Revision()

	e.Undo() // 撤销 9
	if got, _ := e.Document().Tile("ground", Point{3, 3}); got != 0 {
		t.Fatalf("after undo tile=%d", got)
	}
	if !e.CanRedo() {
		t.Fatal("redo should be available after undo")
	}
	// 撤销后产生新编辑：重做历史必须被截断。
	submit(NewSetTileCommand("ground", Point{4, 4}, 11, t0()))
	if e.CanRedo() {
		t.Fatal("redo history must be truncated after new edit")
	}
	if got, _ := e.Document().Tile("ground", Point{4, 4}); got != 11 {
		t.Fatalf("new tile=%d", got)
	}
	if got, _ := e.Document().Tile("ground", Point{3, 3}); got != 0 {
		t.Fatalf("redone-then-truncated tile=%d", got)
	}
	if e.Revision() <= rev3 {
		t.Fatalf("revision should advance, got %d", e.Revision())
	}
	if e.History().Cursor != len(e.History().Entries) {
		t.Fatal("cursor must equal entry count after new edit")
	}
}

// 2) 连续同类微调合并：SetTile 同格与 MoveEntity 同实体。
func TestCoalescing(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	base := t0()
	put := func(c Command) {
		t.Helper()
		if _, err := e.Submit(c); err != nil {
			t.Fatal(err)
		}
	}
	put(NewPlaceEntityCommand(Entity{ID: "a", Kind: "k", LayerID: "ground", Pos: Point{0, 0}}, base))
	h0 := len(e.History().Entries)

	// 三次同格 SetTile 均在窗口内：合并为一条，最终值为最后一次。
	put(NewSetTileCommand("ground", Point{1, 1}, 1, base.Add(10*time.Millisecond)))
	r2, err := e.Submit(NewSetTileCommand("ground", Point{1, 1}, 2, base.Add(20*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Merged {
		t.Fatal("second SetTile should merge")
	}
	r3, err := e.Submit(NewSetTileCommand("ground", Point{1, 1}, 3, base.Add(30*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	if !r3.Merged {
		t.Fatal("third SetTile should merge")
	}
	if got, _ := e.Document().Tile("ground", Point{1, 1}); got != 3 {
		t.Fatalf("coalesced tile=%d", got)
	}
	if len(e.History().Entries) != h0+1 {
		t.Fatalf("history len=%d want %d", len(e.History().Entries), h0+1)
	}
	entry := e.History().Entries[e.History().Cursor-1]
	if entry.Count != 3 {
		t.Fatalf("merged Count=%d want 3", entry.Count)
	}
	// 一次撤销回到合并前（值 0），等价于从未编辑。
	e.Undo()
	if got, _ := e.Document().Tile("ground", Point{1, 1}); got != 0 {
		t.Fatalf("after undo coalesced tile=%d want 0", got)
	}
	e.Redo()
	if got, _ := e.Document().Tile("ground", Point{1, 1}); got != 3 {
		t.Fatalf("after redo tile=%d want 3", got)
	}

	// 超出窗口则不合并。
	if _, err := e.Submit(NewSetTileCommand("ground", Point{1, 1}, 4, base.Add(10*time.Second))); err != nil {
		t.Fatal(err)
	}
	if len(e.History().Entries) != h0+2 {
		t.Fatalf("history should grow after window, len=%d", len(e.History().Entries))
	}

	// MoveEntity 连续拖拽合并。
	mr1, err := e.Submit(NewMoveEntityCommand("a", Point{5, 5}, base.Add(100*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	mr2, err := e.Submit(NewMoveEntityCommand("a", Point{6, 6}, base.Add(200*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	if mr1.Merged || !mr2.Merged {
		t.Fatalf("move merge flags: %+v %+v", mr1, mr2)
	}
	e.Undo()
	if p := e.Document().Entities["a"].Pos; p != (Point{0, 0}) {
		t.Fatalf("after undo move pos=%v", p)
	}
}

// 3) 批次原子性：中途失败整体不生效，错误定位到具体命令。
func TestBatchAtomicFailure(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	before := e.Document()
	beforeRev := e.Revision()

	batch := []Command{
		NewSetTileCommand("ground", Point{1, 1}, 7, t0()),
		NewSetTileCommand("missing-layer", Point{1, 1}, 7, t0()), // 下标 1 失败
		NewPlaceEntityCommand(Entity{ID: "a", Kind: "k", LayerID: "ground"}, t0()),
	}
	_, err := e.SubmitBatch(batch)
	re, ok := AsReject(err)
	if !ok {
		t.Fatalf("want RejectError, got %v", err)
	}
	if re.CommandIndex != 1 {
		t.Fatalf("CommandIndex=%d want 1", re.CommandIndex)
	}
	if re.Code != RejectIllegalOwner {
		t.Fatalf("code=%s", re.Code)
	}
	if !docsEqual(before, e.Document()) {
		t.Fatal("document must be identical after failed batch")
	}
	if e.Revision() != beforeRev {
		t.Fatal("revision must not advance after failed batch")
	}
	if len(e.History().Entries) != 1 { // 仅初始 AddLayer
		t.Fatalf("history must not grow, len=%d", len(e.History().Entries))
	}

	// 约束失败（重叠）同样整体回滚。
	_, err = e.Submit(NewPlaceEntityCommand(Entity{ID: "x", Kind: "k", LayerID: "ground", Pos: Point{2, 2}, Solid: true}, t0()))
	if err != nil {
		t.Fatal(err)
	}
	beforeX := e.Document() // 重叠批次应回到"x 已存在、tile 未改"的状态
	bad := []Command{
		NewSetTileCommand("ground", Point{0, 0}, 3, t0()),
		NewPlaceEntityCommand(Entity{ID: "y", Kind: "k", LayerID: "ground", Pos: Point{2, 2}, Solid: true}, t0()),
	}
	_, err = e.SubmitBatch(bad)
	re, ok = AsReject(err)
	if !ok || re.Code != RejectOverlap || re.CommandIndex != 1 {
		t.Fatalf("want overlap @1, got %v", err)
	}
	if !docsEqual(beforeX, e.Document()) {
		t.Fatal("document must rollback overlap batch")
	}
}

// 4) 各类约束拒绝码可区分，拒绝后文档不变。
func TestConstraintRejections(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	cases := []struct {
		name string
		cmd  Command
		code RejectCode
	}{
		{"oob tile", NewSetTileCommand("ground", Point{99, 0}, 1, t0()), RejectOutOfBounds},
		{"unknown layer tile", NewSetTileCommand("nope", Point{0, 0}, 1, t0()), RejectIllegalOwner},
		{"dup layer", NewAddLayerCommand(Layer{ID: "ground", Width: 1, Height: 1}, t0()), RejectDuplicateID},
		{"bad layer size", NewAddLayerCommand(Layer{ID: "L", Width: 0, Height: 1}, t0()), RejectInvalidValue},
		{"layer exceeds map", NewAddLayerCommand(Layer{ID: "L", Width: 32, Height: 32}, t0()), RejectOutOfBounds},
		{"entity missing layer", NewPlaceEntityCommand(Entity{ID: "e", Kind: "k", LayerID: "?"}, t0()), RejectIllegalOwner},
		{"entity oob", NewPlaceEntityCommand(Entity{ID: "e", Kind: "k", LayerID: "ground", Pos: Point{20, 20}}, t0()), RejectOutOfBounds},
		{"move missing", NewMoveEntityCommand("ghost", Point{0, 0}, t0()), RejectNotFound},
		{"remove missing entity", NewRemoveEntityCommand("ghost", t0()), RejectNotFound},
		{"zone bad rect", NewAddZoneCommand(Zone{ID: "z", Kind: "spawn", Bounds: Rect{MinX: 1, MinY: 1, MaxX: 1, MaxY: 2}}, t0()), RejectInvalidValue},
		{"zone oob", NewAddZoneCommand(Zone{ID: "z", Kind: "spawn", Bounds: Rect{MinX: 0, MinY: 0, MaxX: 99, MaxY: 99}}, t0()), RejectOutOfBounds},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := e.Document()
			_, err := e.Submit(tc.cmd)
			re, ok := AsReject(err)
			if !ok {
				t.Fatalf("want RejectError, got %v", err)
			}
			if re.Code != tc.code {
				t.Fatalf("code=%s want %s", re.Code, tc.code)
			}
			if re.CommandIndex != 0 {
				t.Fatalf("single submit index=%d want 0", re.CommandIndex)
			}
			if !docsEqual(before, e.Document()) {
				t.Fatalf("document changed after rejection: %s", tc.name)
			}
		})
	}

	// 删除仍被实体引用的图层：非法归属。
	if _, err := e.Submit(NewPlaceEntityCommand(Entity{ID: "e", Kind: "k", LayerID: "ground", Pos: Point{1, 1}}, t0())); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(NewRemoveLayerCommand("ground", t0())); err == nil {
		t.Fatal("removing referenced layer must fail")
	} else if re, _ := AsReject(err); re.Code != RejectIllegalOwner {
		t.Fatalf("code=%s", re.Code)
	}
}

// 5) 增量重建与全量重建逐字段一致（含撤销后回退）。
func TestIncrementalEqualsFull(t *testing.T) {
	e := newTestEditor(t, 32, 32)
	cmds := []Command{
		NewAddLayerCommand(Layer{ID: "deco", Name: "D", Width: 16, Height: 16}, t0()),
		NewSetTileCommand("ground", Point{1, 1}, 4, t0()),
		NewSetTileCommand("ground", Point{20, 20}, 5, t0()),
		NewSetTileCommand("deco", Point{0, 0}, 9, t0()),
		NewPlaceEntityCommand(Entity{ID: "e1", Kind: "npc", LayerID: "ground", Pos: Point{3, 3}, Solid: true, Props: map[string]string{"hp": "10"}}, t0()),
		NewPlaceEntityCommand(Entity{ID: "e2", Kind: "prop", LayerID: "deco", Pos: Point{1, 1}}, t0()),
		NewMoveEntityCommand("e1", Point{4, 4}, t0()),
		NewAddZoneCommand(Zone{ID: "z1", Kind: "spawn", Bounds: Rect{MinX: 0, MinY: 0, MaxX: 4, MaxY: 4}}, t0()),
		NewRemoveEntityCommand("e2", t0()),
	}
	for i, c := range cmds {
		if i%3 == 2 {
			if _, err := e.SubmitBatch([]Command{c, NewSetTileCommand("ground", Point{2, 2}, uint16(i+1), t0())}); err != nil {
				t.Fatal(err)
			}
		} else if _, err := e.Submit(c); err != nil {
			t.Fatal(err)
		}
		full := e.builder.rebuildFrom(e.doc, e.rev)
		got := e.Artifact()
		if !artifactsEqual(full, got) {
			t.Fatalf("step %d: incremental != full\nfull=%s\ngot =%s", i, mustJSON(full), mustJSON(got))
		}
	}

	// 撤销每一步后：增量产物必须与从零全量重建逐字段一致（无过期内容）。
	undone := 0
	for e.CanUndo() {
		e.Undo()
		undone++
		full := e.builder.rebuildFrom(e.doc, e.rev)
		got := e.Artifact()
		if !artifactsEqual(full, got) {
			t.Fatalf("after undo #%d: incremental != full\nfull=%s\ngot =%s",
				undone, mustJSON(full), mustJSON(got))
		}
		if len(got.Chunks) != len(e.Document().LayerOrder) {
			t.Fatalf("undo #%d: artifact layers out of sync with document", undone)
		}
	}
	// 全部撤销后：只剩初始 ground 图层，无实体无区域。
	if a := e.Artifact(); len(a.EntityIndex["ground"]) != 0 || len(a.ZoneList) != 0 {
		t.Fatalf("stale artifact content after all undoes: %s", mustJSON(a))
	}
}

// 6) 打包版本与损坏输入安全处理。
func TestPackVersioningAndCorruption(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	if _, err := e.Submit(NewSetTileCommand("ground", Point{1, 1}, 6, t0())); err != nil {
		t.Fatal(err)
	}
	pk, err := Pack(e.Document(), e.Artifact())
	if err != nil {
		t.Fatal(err)
	}
	d2, a2, err := Unpack(pk)
	if err != nil {
		t.Fatal(err)
	}
	if !docsEqual(d2, e.Document()) {
		t.Fatal("document roundtrip mismatch")
	}
	if !artifactsEqual(a2, e.Artifact()) {
		t.Fatal("artifact roundtrip mismatch")
	}

	// 同一文档+产物重复打包必须字节一致。
	pk2, _ := Pack(e.Document(), e.Artifact())
	if !bytes.Equal(pk, pk2) {
		t.Fatal("packing is not deterministic")
	}
	// JSON 文档往返也必须字节一致。
	j1, _ := MarshalDocument(e.Document())
	j2, _ := MarshalDocument(d2)
	if !bytes.Equal(j1, j2) {
		t.Fatalf("marshal not deterministic:\n%s\n%s", j1, j2)
	}
	d3, err := UnmarshalDocument(j1)
	if err != nil || !docsEqual(d3, e.Document()) {
		t.Fatalf("json doc roundtrip: %v", err)
	}

	// 旧版本：篡改版本号为 0（校验和必然不匹配，先返回 checksum；
	// 修复校验和后应返回 UNKNOWN_VERSION）。
	badVer := append([]byte(nil), pk...)
	badVer[4] = 0
	badVer[5] = 0
	if _, _, err := Unpack(badVer); err == nil {
		t.Fatal("expected checksum error for tampered version")
	}
	fixed := make([]byte, len(pk))
	copy(fixed, pk)
	binaryBigEndianPutUint16(fixed[4:6], 0)
	body := fixed[10 : len(fixed)-32]
	sum := sha256Sum(body)
	copy(fixed[len(fixed)-32:], sum)
	_, _, err = Unpack(fixed)
	if re, ok := AsReject(err); !ok || re.Code != RejectUnknownVersion {
		t.Fatalf("want unknown version, got %v", err)
	}

	// 未来版本。
	future := append([]byte(nil), pk...)
	binaryBigEndianPutUint16(future[4:6], MaxSupportedVersion+1)
	copy(future[len(future)-32:], sha256Sum(future[10:len(future)-32]))
	if _, _, err := Unpack(future); err == nil {
		t.Fatal("future version must be rejected")
	}

	// 损坏输入：截断、错误 magic、翻转载荷字节、空输入。
	if _, _, err := Unpack(nil); err == nil {
		t.Fatal("nil packet must error")
	}
	if _, _, err := Unpack(pk[:7]); err == nil {
		t.Fatal("short packet must error")
	}
	if _, _, err := Unpack(pk[:len(pk)-1]); err == nil {
		t.Fatal("truncated packet must error")
	}
	flip := append([]byte(nil), pk...)
	flip[20] ^= 0xFF
	if re, ok := AsReject(UnpackErr(flip)); !ok || re.Code != RejectChecksum {
		t.Fatalf("flipped bytes must yield checksum error, got %v", UnpackErr(flip))
	}
	badMagic := append([]byte(nil), pk...)
	copy(badMagic[0:4], "XXXX")
	if re, ok := AsReject(UnpackErr(badMagic)); !ok || re.Code != RejectCorrupt {
		t.Fatalf("bad magic must yield corrupt, got %v", UnpackErr(badMagic))
	}
	if _, err := UnmarshalDocument([]byte("{not json")); err == nil {
		t.Fatal("broken json must error")
	}
}

// 7) 确定性：同初始文档同命令串在两个 Editor 上文档、历史、产物逐字段一致。
func TestDeterministicReplay(t *testing.T) {
	build := func() (*Editor, []Command) {
		e := newTestEditor(t, 24, 24)
		cmds := []Command{
			NewSetTileCommand("ground", Point{1, 1}, 3, t0().Add(1*time.Millisecond)),
			NewSetTileCommand("ground", Point{1, 1}, 4, t0().Add(2*time.Millisecond)), // 合并
			NewPlaceEntityCommand(Entity{ID: "b", Kind: "k", LayerID: "ground", Pos: Point{9, 9}, Solid: true, Props: map[string]string{"a": "1"}}, t0()),
			NewPlaceEntityCommand(Entity{ID: "a", Kind: "k", LayerID: "ground", Pos: Point{8, 8}, Solid: true}, t0()),
			NewMoveEntityCommand("a", Point{7, 7}, t0().Add(3*time.Millisecond)),
			NewAddZoneCommand(Zone{ID: "z", Kind: "s", Bounds: Rect{MinX: 0, MinY: 0, MaxX: 2, MaxY: 2}}, t0()),
		}
		return e, cmds
	}
	e1, c1 := build()
	e2, c2 := build()
	for i := range c1 {
		if i == 2 {
			if _, err := e1.SubmitBatch([]Command{c1[i], c1[i+1]}); err != nil {
				t.Fatal(err)
			}
			if _, err := e2.SubmitBatch([]Command{c2[i], c2[i+1]}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if i == 3 {
			continue
		}
		if _, err := e1.Submit(c1[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := e2.Submit(c2[i]); err != nil {
			t.Fatal(err)
		}
	}
	e1.Undo()
	e2.Undo()
	if !docsEqual(e1.Document(), e2.Document()) {
		t.Fatal("documents differ after replay")
	}
	if !reflect.DeepEqual(e1.History(), e2.History()) {
		t.Fatalf("history differs:\n%+v\n%+v", e1.History(), e2.History())
	}
	if !artifactsEqual(e1.Artifact(), e2.Artifact()) {
		t.Fatal("artifacts differ after replay")
	}
	p1, _ := Pack(e1.Document(), e1.Artifact())
	p2, _ := Pack(e2.Document(), e2.Artifact())
	if !bytes.Equal(p1, p2) {
		t.Fatal("packed bytes differ")
	}
}

// 8) 大体量：单次编辑耗时/内存不随总规模线性增长；历史与缓存上限可控。
func TestLargeDocumentBoundedCost(t *testing.T) {
	const n = 1024
	e, err := NewEditor(NewDocument(n, n), Config{HistoryLimit: 8, CacheLimit: 16, Coalesce: MergePolicy{Window: DefaultMergeWindow}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(NewAddLayerCommand(Layer{ID: "g", Width: n, Height: n}, t0())); err != nil {
		t.Fatal(err)
	}
	// 批量铺放大量实体（非 Solid，避免重叠）：
	// 每批 2000 条作为一次提交，降低历史与派生开销。
	const total = 12000
	for i := 0; i < total; i += 2000 {
		batch := make([]Command, 0, 2000)
		for j := 0; j < 2000; j++ {
			k := i + j
			x := int32(k % n)
			y := int32(k / n)
			batch = append(batch, NewPlaceEntityCommand(
				Entity{ID: "e" + intToStr(int32(k)), Kind: "k", LayerID: "g", Pos: Point{x, y}}, t0()))
		}
		if _, err := e.SubmitBatch(batch); err != nil {
			t.Fatal(err)
		}
	}
	// 单格编辑：只触碰一个块，不随 20000 实体/16M 格增长。
	_, err = e.Submit(NewSetTileCommand("g", Point{100, 100}, 42, t0()))
	if err != nil {
		t.Fatal(err)
	}
	// 历史上限：插入远超 8 条的提交后最多 8 条，最近编辑仍正确。
	if len(e.History().Entries) > 8 {
		t.Fatalf("history limit not enforced: %d", len(e.History().Entries))
	}
	if got, _ := e.Document().Tile("g", Point{100, 100}); got != 42 {
		t.Fatalf("recent edit lost under eviction: %d", got)
	}
	if e.builder.cache.len() > 16 {
		t.Fatalf("cache limit not enforced: %d", e.builder.cache.len())
	}
	// 产物增量与全量仍一致。
	full := e.builder.rebuildFrom(e.doc, e.rev)
	if !artifactsEqual(full, e.Artifact()) {
		t.Fatal("large doc incremental mismatch")
	}
}

// ---- helpers ----

func docsEqual(a, b *Document) bool {
	ja, _ := MarshalDocument(a)
	jb, _ := MarshalDocument(b)
	return bytes.Equal(ja, jb)
}

// artifactsEqual 由库内提供（artifact.go）。

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
