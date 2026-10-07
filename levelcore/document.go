package levelcore

import "sort"

// FormatVersion 是导出/打包格式的当前版本。
const FormatVersion uint16 = 1

// MinSupportedVersion / MaxSupportedVersion 声明可安全读入的格式范围。
const (
	MinSupportedVersion uint16 = 1
	MaxSupportedVersion uint16 = FormatVersion
)

// Point 是地图上的整数坐标。
type Point struct{ X, Y int32 }

// Rect 为左闭右开矩形：Min<=p<Max。
type Rect struct {
	MinX, MinY, MaxX, MaxY int32
}

// Contains 判断点是否落在矩形内。
func (r Rect) Contains(p Point) bool {
	return p.X >= r.MinX && p.X < r.MaxX && p.Y >= r.MinY && p.Y < r.MaxY
}

// Empty 判断矩形是否为空（无格子）。
func (r Rect) Empty() bool {
	return r.MaxX <= r.MinX || r.MaxY <= r.MinY
}

// Layer 表示一层地形。
type Layer struct {
	ID     string
	Name   string
	Width  int32
	Height int32
}

// Entity 表示一个实体摆放。
type Entity struct {
	ID      string
	Kind    string
	LayerID string
	Pos     Point
	Solid   bool
	Props   map[string]string
}

// Zone 表示一个区域标记。
type Zone struct {
	ID     string
	Kind   string
	Bounds Rect
	Props  map[string]string
}

// tileChunk 是一个 16x16 地形块的稀疏单元存储。
// 块一旦被冻结快照引用即视为不可变；工作副本写入时整体替换为新块，
// 依赖 GC 回收失去引用的旧块，因此无需遍历式引用计数。
type tileChunk struct {
	cells map[uint64]uint16
}

// tilePageSize 是每个持久化页覆盖的块槽位数（线性块索引按页分组）。
const tilePageSize = 64

// tilePage 是连续 64 个块槽位的定长数组；nil 表示空块。
// 页一旦进入冻结状态即不可变，写入时整页复制（成本恒定 64 个指针）。
type tilePage struct {
	slots [tilePageSize]*tileChunk
}

// tileState 是某图层块表的持久化节点：pages 只记录本节点覆盖的页，
// 未命中沿 parent 查找。持久化以"页"为粒度，使摊平成本只与脏页数量
// （≈改动块数/64）相关，单次编辑成本不随文档总规模线性增长。
type tileState struct {
	parent *tileState
	pages  map[uint64]*tilePage
	depth  int
}

// tileFlattenDepth 是覆盖链深度阈值；超过则摊平（摊还成本有界）。
const tileFlattenDepth = 8

// chunkKey 是块在块映射中的复合键。
func chunkKey(cx, cy uint32) uint64 { return uint64(cx)<<32 | uint64(cy) }

// chunkKey2 是供 delta 使用的数组型块键。
func chunkKey2(cx, cy uint32) [2]uint32 { return [2]uint32{cx, cy} }

// ChunkSize 是地形分块边长（固定常量以保证跨机器一致）。
const ChunkSize = 16

// Document 是被编辑的核心数据，所有变更应经由 Editor。
type Document struct {
	SchemaVersion uint16
	MapWidth      int32
	MapHeight     int32
	Layers        map[string]*Layer
	LayerOrder    []string
	Entities      map[string]*Entity
	Zones         map[string]*Zone
	ZoneOrder     []string
	// tiles[layerID] 按分块稀疏存储；块以 tileChunk 引用计数做 COW。
	tiles map[string]*tileState
	// occ 是 Solid 实体的空间占用索引：layerID+坐标 -> 实体 ID。
	occ map[string]string
	// layerEntities 记录每个图层的实体数量，O(1) 判断图层是否仍被引用。
	layerEntities map[string]int
	// active 非空时记录增量，供约束校验与产物重建。
	active *delta

	// COW 标记：对应容器是否仍与某个冻结快照共享。仅在首次写时惰性分离，
	// 未被本次提交触及的容器永不复制，保证单次编辑成本不随文档规模增长。
	structsShared bool // Entities/Zones/Layers 及其顺序切片
	tilesShared   bool // tiles 外层容器是否与冻结版本共享
	indexShared   bool // occ / layerEntities
	// workLayer 记录本工作版本已派生覆盖节点的图层，每图层每版本只派生一次。
	workLayer map[string]*tileState
}

