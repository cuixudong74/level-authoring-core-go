package levelcore

import (
	"fmt"
	"time"
)

// Command 描述一次可应用、可撤销的编辑。
// 实现均为本包内的具体类型；外部通过构造函数创建。
type Command interface {
	commandTag()
	// kind 返回稳定的命令种类名，用于历史记录与跨机器对齐。
	kind() string
	// describe 返回确定性的命令摘要（不含时间戳等环境信息）。
	describe() string
	// canMergeTo 判断 other 是否应并入当前命令（连续同类微调合并）。
	canMergeTo(other Command, policy MergePolicy) bool
	// merge 把 other 合并进自身；必须保留最早的逆操作信息。
	merge(other Command)
	// apply 在文档上执行结构性变更并记录逆操作所需信息；
	// 仅做与最终状态无关的结构性前置校验，跨对象约束由 verify 负责。
	apply(d *Document) *RejectError
	// undo 恢复 apply 之前的状态。
	undo(d *Document)
	// verify 在"整批命令全部应用之后的最终文档"上校验该命令的约束，
	// 返回的 RejectError 的 CommandIndex 由 Editor 统一填写。
	verify(d *Document) *RejectError
}

// MergePolicy 定义连续同类微调的合并策略。
type MergePolicy struct {
	// Window 为合并时间窗；<=0 表示禁用合并。时间戳来自提交方，
	// 不读取本机墙钟，因此结果与历史在任意机器上逐字段一致。
	Window time.Duration
}

// DefaultMergeWindow 是默认合并窗口：500ms。
const DefaultMergeWindow = 500 * time.Millisecond

func withinWindow(a, b time.Time, w time.Duration) bool {
	if w <= 0 {
		return false
	}
	diff := b.Sub(a)
	return diff >= 0 && diff <= w
}

// ---- 地形 ----

// SetTileCommand 设置某层某坐标的地形单元值（0 表示清除）。
type SetTileCommand struct {
	LayerID string
	Pos     Point
	NewVal  uint16
	Ts      time.Time
	oldVal  uint16
	hadVal  bool
	// oldChunk 保存应用前整块的拷贝（nil 表示该块此前不存在），
	// 撤销时整块还原，避免在派生覆盖节点上遮蔽墓碑。
	oldChunk *tileChunk
}

// NewSetTileCommand 构造地形单元编辑命令。
func NewSetTileCommand(layerID string, p Point, v uint16, ts time.Time) *SetTileCommand {
	return &SetTileCommand{LayerID: layerID, Pos: p, NewVal: v, Ts: ts}
}

func (*SetTileCommand) commandTag()  {}
func (*SetTileCommand) kind() string { return "SetTile" }
func (c *SetTileCommand) describe() string {
	return fmt.Sprintf("SetTile layer=%q cell=(%d,%d) value=%d", c.LayerID, c.Pos.X, c.Pos.Y, c.NewVal)
}
func (c *SetTileCommand) canMergeTo(o Command, p MergePolicy) bool {
	n, ok := o.(*SetTileCommand)
	if !ok || n.LayerID != c.LayerID || n.Pos != c.Pos {
		return false
	}
	return withinWindow(c.Ts, n.Ts, p.Window)
}
func (c *SetTileCommand) merge(o Command) {
	n := o.(*SetTileCommand)
	c.NewVal = n.NewVal // 保留首次记录的 oldVal/hadVal 作为整条合并的逆操作
	c.Ts = n.Ts
}
func (c *SetTileCommand) apply(d *Document) *RejectError {
	l, ok := d.Layers[c.LayerID]
	if !ok {
		return rejectf(-1, RejectIllegalOwner, "set tile on unknown layer %q", c.LayerID)
	}
	if c.Pos.X < 0 || c.Pos.Y < 0 || c.Pos.X >= l.Width || c.Pos.Y >= l.Height {
		return rejectf(-1, RejectOutOfBounds, "cell (%d,%d) outside layer %q size %dx%d",
			c.Pos.X, c.Pos.Y, c.LayerID, l.Width, l.Height)
	}
	c.oldVal, c.hadVal = d.Tile(c.LayerID, c.Pos)
	d.forkTiles()
	d.SetTile(c.LayerID, c.Pos, c.NewVal)
	d.markTileAt(c.LayerID, c.Pos)
	return nil
}
func (c *SetTileCommand) undo(d *Document) {
	d.forkTiles()
	d.restoreChunk(c.LayerID, c.Pos, c.oldChunk)
	d.markTileAt(c.LayerID, c.Pos)
}
func (c *SetTileCommand) verify(d *Document) *RejectError { return nil }

