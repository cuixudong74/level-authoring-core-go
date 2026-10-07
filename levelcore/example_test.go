package levelcore_test

import (
	"fmt"
	"time"

	levelcore "github.com/cuixudong74/level-authoring-core-go/levelcore"
)

// ExampleOverview 演示：图层、地形、实体、约束拒绝、撤销与确定性打包。
func Example() {
	doc := levelcore.NewDocument(32, 32)
	ed, err := levelcore.NewEditor(doc, levelcore.DefaultConfig())
	if err != nil {
		panic(err)
	}
	ts := time.Unix(1_700_000_000, 0)
	if _, err := ed.Submit(levelcore.NewAddLayerCommand(
		levelcore.Layer{ID: "ground", Name: "Ground", Width: 32, Height: 32}, ts)); err != nil {
		panic(err)
	}
	if _, err := ed.Submit(levelcore.NewSetTileCommand(
		"ground", levelcore.Point{X: 4, Y: 5}, 9, ts)); err != nil {
		panic(err)
	}
	// Solid 实体占据同一格 -> 第二次摆放被拒绝（OVERLAP）。
	put := levelcore.NewPlaceEntityCommand(levelcore.Entity{
		ID: "hero", Kind: "npc", LayerID: "ground",
		Pos: levelcore.Point{X: 1, Y: 1}, Solid: true,
	}, ts)
	if _, err := ed.Submit(put); err != nil {
		panic(err)
	}
	_, err = ed.Submit(levelcore.NewPlaceEntityCommand(levelcore.Entity{
		ID: "foe", Kind: "npc", LayerID: "ground",
		Pos: levelcore.Point{X: 1, Y: 1}, Solid: true,
	}, ts))
	if re, ok := levelcore.AsReject(err); ok {
		fmt.Println("rejected:", re.Code)
	}

	v, _ := ed.Document().Tile("ground", levelcore.Point{X: 4, Y: 5})
	fmt.Println("tile before undo:", v)
	ed.Undo() // 撤销最近一次成功编辑：移除实体 hero
	_, hasHero := ed.Document().Entities["hero"]
	fmt.Println("hero present after undo:", hasHero)
	ed.Undo() // 再撤销：清掉地形
	v, _ = ed.Document().Tile("ground", levelcore.Point{X: 4, Y: 5})
	fmt.Println("tile after second undo:", v)

	ed.Redo()
	ed.Redo()
	pkg, err := levelcore.LevelPack(ed.Document(), ed.Artifact())
	if err != nil {
		panic(err)
	}
	_, _, err = levelcore.LevelUnpack(pkg)
	fmt.Println("pack bytes:", len(pkg) > 0, "roundtrip err:", err)
	// Output:
	// rejected: OVERLAP
	// tile before undo: 9
	// hero present after undo: false
	// tile after second undo: 0
	// pack bytes: true roundtrip err: <nil>
}