// NewDocument 创建一个空白文档。
func NewDocument(width, height int32) *Document {
	return &Document{
		SchemaVersion: FormatVersion,
		MapWidth:      width,
		MapHeight:     height,
		Layers:        map[string]*Layer{},
		Entities:      map[string]*Entity{},
		Zones:         map[string]*Zone{},
		tiles:         map[string]*tileState{},
		occ:           map[string]string{},
		layerEntities: map[string]int{},
	}
}

// unsigned32 把非负 int32 转为 uint32。
func unsigned32(v int32) (uint32, bool) {
	if v < 0 {
		return 0, false
	}
	return uint32(v), true
}

// chunkCoordXY 计算某格所在块坐标与块内线性下标（行优先）。
func chunkCoordXY(x, y int32) (cx, cy uint32, idx uint64, ok bool) {
	ux, okx := unsigned32(x)
	uy, oky := unsigned32(y)
	if !okx || !oky {
		return 0, 0, 0, false
	}
	cx = ux / ChunkSize
	cy = uy / ChunkSize
	idx = uint64(uy%ChunkSize)*ChunkSize + uint64(ux%ChunkSize)
	return cx, cy, idx, true
}

// layerLinear 计算某图层内 (cx,cy) 的线性块索引。
func layerLinear(layerID string, d *Document, cx, cy uint32) (uint64, uint32) {
	l := d.Layers[layerID]
	cols := uint32((l.Width + ChunkSize - 1) / ChunkSize)
	return uint64(cy)*uint64(cols) + uint64(cx), cols
}

// Tile 读取某层某格的值；未记录时返回 (0,false)。
func (d *Document) Tile(layerID string, p Point) (uint16, bool) {
	cx, cy, idx, ok := chunkCoordXY(p.X, p.Y)
	if !ok {
		return 0, false
	}
	ch := d.chunkAt(layerID, cx, cy)
	if ch == nil {
		return 0, false
	}
	v, has := ch.cells[idx]
	return v, has
}

// lookupPage 沿状态链查找页。
func (s *tileState) lookupPage(pg uint64) *tilePage {
	for cur := s; cur != nil; cur = cur.parent {
		if p, ok := cur.pages[pg]; ok {
			return p // 可能为 nil（整页为空）
		}
	}
	return nil
}

func (s *tileState) lookup(linear uint64) *tileChunk {
	pg, off := linear/tilePageSize, linear%tilePageSize
	page := s.lookupPage(pg)
	if page == nil {
		return nil
	}
	return page.slots[off]
}

// SetTile 写入某层某格；v==0 视为清除。
func (d *Document) SetTile(layerID string, p Point, v uint16) {
	cx, cy, idx, ok := chunkCoordXY(p.X, p.Y)
	if !ok {
		return
	}
	d.forkTiles()
	st := d.workState(layerID)
	if st == nil {
		if v == 0 {
			return
		}
		st = d.deriveLayer(layerID)
	}
	linear, _ := layerLinear(layerID, d, cx, cy)
	pg, off := linear/tilePageSize, linear%tilePageSize
	page := st.lookupPage(pg)
	old := (*tileChunk)(nil)
	if page != nil {
		old = page.slots[off]
	}
	if v == 0 {
		if old == nil {
			return
		}
		st.putChunk(linear, nil)
		return
	}
	if old == nil {
		st.putChunk(linear, &tileChunk{cells: map[uint64]uint16{idx: v}})
		return
	}
	nc := copyChunk(old)
	nc.cells[idx] = v
	st.putChunk(linear, nc)
}

// putChunk 把块写入本节点：对目标页做定长 COW 复制后改一个槽位。
func (s *tileState) putChunk(linear uint64, ch *tileChunk) {
	pg, off := linear/tilePageSize, linear%tilePageSize
	var np tilePage
	if old := s.lookupPage(pg); old != nil {
		np = *old // 64 槽位数组复制，成本恒定
	}
	np.slots[off] = ch
	if s.pages == nil {
		s.pages = map[uint64]*tilePage{}
	}
	s.pages[pg] = &np
}