// ---- 图层 ----

// AddLayerCommand 新增一个地形层（含尺寸）。
type AddLayerCommand struct {
	Layer Layer
	Ts    time.Time
}

// NewAddLayerCommand 构造新增图层命令。
func NewAddLayerCommand(l Layer, ts time.Time) *AddLayerCommand {
	return &AddLayerCommand{Layer: l, Ts: ts}
}

func (*AddLayerCommand) commandTag()  {}
func (*AddLayerCommand) kind() string { return "AddLayer" }
func (c *AddLayerCommand) describe() string {
	return fmt.Sprintf("AddLayer id=%q name=%q size=%dx%d", c.Layer.ID, c.Layer.Name, c.Layer.Width, c.Layer.Height)
}
func (*AddLayerCommand) canMergeTo(Command, MergePolicy) bool { return false }
func (*AddLayerCommand) merge(Command)                        {}
func (c *AddLayerCommand) apply(d *Document) *RejectError {
	l := c.Layer
	if l.ID == "" {
		return rejectf(-1, RejectInvalidValue, "layer id must not be empty")
	}
	if l.Width <= 0 || l.Height <= 0 {
		return rejectf(-1, RejectInvalidValue, "layer %q size must be positive, got %dx%d", l.ID, l.Width, l.Height)
	}
	if l.Width > d.MapWidth || l.Height > d.MapHeight {
		return rejectf(-1, RejectOutOfBounds, "layer %q size %dx%d exceeds map %dx%d",
			l.ID, l.Width, l.Height, d.MapWidth, d.MapHeight)
	}
	if _, dup := d.Layers[l.ID]; dup {
		return rejectf(-1, RejectDuplicateID, "layer id %q already exists", l.ID)
	}
	d.forkStructs()
	d.forkTiles()
	lc := l
	d.Layers[l.ID] = &lc
	d.LayerOrder = append(d.LayerOrder, l.ID)
	d.tiles[l.ID] = &tileState{pages: map[uint64]*tilePage{}}
	d.markLayerAdd(l.ID)
	for cy := int32(0); cy < (l.Height+ChunkSize-1)/ChunkSize; cy++ {
		for cx := int32(0); cx < (l.Width+ChunkSize-1)/ChunkSize; cx++ {
			d.markTileIfActive(l.ID, uint32(cx), uint32(cy))
		}
	}
	return nil
}
func (c *AddLayerCommand) undo(d *Document) {
	d.markLayerRemove(c.Layer.ID)
	d.forkStructs()
	d.forkTiles()
	delete(d.tiles, c.Layer.ID)
	delete(d.Layers, c.Layer.ID)
	for i, id := range d.LayerOrder {
		if id == c.Layer.ID {
			d.LayerOrder = append(d.LayerOrder[:i], d.LayerOrder[i+1:]...)
			break
		}
	}
}
func (c *AddLayerCommand) verify(d *Document) *RejectError { return nil }

// RemoveLayerCommand 删除图层及其单元；最终文档中不得有实体仍归属该层。
type RemoveLayerCommand struct {
	LayerID  string
	Ts       time.Time
	oldLayer Layer
	oldState *tileState
	orderAt  int
}

// NewRemoveLayerCommand 构造删除图层命令。
func NewRemoveLayerCommand(layerID string, ts time.Time) *RemoveLayerCommand {
	return &RemoveLayerCommand{LayerID: layerID, Ts: ts, orderAt: -1}
}

func (*RemoveLayerCommand) commandTag()  {}
func (*RemoveLayerCommand) kind() string { return "RemoveLayer" }
func (c *RemoveLayerCommand) describe() string {
	return fmt.Sprintf("RemoveLayer id=%q", c.LayerID)
}
func (*RemoveLayerCommand) canMergeTo(Command, MergePolicy) bool { return false }
func (*RemoveLayerCommand) merge(Command)                        {}
func (c *RemoveLayerCommand) apply(d *Document) *RejectError {
	l, ok := d.Layers[c.LayerID]
	if !ok {
		return rejectf(-1, RejectNotFound, "layer %q does not exist", c.LayerID)
	}
	d.forkStructs()
	d.forkTiles()
	c.oldLayer = *l
	// tileState 为不可变持久化节点：移除图层时保存该节点，
	// 撤销删图层时原样挂回，不复制任何块。
	c.oldState = d.tiles[c.LayerID]
	for i, id := range d.LayerOrder {
		if id == c.LayerID {
			c.orderAt = i
			d.LayerOrder = append(d.LayerOrder[:i], d.LayerOrder[i+1:]...)
			break
		}
	}
	delete(d.Layers, c.LayerID)
	delete(d.tiles, c.LayerID)
	d.markLayerRemove(c.LayerID)
	return nil
}
func (c *RemoveLayerCommand) undo(d *Document) {
	d.forkStructs()
	d.forkTiles()
	l := c.oldLayer
	d.Layers[c.LayerID] = &l
	if c.oldState != nil {
		d.tiles[c.LayerID] = c.oldState
	} else {
		d.tiles[c.LayerID] = &tileState{pages: map[uint64]*tilePage{}}
	}
	d.markLayerAdd(c.LayerID)
	if st := d.tiles[c.LayerID]; st != nil {
		st.iterChunksMark(d, c.LayerID)
	}
	if c.orderAt >= 0 && c.orderAt <= len(d.LayerOrder) {
		d.LayerOrder = append(d.LayerOrder, "")
		copy(d.LayerOrder[c.orderAt+1:], d.LayerOrder[c.orderAt:])
		d.LayerOrder[c.orderAt] = c.LayerID
	} else {
		d.LayerOrder = append(d.LayerOrder, c.LayerID)
	}
}
func (c *RemoveLayerCommand) verify(d *Document) *RejectError {
	if n := d.layerEntities[c.LayerID]; n > 0 {
		return rejectf(-1, RejectIllegalOwner,
			"cannot remove layer %q: %d entities still belong to it", c.LayerID, n)
	}
	return nil
}

// ---- 实体 ----

// PlaceEntityCommand 摆放一个实体。
type PlaceEntityCommand struct {
	Entity      Entity
	Ts          time.Time
	priorHolder string
	hadHolder   bool
}

// NewPlaceEntityCommand 构造实体摆放命令。
func NewPlaceEntityCommand(e Entity, ts time.Time) *PlaceEntityCommand {
	return &PlaceEntityCommand{Entity: e, Ts: ts}
}

func (*PlaceEntityCommand) commandTag()  {}
func (*PlaceEntityCommand) kind() string { return "PlaceEntity" }
func (c *PlaceEntityCommand) describe() string {
	return fmt.Sprintf("PlaceEntity id=%q kind=%q layer=%q at=(%d,%d) solid=%t",
		c.Entity.ID, c.Entity.Kind, c.Entity.LayerID, c.Entity.Pos.X, c.Entity.Pos.Y, c.Entity.Solid)
}
func (*PlaceEntityCommand) canMergeTo(Command, MergePolicy) bool { return false }
func (*PlaceEntityCommand) merge(Command)                        {}
func (c *PlaceEntityCommand) apply(d *Document) *RejectError {
	e := c.Entity
	if e.ID == "" {
		return rejectf(-1, RejectInvalidValue, "entity id must not be empty")
	}
	if e.Kind == "" {
		return rejectf(-1, RejectInvalidValue, "entity %q kind must not be empty", e.ID)
	}
	if _, dup := d.Entities[e.ID]; dup {
		return rejectf(-1, RejectDuplicateID, "entity id %q already exists", e.ID)
	}
	if _, ok := d.Layers[e.LayerID]; !ok {
		return rejectf(-1, RejectIllegalOwner, "entity %q refers to unknown layer %q", e.ID, e.LayerID)
	}
	k := occKey(e.LayerID, e.Pos)
	if h, ok := d.occ[k]; ok {
		c.priorHolder = h
		c.hadHolder = true
	}
	d.forkStructs()
	ec := e
	if ec.Props != nil {
		ec.Props = cloneProps(e.Props)
	}
	d.Entities[e.ID] = &ec
	d.indexAdd(&ec)
	d.markEntPlaced(e.ID, e.LayerID)
	return nil
}
func (c *PlaceEntityCommand) undo(d *Document) {
	d.forkStructs()
	delete(d.Entities, c.Entity.ID)
	// 先恢复占用键的旧映射，再递减图层计数（indexRemove 的 occ 删除
	// 会误伤同格的既有占用者，因此这里只借用它的计数逻辑）。
	if c.Entity.Solid {
		k := occKey(c.Entity.LayerID, c.Entity.Pos)
		if c.hadHolder {
			d.occ[k] = c.priorHolder
		} else {
			delete(d.occ, k)
		}
	}
	d.decLayerCount(c.Entity.LayerID)
	d.markEntRemoved(c.Entity.ID, c.Entity.LayerID)
}
func (c *PlaceEntityCommand) verify(d *Document) *RejectError {
	if err := verifyEntityBounds(d, &c.Entity); err != nil {
		return err
	}
	if c.Entity.Solid && c.hadHolder {
		return rejectf(-1, RejectOverlap,
			"entity %q overlaps solid entity %q on layer %q at (%d,%d)",
			c.Entity.ID, c.priorHolder, c.Entity.LayerID, c.Entity.Pos.X, c.Entity.Pos.Y)
	}
	return nil
}