func copyChunk(old *tileChunk) *tileChunk {
	nc := &tileChunk{cells: make(map[uint64]uint16, len(old.cells)+1)}
	for idx, val := range old.cells {
		nc.cells[idx] = val
	}
	return nc
}

// workState 返回某图层在本工作版本的覆盖节点（尚未派生则返回 nil）。
func (d *Document) workState(layerID string) *tileState { return d.workLayer[layerID] }

// deriveLayer 为某图层派生覆盖节点；链过深时先摊平（摊还成本只与脏页数相关）。
func (d *Document) deriveLayer(layerID string) *tileState {
	base := d.tiles[layerID]
	if base != nil && base.depth >= tileFlattenDepth {
		base = &tileState{pages: base.flatten(), depth: 1}
		d.tiles[layerID] = base
	}
	depth := 1
	if base != nil {
		depth = base.depth + 1
	}
	st := &tileState{parent: base, pages: map[uint64]*tilePage{}, depth: depth}
	d.tiles[layerID] = st
	d.workLayer[layerID] = st
	return st
}

// tileChunk 返回某层某块的单元映射与是否存在（读取路径使用）。
func (d *Document) tileChunk(layerID string, cx, cy uint32) (map[uint64]uint16, bool) {
	ch := d.chunkAt(layerID, cx, cy)
	if ch == nil {
		return nil, false
	}
	return ch.cells, true
}

// chunkAt 直接返回块对象（供增量重建使用）。
func (d *Document) chunkAt(layerID string, cx, cy uint32) *tileChunk {
	st := d.tiles[layerID]
	if st == nil {
		return nil
	}
	linear, _ := layerLinear(layerID, d, cx, cy)
	return st.lookup(linear)
}

// restoreChunk 把指定块恢复为快照内容（ch==nil 表示恢复为不存在）。
func (d *Document) restoreChunk(layerID string, p Point, ch *tileChunk) {
	cx, cy, _, ok := chunkCoordXY(p.X, p.Y)
	if !ok {
		return
	}
	st := d.workState(layerID)
	if st == nil {
		st = d.deriveLayer(layerID)
	}
	linear, _ := layerLinear(layerID, d, cx, cy)
	st.putChunk(linear, ch) // nil 槽位即表示块不存在
}

// iterChunks 确定性遍历某图层当前可见的非空块（线性序），供全量重建/序列化。
func (d *Document) iterChunks(layerID string, fn func(cx, cy uint32, ch *tileChunk)) {
	st := d.tiles[layerID]
	if st == nil {
		return
	}
	flat := st.flatten()
	_, cols := uint32(0), uint32(1)
	l := d.Layers[layerID]
	cols = uint32((l.Width + ChunkSize - 1) / ChunkSize)
	pageKeys := make([]uint64, 0, len(flat))
	for pk := range flat {
		pageKeys = append(pageKeys, pk)
	}
	sortU64(pageKeys)
	for _, pk := range pageKeys {
		page := flat[pk]
		if page == nil {
			continue
		}
		for off := uint64(0); off < tilePageSize; off++ {
			ch := page.slots[off]
			if ch == nil {
				continue
			}
			linear := pk*tilePageSize + off
			cx := uint32(linear % uint64(cols))
			cy := uint32(linear / uint64(cols))
			fn(cx, cy, ch)
		}
	}
}

// iterChunksMark 遍历图层全部非空块并登记到活动增量（删除图层撤销时使用）。
func (s *tileState) iterChunksMark(d *Document, layerID string) {
	flat := s.flatten()
	for pk, page := range flat {
		if page == nil {
			continue
		}
		for off, ch := range page.slots {
			if ch == nil {
				continue
			}
			linear := pk*tilePageSize + uint64(off)
			cols := uint32((d.Layers[layerID].Width + ChunkSize - 1) / ChunkSize)
			cx := uint32(linear % uint64(cols))
			cy := uint32(linear / uint64(cols))
			d.markTileIfActive(layerID, cx, cy)
		}
	}
}