// MoveEntityCommand 移动实体；连续拖拽（同实体、时间窗内）合并为一条。
type MoveEntityCommand struct {
	EntityID string
	To       Point
	Ts       time.Time
	from     Point
	gotFrom  bool
	// 应用瞬间两个占用键的旧映射，供撤销时精确恢复。
	toPrev, fromPrev string
	toHad, fromHad   bool
}

// NewMoveEntityCommand 构造实体移动命令。
func NewMoveEntityCommand(entityID string, to Point, ts time.Time) *MoveEntityCommand {
	return &MoveEntityCommand{EntityID: entityID, To: to, Ts: ts}
}

func (*MoveEntityCommand) commandTag()  {}
func (*MoveEntityCommand) kind() string { return "MoveEntity" }
func (c *MoveEntityCommand) describe() string {
	return fmt.Sprintf("MoveEntity id=%q to=(%d,%d)", c.EntityID, c.To.X, c.To.Y)
}
func (c *MoveEntityCommand) canMergeTo(o Command, p MergePolicy) bool {
	n, ok := o.(*MoveEntityCommand)
	if !ok || n.EntityID != c.EntityID {
		return false
	}
	return withinWindow(c.Ts, n.Ts, p.Window)
}
func (c *MoveEntityCommand) merge(o Command) {
	n := o.(*MoveEntityCommand)
	c.To = n.To // 保留首次起点作为整条合并的逆操作
	c.Ts = n.Ts
}
func (c *MoveEntityCommand) apply(d *Document) *RejectError {
	e, ok := d.Entities[c.EntityID]
	if !ok {
		return rejectf(-1, RejectNotFound, "entity %q does not exist", c.EntityID)
	}
	d.forkStructs()
	if !c.gotFrom {
		c.from = e.Pos
		c.gotFrom = true
	}
	e = d.mutableEntity(c.EntityID)
	// 捕获 from/to 两格在移动前的占用映射。
	kf := occKey(e.LayerID, c.from)
	if h, ok := d.occ[kf]; ok {
		c.fromPrev, c.fromHad = h, true
	}
	kt := occKey(e.LayerID, c.To)
	if h, ok := d.occ[kt]; ok && h != e.ID {
		c.toPrev, c.toHad = h, true
	}
	d.indexRemove(e)
	e.Pos = c.To
	d.indexAdd(e)
	d.markMoved(c.EntityID, e.LayerID)
	return nil
}
func (c *MoveEntityCommand) undo(d *Document) {
	d.forkStructs()
	d.forkIndexes()
	e := d.mutableEntity(c.EntityID)
	// 恢复两个占用键：from 回到移动前映射（即自身），to 回到其旧占用者。
	if e.Solid {
		kf := occKey(e.LayerID, c.from)
		if c.fromHad {
			d.occ[kf] = c.fromPrev
		} else {
			d.occ[kf] = e.ID
		}
		kt := occKey(e.LayerID, c.To)
		if c.toHad {
			d.occ[kt] = c.toPrev
		} else {
			delete(d.occ, kt)
		}
	}
	e.Pos = c.from
	d.markMoved(c.EntityID, e.LayerID)
}
func (c *MoveEntityCommand) verify(d *Document) *RejectError {
	e, ok := d.Entities[c.EntityID]
	if !ok {
		return rejectf(-1, RejectNotFound, "entity %q does not exist", c.EntityID)
	}
	if err := verifyEntityBounds(d, e); err != nil {
		return err
	}
	if e.Solid && c.toHad {
		return rejectf(-1, RejectOverlap,
			"entity %q overlaps solid entity %q on layer %q at (%d,%d)",
			e.ID, c.toPrev, e.LayerID, c.To.X, c.To.Y)
	}
	return nil
}