// flatten 把链折叠为一张完整只读页映射。
func (s *tileState) flatten() map[uint64]*tilePage {
	if s.parent == nil {
		return s.pages
	}
	if s.depth <= tileFlattenDepth {
		out := map[uint64]*tilePage{}
		for cur := s; cur != nil; cur = cur.parent {
			for pg, p := range cur.pages {
				if _, seen := out[pg]; !seen {
					out[pg] = p
				}
			}
		}
		return out
	}
	merged := map[uint64]*tilePage{}
	var chain []*tileState
	for cur := s; cur != nil; cur = cur.parent {
		chain = append(chain, cur)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for pg, p := range chain[i].pages {
			merged[pg] = p
		}
	}
	s.parent = nil
	s.pages = merged
	s.depth = 1
	return merged
}

// clone 返回文档的写时复制分支：所有容器直接共享，O(1) 复杂度。
// 后续写入在首次触及某容器/对象时惰性分离，不影响已冻结快照。
func (d *Document) clone() *Document {
	n := &Document{
		SchemaVersion: d.SchemaVersion,
		MapWidth:      d.MapWidth,
		MapHeight:     d.MapHeight,
		Layers:        d.Layers,
		LayerOrder:    d.LayerOrder,
		Entities:      d.Entities,
		Zones:         d.Zones,
		ZoneOrder:     d.ZoneOrder,
		tiles:         d.tiles,
		occ:           d.occ,
		layerEntities: d.layerEntities,
		structsShared: true,
		tilesShared:   true,
		indexShared:   true,
		workLayer:     map[string]*tileState{},
	}
	return n
}

// forkStructs 惰性分离结构容器（Entities/Zones/Layers 及顺序切片）。
func (d *Document) forkStructs() {
	if !d.structsShared {
		return
	}
	ents := make(map[string]*Entity, len(d.Entities))
	for k, v := range d.Entities {
		ents[k] = v
	}
	d.Entities = ents
	zones := make(map[string]*Zone, len(d.Zones))
	for k, v := range d.Zones {
		zones[k] = v
	}
	d.Zones = zones
	layers := make(map[string]*Layer, len(d.Layers))
	for k, v := range d.Layers {
		layers[k] = v
	}
	d.Layers = layers
	d.LayerOrder = append([]string(nil), d.LayerOrder...)
	d.ZoneOrder = append([]string(nil), d.ZoneOrder...)
	d.structsShared = false
}

// mutableEntity 返回本版本独占的实体对象（对象级 COW）。
func (d *Document) mutableEntity(id string) *Entity {
	e := d.Entities[id]
	if e == nil {
		return nil
	}
	ec := *e
	if e.Props != nil {
		ec.Props = cloneProps(e.Props)
	}
	d.Entities[id] = &ec
	return &ec
}

// forkTiles 惰性分离地形外层容器（layerID->tileState），O(图层数)。
// 每个图层的 tileState 是不可变持久化节点，写入由 deriveLayer 就地派生新节点。
func (d *Document) forkTiles() {
	if !d.tilesShared {
		return
	}
	tiles := make(map[string]*tileState, len(d.tiles))
	for layerID, st := range d.tiles {
		tiles[layerID] = st
	}
	d.tiles = tiles
	d.tilesShared = false
}

// forkIndexes 惰性分离占用索引与图层计数。
func (d *Document) forkIndexes() {
	if !d.indexShared {
		return
	}
	occ := make(map[string]string, len(d.occ))
	for k, v := range d.occ {
		occ[k] = v
	}
	d.occ = occ
	counts := make(map[string]int, len(d.layerEntities))
	for k, v := range d.layerEntities {
		counts[k] = v
	}
	d.layerEntities = counts
	d.indexShared = false
}

// indexAdd 登记 Solid 实体占用并递增图层计数。
func (d *Document) indexAdd(e *Entity) {
	d.forkIndexes()
	if e.Solid {
		d.occ[occKey(e.LayerID, e.Pos)] = e.ID
	}
	d.layerEntities[e.LayerID]++
}