// RemoveEntityCommand 删除实体。
type RemoveEntityCommand struct {
	EntityID string
	Ts       time.Time
	old      Entity
}

// NewRemoveEntityCommand 构造删除实体命令。
func NewRemoveEntityCommand(entityID string, ts time.Time) *RemoveEntityCommand {
	return &RemoveEntityCommand{EntityID: entityID, Ts: ts}
}

func (*RemoveEntityCommand) commandTag()  {}
func (*RemoveEntityCommand) kind() string { return "RemoveEntity" }
func (c *RemoveEntityCommand) describe() string {
	return fmt.Sprintf("RemoveEntity id=%q", c.EntityID)
}
func (*RemoveEntityCommand) canMergeTo(Command, MergePolicy) bool { return false }
func (*RemoveEntityCommand) merge(Command)                        {}
func (c *RemoveEntityCommand) apply(d *Document) *RejectError {
	e, ok := d.Entities[c.EntityID]
	if !ok {
		return rejectf(-1, RejectNotFound, "entity %q does not exist", c.EntityID)
	}
	c.old = *e
	if e.Props != nil {
		c.old.Props = cloneProps(e.Props)
	}
	d.forkStructs()
	d.indexRemove(e)
	delete(d.Entities, c.EntityID)
	d.markEntRemoved(c.EntityID, c.old.LayerID)
	return nil
}
func (c *RemoveEntityCommand) undo(d *Document) {
	d.forkStructs()
	e := c.old
	if e.Props != nil {
		e.Props = cloneProps(c.old.Props)
	}
	d.Entities[e.ID] = &e
	d.indexAdd(&e)
	d.markEntPlaced(e.ID, e.LayerID)
}
func (c *RemoveEntityCommand) verify(d *Document) *RejectError { return nil }

// ---- 区域 ----

// AddZoneCommand 新增区域标记。
type AddZoneCommand struct {
	Zone Zone
	Ts   time.Time
}

// NewAddZoneCommand 构造新增区域命令。
func NewAddZoneCommand(z Zone, ts time.Time) *AddZoneCommand {
	return &AddZoneCommand{Zone: z, Ts: ts}
}

func (*AddZoneCommand) commandTag()  {}
func (*AddZoneCommand) kind() string { return "AddZone" }
func (c *AddZoneCommand) describe() string {
	b := c.Zone.Bounds
	return fmt.Sprintf("AddZone id=%q kind=%q rect=(%d,%d)-(%d,%d)",
		c.Zone.ID, c.Zone.Kind, b.MinX, b.MinY, b.MaxX, b.MaxY)
}
func (*AddZoneCommand) canMergeTo(Command, MergePolicy) bool { return false }
func (*AddZoneCommand) merge(Command)                        {}
func (c *AddZoneCommand) apply(d *Document) *RejectError {
	z := c.Zone
	if z.ID == "" {
		return rejectf(-1, RejectInvalidValue, "zone id must not be empty")
	}
	if z.Kind == "" {
		return rejectf(-1, RejectInvalidValue, "zone %q kind must not be empty", z.ID)
	}
	if _, dup := d.Zones[z.ID]; dup {
		return rejectf(-1, RejectDuplicateID, "zone id %q already exists", z.ID)
	}
	if b := z.Bounds; b.Empty() {
		return rejectf(-1, RejectInvalidValue, "zone %q bounds must be non-empty", z.ID)
	}
	d.forkStructs()
	zc := z
	if zc.Props != nil {
		zc.Props = cloneProps(z.Props)
	}
	d.Zones[z.ID] = &zc
	d.ZoneOrder = append(d.ZoneOrder, z.ID)
	d.markZoneAdd(z.ID)
	return nil
}
func (c *AddZoneCommand) undo(d *Document) {
	d.forkStructs()
	d.markZoneRemove(c.Zone.ID)
	delete(d.Zones, c.Zone.ID)
	for i, id := range d.ZoneOrder {
		if id == c.Zone.ID {
			d.ZoneOrder = append(d.ZoneOrder[:i], d.ZoneOrder[i+1:]...)
			break
		}
	}
}
func (c *AddZoneCommand) verify(d *Document) *RejectError {
	return verifyZoneBounds(d, c.Zone.ID, c.Zone.Bounds)
}

// RemoveZoneCommand 删除区域标记。
type RemoveZoneCommand struct {
	ZoneID  string
	Ts      time.Time
	old     Zone
	orderAt int
}

// NewRemoveZoneCommand 构造删除区域命令。
func NewRemoveZoneCommand(zoneID string, ts time.Time) *RemoveZoneCommand {
	return &RemoveZoneCommand{ZoneID: zoneID, Ts: ts, orderAt: -1}
}

func (*RemoveZoneCommand) commandTag()  {}
func (*RemoveZoneCommand) kind() string { return "RemoveZone" }
func (c *RemoveZoneCommand) describe() string {
	return fmt.Sprintf("RemoveZone id=%q", c.ZoneID)
}
func (*RemoveZoneCommand) canMergeTo(Command, MergePolicy) bool { return false }
func (*RemoveZoneCommand) merge(Command)                        {}
func (c *RemoveZoneCommand) apply(d *Document) *RejectError {
	d.forkStructs()
	z, ok := d.Zones[c.ZoneID]
	if !ok {
		return rejectf(-1, RejectNotFound, "zone %q does not exist", c.ZoneID)
	}
	c.old = *z
	if z.Props != nil {
		c.old.Props = cloneProps(z.Props)
	}
	for i, id := range d.ZoneOrder {
		if id == c.ZoneID {
			c.orderAt = i
			d.ZoneOrder = append(d.ZoneOrder[:i], d.ZoneOrder[i+1:]...)
			break
		}
	}
	delete(d.Zones, c.ZoneID)
	d.markZoneRemove(c.ZoneID)
	return nil
}
func (c *RemoveZoneCommand) undo(d *Document) {
	d.forkStructs()
	z := c.old
	if z.Props != nil {
		z.Props = cloneProps(c.old.Props)
	}
	d.Zones[z.ID] = &z
	d.markZoneAdd(z.ID)
	if c.orderAt >= 0 && c.orderAt <= len(d.ZoneOrder) {
		d.ZoneOrder = append(d.ZoneOrder, "")
		copy(d.ZoneOrder[c.orderAt+1:], d.ZoneOrder[c.orderAt:])
		d.ZoneOrder[c.orderAt] = z.ID
	} else {
		d.ZoneOrder = append(d.ZoneOrder, z.ID)
	}
}
func (c *RemoveZoneCommand) verify(d *Document) *RejectError { return nil }

func cloneProps(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// verifyEntityBounds 在最终文档上校验实体的归属与越界约束。
// 重叠约束需要"变更前的占用者"，由各命令自行捕获并判断。
func verifyEntityBounds(d *Document, e *Entity) *RejectError {
	l, ok := d.Layers[e.LayerID]
	if !ok {
		return rejectf(-1, RejectIllegalOwner, "entity %q refers to unknown layer %q", e.ID, e.LayerID)
	}
	if e.Pos.X < 0 || e.Pos.Y < 0 || e.Pos.X >= l.Width || e.Pos.Y >= l.Height {
		return rejectf(-1, RejectOutOfBounds,
			"entity %q at (%d,%d) outside layer %q size %dx%d",
			e.ID, e.Pos.X, e.Pos.Y, e.LayerID, l.Width, l.Height)
	}
	return nil
}

// verifyZoneBounds 校验区域矩形合法且不越出地图。
func verifyZoneBounds(d *Document, id string, b Rect) *RejectError {
	if b.MinX >= b.MaxX || b.MinY >= b.MaxY {
		return rejectf(-1, RejectInvalidValue, "zone %q bounds must be non-empty", id)
	}
	if b.MinX < 0 || b.MinY < 0 || b.MaxX > d.MapWidth || b.MaxY > d.MapHeight {
		return rejectf(-1, RejectOutOfBounds,
			"zone %q bounds (%d,%d)-(%d,%d) exceed map %dx%d",
			id, b.MinX, b.MinY, b.MaxX, b.MaxY, d.MapWidth, d.MapHeight)
	}
	return nil
}