// indexRemove 移除实体占用并递减图层计数。
func (d *Document) indexRemove(e *Entity) {
	d.forkIndexes()
	if e.Solid {
		delete(d.occ, occKey(e.LayerID, e.Pos))
	}
	d.layerEntities[e.LayerID]--
	if d.layerEntities[e.LayerID] <= 0 {
		delete(d.layerEntities, e.LayerID)
	}
}

// decLayerCount 仅递减图层计数（占用键恢复由命令精确处理）。
func (d *Document) decLayerCount(layerID string) {
	d.forkIndexes()
	d.layerEntities[layerID]--
	if d.layerEntities[layerID] <= 0 {
		delete(d.layerEntities, layerID)
	}
}

// ---- 增量记录入口 ----

func (d *Document) markMoved(id, layerID string) {
	if d.active != nil {
		d.active.entitiesMoved[id] = struct{}{}
		d.active.entityLayer[id] = layerID
	}
}

func (d *Document) markEntPlaced(id, layerID string) {
	if d.active != nil {
		d.active.entitiesPlaced[id] = struct{}{}
		delete(d.active.entitiesRemoved, id)
		d.active.entityLayer[id] = layerID
	}
}

func (d *Document) markEntRemoved(id, layerID string) {
	if d.active != nil {
		d.active.entitiesRemoved[id] = struct{}{}
		delete(d.active.entitiesPlaced, id)
		d.active.entityLayer[id] = layerID
	}
}

func (d *Document) markLayerAdd(id string) {
	if d.active != nil {
		d.active.layersAdded[id] = struct{}{}
	}
}

func (d *Document) markLayerRemove(id string) {
	if d.active != nil {
		d.active.layersRemoved[id] = struct{}{}
	}
}

func (d *Document) markTileIfActive(layerID string, cx, cy uint32) {
	if d.active != nil {
		d.active.markTile(layerID, cx, cy)
	}
}

func (d *Document) markTileAt(layerID string, p Point) {
	if d.active == nil {
		return
	}
	cx, cy, _, _ := chunkCoordXY(p.X, p.Y)
	d.active.markTile(layerID, cx, cy)
}

func (d *Document) markZoneAdd(id string) {
	if d.active != nil {
		d.active.zonesAdded[id] = struct{}{}
	}
}

func (d *Document) markZoneRemove(id string) {
	if d.active != nil {
		d.active.zonesRemoved[id] = struct{}{}
	}
}

func (d *Document) beginDelta() *delta {
	dl := newDelta()
	d.active = dl
	return dl
}

func (d *Document) endDelta() *delta {
	dl := d.active
	d.active = nil
	return dl
}

// rebuildIndex 根据实体表重建占用索引与图层计数（载入/解包后使用）。
func (d *Document) rebuildIndex() {
	d.occ = make(map[string]string, len(d.Entities))
	d.layerEntities = make(map[string]int)
	for id, e := range d.Entities {
		if e.Solid {
			d.occ[occKey(e.LayerID, e.Pos)] = id
		}
		d.layerEntities[e.LayerID]++
	}
	d.indexShared = false
}

// markFrozen 声明当前文档为不可变冻结版本：它持有的全部容器从此只读，
// 下一次克隆必须在首次写入时重新分离。
func (d *Document) markFrozen() {
	d.structsShared = false // 自身独占；clone() 会置 true 表示新副本共享
	d.tilesShared = false
	d.indexShared = false
	d.workLayer = nil
	d.active = nil
}

// occKey 生成占用索引键，使用 NUL 分隔避免歧义。
func occKey(layerID string, p Point) string {
	return layerID + "\x00" + intToStr(p.X) + "\x00" + intToStr(p.Y)
}

// sortedKeys 是确定性取键辅助函数。
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// intToStr 把 int32 确定性地转成十进制字符串。
func intToStr(v int32) string {
	neg := v < 0
	u := uint32(v)
	if neg {
		u = uint32(-int64(v))
	}
	var buf [12]byte
	i := len(buf)
	if u == 0 {
		i--
		buf[i] = '0'
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
