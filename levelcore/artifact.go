package levelcore

import (
	"container/list"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Artifact 是由 Document 确定性派生的可打包产物。
//
// 派生规则（无任何墙钟/随机/映射遍历因素）：
//
//   - Chunks：每个图层按其尺寸划分 ChunkSize 的完整块网格，
//     包含全部块（空块也存在，值为空串），键为 "cx,cy"。
//     块内容编码为 "idx:v" 对，按 idx 升序，以 "," 连接。
//   - EntityIndex：每个图层一项，值为该图层实体的确定性摘要，
//     按实体 ID 升序；摘要格式 "id|kind|x|y|solid|k=v;..."。
//   - ZoneList：区域摘要按区域 ID 升序；
//     摘要格式 "id|kind|minX,minY,maxX,maxY|k=v;..."。
//   - Revision 由编辑序列单调产生，BuiltAt 固定为零值时刻。
//
// 性能：Chunks 的图层块表以持久化映射（chunkMap）保存，
// 单次增量只生成一个仅含改动键的 overlay，成本与改动块数成正比，
// 不随文档/历史块总量线性增长；序列化时按键序摊平，结果确定。
type Artifact struct {
	FormatVersion uint16
	Chunks        map[string]*chunkMap
	EntityIndex   map[string][]string
	ZoneList      []string
	Revision      uint64
	BuiltAt       string
}

// chunkMap 是不可变的持久化字符串映射：overlay 记录本次改动，
// get 沿链查找（nil 值表示墓碑，仅在合并时使用）。
type chunkMap struct {
	parent  *chunkMap
	overlay map[string]string
	depth   int
}

// chunkMapDepth 是序列化前触发摊平的深度阈值（摊还有界）。
const chunkMapDepth = 16

func newChunkMap() *chunkMap {
	return &chunkMap{overlay: map[string]string{}}
}

func (m *chunkMap) get(k string) (string, bool) {
	for cur := m; cur != nil; cur = cur.parent {
		if v, ok := cur.overlay[k]; ok {
			return v, true
		}
	}
	return "", false
}

// withOverlay 以 m 为父版本生成仅含 entries 的新版本（O(len(entries))）。
func (m *chunkMap) withOverlay(entries map[string]string) *chunkMap {
	return &chunkMap{parent: m, overlay: entries, depth: m.depth + 1}
}

// flatten 返回一张完整只读 map；深度超阈值时摊平，保证查找/序列化有界。
func (m *chunkMap) flatten() map[string]string {
	if m.parent == nil {
		return m.overlay
	}
	if m.depth <= chunkMapDepth {
		out := map[string]string{}
		for cur := m; cur != nil; cur = cur.parent {
			for k, v := range cur.overlay {
				if _, seen := out[k]; !seen {
					out[k] = v
				}
			}
		}
		return out
	}
	merged := map[string]string{}
	var chain []*chunkMap
	for cur := m; cur != nil; cur = cur.parent {
		chain = append(chain, cur)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for k, v := range chain[i].overlay {
			merged[k] = v
		}
	}
	m.parent = nil
	m.overlay = merged
	m.depth = 1
	return merged
}

// DefaultCacheLimit 是块缓存的默认容量（按块计）。
const DefaultCacheLimit = 512

// chunkCache 是有容量上限的 LRU 缓存：缓存"文档块内容 -> 编码"。
// 键仅依赖块坐标与数据本身；内容变化的块会被显式失效，
// 因此缓存不会返回过期编码。
type chunkCache struct {
	limit int
	store map[string]*list.Element
	order *list.List
}

type cacheEntry struct {
	key  string
	code string
}

func newChunkCache(limit int) *chunkCache {
	if limit <= 0 {
		limit = DefaultCacheLimit
	}
	return &chunkCache{
		limit: limit,
		store: map[string]*list.Element{},
		order: list.New(),
	}
}

func (c *chunkCache) get(key string) (string, bool) {
	if el, ok := c.store[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*cacheEntry).code, true
	}
	return "", false
}

func (c *chunkCache) put(key, code string) {
	if el, ok := c.store[key]; ok {
		el.Value.(*cacheEntry).code = code
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&cacheEntry{key: key, code: code})
	c.store[key] = el
	for c.order.Len() > c.limit {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		c.order.Remove(oldest)
		delete(c.store, oldest.Value.(*cacheEntry).key)
	}
}

// invalidateLayer 丢弃某图层的全部缓存（图层删除/整层重建时使用）。
func (c *chunkCache) invalidateLayer(layerID string) {
	prefix := layerID + "|"
	for k := range c.store {
		if strings.HasPrefix(k, prefix) {
			delete(c.store, k)
		}
	}
	// 重整 LRU 链表（删除图层是罕见操作）。
	for el := c.order.Front(); el != nil; {
		next := el.Next()
		if strings.HasPrefix(el.Value.(*cacheEntry).key, prefix) {
			c.order.Remove(el)
		}
		el = next
	}
}

// invalidateKey 丢弃单个块的缓存编码（块内容变化时使用）。
func (c *chunkCache) invalidateKey(key string) {
	if el, ok := c.store[key]; ok {
		c.order.Remove(el)
		delete(c.store, key)
	}
}

func (c *chunkCache) len() int { return c.order.Len() }

// artifactBuilder 维护增量派生状态。
//
// chunksWork/entityWork 是可变工作态；每次 freeze 把它们的引用装进不可变
// Artifact 快照。增量更新只改写被触及的工作 map/切片，随后整图快照零拷贝，
// 因此读取产物的成本为 O(1)，与文档规模无关。
type artifactBuilder struct {
	artifact   *Artifact
	cache      *chunkCache
	chunksWork map[string]*chunkMap
	// entityIDs: layerID -> 排序实体 ID；entityWork: 对齐的编码摘要。
	entityIDs  map[string][]string
	entityWork map[string][]string
	// zoneIDs 为排序区域 ID，zoneWork 为其编码列表。
	zoneIDs  []string
	zoneWork []string
}

func newArtifactBuilder(cacheLimit int) *artifactBuilder {
	return &artifactBuilder{
		cache:      newChunkCache(cacheLimit),
		chunksWork: map[string]*chunkMap{},
		entityIDs:  map[string][]string{},
		entityWork: map[string][]string{},
		zoneIDs:    []string{},
		zoneWork:   []string{},
	}
}

func chunkMapKey(cx, cy uint32) string {
	return strconv.FormatUint(uint64(cx), 10) + "," + strconv.FormatUint(uint64(cy), 10)
}

// chunkDims 返回图层的块列/行数。
func chunkDims(l *Layer) (cols, rows uint32) {
	return uint32((l.Width + ChunkSize - 1) / ChunkSize),
		uint32((l.Height + ChunkSize - 1) / ChunkSize)
}

// encodeChunkCells 把块内单元确定性编码：按下标升序输出 "idx:v" 对。
func encodeChunkCells(cells map[uint64]uint16) string {
	if len(cells) == 0 {
		return ""
	}
	idxs := make([]uint64, 0, len(cells))
	for idx := range cells {
		idxs = append(idxs, idx)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })
	var sb strings.Builder
	for i, idx := range idxs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatUint(idx, 10))
		sb.WriteByte(':')
		sb.WriteString(strconv.FormatUint(uint64(cells[idx]), 10))
	}
	return sb.String()
}

func chunksPtr(d *Document, layerID string, cx, cy uint32) *tileChunk {
	return d.chunkAt(layerID, cx, cy)
}

func uintptrKey(p *tileChunk) string {
	return strconv.FormatUint(uint64(uintptr(unsafePointer(p))), 16)
}

// encodeEntity 生成实体摘要。
func encodeEntity(e *Entity) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s|%s|%d|%d|%t|", e.ID, e.Kind, e.Pos.X, e.Pos.Y, e.Solid)
	keys := make([]string, 0, len(e.Props))
	for k := range e.Props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(';')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(e.Props[k])
	}
	return sb.String()
}

// encodeZone 生成区域摘要。
func encodeZone(z *Zone) string {
	b := z.Bounds
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s|%s|%d,%d,%d,%d|", z.ID, z.Kind, b.MinX, b.MinY, b.MaxX, b.MaxY)
	keys := make([]string, 0, len(z.Props))
	for k := range z.Props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(';')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(z.Props[k])
	}
	return sb.String()
}

// rebuildFrom 从零全量重建产物。
func (b *artifactBuilder) rebuildFrom(d *Document, revision uint64) *Artifact {
	// 全新缓存与工作态，避免任何历史状态残留。
	b.cache = newChunkCache(b.cache.limit)
	b.chunksWork = map[string]*chunkMap{}
	b.entityIDs = map[string][]string{}
	b.entityWork = map[string][]string{}
	b.zoneIDs = b.zoneIDs[:0]

	// 图层顺序固定为 LayerOrder；块按 (cy,cx) 行优先遍历。
	for _, layerID := range d.LayerOrder {
		l := d.Layers[layerID]
		cols, rows := chunkDims(l)
		chunks := make(map[string]string, int(cols*rows))
		for cy := uint32(0); cy < rows; cy++ {
			for cx := uint32(0); cx < cols; cx++ {
				key := chunkMapKey(cx, cy)
				ch := chunksPtr(d, layerID, cx, cy)
				if ch == nil {
					chunks[key] = ""
				} else {
					chunks[key] = b.chunkCode(ch, layerID, cx, cy)
				}
			}
		}
		b.chunksWork[layerID] = &chunkMap{overlay: chunks}
	}

	// 实体索引：收集到图层后按 ID 排序。
	byLayer := map[string][]string{}
	for id := range d.Entities {
		e := d.Entities[id]
		byLayer[e.LayerID] = append(byLayer[e.LayerID], id)
	}
	for _, layerID := range d.LayerOrder {
		ids := byLayer[layerID]
		sort.Strings(ids)
		b.entityIDs[layerID] = ids
		b.entityWork[layerID] = b.encodeEntityList(d, ids)
	}

	// 区域按 ID 排序。
	b.zoneIDs = b.zoneIDs[:0]
	for id := range d.Zones {
		b.zoneIDs = append(b.zoneIDs, id)
	}
	sort.Strings(b.zoneIDs)
	b.zoneWork = b.encodeZoneList(d, b.zoneIDs)
	return b.freeze(revision)
}

// freeze 用当前工作态的引用组装一份不可变产物快照。
// 调用方在 freeze 之后不会就地修改被装入的 map/slice，
// 因此快照无需深拷贝，读取成本与文档规模无关。
func (b *artifactBuilder) freeze(revision uint64) *Artifact {
	chunks := make(map[string]*chunkMap, len(b.chunksWork))
	for layerID, m := range b.chunksWork {
		chunks[layerID] = m
	}
	ents := make(map[string][]string, len(b.entityWork))
	for layerID, codes := range b.entityWork {
		ents[layerID] = codes
	}
	a := &Artifact{
		FormatVersion: FormatVersion,
		Chunks:        chunks,
		EntityIndex:   ents,
		ZoneList:      b.zoneWork,
		Revision:      revision,
		BuiltAt:       deterministicBuildTime,
	}
	b.artifact = a
	return a
}

// chunkCode 编码块内容；缓存以"块指针标识"为键：块不可变且编辑时整体替换，
// 同一指针必然对应同一编码，因此既不会返回过期值，也无需失效。
func (b *artifactBuilder) chunkCode(ch *tileChunk, layerID string, cx, cy uint32) string {
	key := layerID + "|" + chunkMapKey(cx, cy) + "|" + uintptrKey(ch)
	if code, ok := b.cache.get(key); ok {
		return code
	}
	code := encodeChunkCells(ch.cells)
	b.cache.put(key, code)
	return code
}

func (b *artifactBuilder) encodeEntityList(d *Document, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, encodeEntity(d.Entities[id]))
	}
	return out
}

func (b *artifactBuilder) encodeZoneList(d *Document, ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, encodeZone(d.Zones[id]))
	}
	return out
}

// applyDelta 基于 delta 做增量重建；产物块表为持久化映射，
// 单次成本只与本次改动的块/实体/区域数量相关，与文档总规模无关。
func (b *artifactBuilder) applyDelta(d *Document, revision uint64, dl *delta) *Artifact {
	if b.chunksWork == nil {
		return b.rebuildFrom(d, revision)
	}
	w := b.chunksWork

	// 图层删除。
	for id := range dl.layersRemoved {
		b.cache.invalidateLayer(id)
		delete(w, id)
		delete(b.entityWork, id)
		delete(b.entityIDs, id)
	}
	// 图层新增：建立完整块网格（新图层本身一次性成本与其尺寸相关）。
	for _, id := range sortedKeys(dl.layersAdded) {
		l := d.Layers[id]
		if l == nil {
			continue
		}
		cols, rows := chunkDims(l)
		grid := make(map[string]string, int(cols*rows))
		for cy := uint32(0); cy < rows; cy++ {
			for cx := uint32(0); cx < cols; cx++ {
				key := chunkMapKey(cx, cy)
				if ch := chunksPtr(d, id, cx, cy); ch == nil {
					grid[key] = ""
				} else {
					grid[key] = b.chunkCode(ch, id, cx, cy)
				}
			}
		}
		w[id] = &chunkMap{overlay: grid}
		if _, ok := b.entityWork[id]; !ok {
			b.entityWork[id] = []string{}
			b.entityIDs[id] = []string{}
		}
	}
	// 地形块：每个受影响图层生成一个仅含改动块的 overlay（O(改动块数)）。
	for layerID, set := range dl.tileChunks {
		base := w[layerID]
		entries := make(map[string]string, len(set))
		for k := range set {
			cx, cy := k[0], k[1]
			key := chunkMapKey(cx, cy)
			if ch := chunksPtr(d, layerID, cx, cy); ch == nil {
				entries[key] = ""
			} else {
				entries[key] = b.chunkCode(ch, layerID, cx, cy)
			}
		}
		if base == nil {
			// 图层网格尚未建立（例如块先于显式网格出现的防御路径）：
			// 以空网格为底，确保未改动的空块仍解析为空串。
			l := d.Layers[layerID]
			if l != nil {
				cols, rows := chunkDims(l)
				grid := make(map[string]string, int(cols*rows))
				for cy := uint32(0); cy < rows; cy++ {
					for cx := uint32(0); cx < cols; cx++ {
						grid[chunkMapKey(cx, cy)] = ""
					}
				}
				base = &chunkMap{overlay: grid}
			} else {
				base = newChunkMap()
			}
		}
		w[layerID] = base.withOverlay(entries)
	}

	// 实体：仅复制并更新受影响图层的有序切片。
	touchedEnt := map[string]struct{}{}
	for id := range dl.entitiesRemoved {
		touchedEnt[dl.entityLayer[id]] = struct{}{}
	}
	for id := range dl.entitiesPlaced {
		if e := d.Entities[id]; e != nil {
			touchedEnt[e.LayerID] = struct{}{}
		} else {
			touchedEnt[dl.entityLayer[id]] = struct{}{}
		}
	}
	for id := range dl.entitiesMoved {
		touchedEnt[dl.entityLayer[id]] = struct{}{}
	}
	b.entitySlicesCOW(touchedEnt)
	for _, id := range sortedKeys(dl.entitiesRemoved) {
		layerID := dl.entityLayer[id]
		ids := b.entityIDs[layerID]
		pos := sort.SearchStrings(ids, id)
		b.entityIDs[layerID] = removeSorted(ids, id)
		codes := b.entityWork[layerID]
		if pos < len(codes) {
			b.entityWork[layerID] = removeCodeAt(codes, pos)
		}
	}
	for _, id := range sortedKeys(dl.entitiesPlaced) {
		e := d.Entities[id]
		if e == nil {
			continue
		}
		ids := b.entityIDs[e.LayerID]
		pos := sort.SearchStrings(ids, id)
		if pos < len(ids) && ids[pos] == id {
			b.entityWork[e.LayerID][pos] = encodeEntity(e)
			continue
		}
		b.entityIDs[e.LayerID] = insertSorted(ids, id)
		b.entityWork[e.LayerID] = insertCode(b.entityWork[e.LayerID], pos, encodeEntity(e))
	}
	for mid := range dl.entitiesMoved {
		if e := d.Entities[mid]; e != nil {
			ids := b.entityIDs[e.LayerID]
			if p := sort.SearchStrings(ids, mid); p < len(ids) && ids[p] == mid {
				b.entityWork[e.LayerID][p] = encodeEntity(e)
			}
		}
	}

	// 区域：复制有序 ID 列表后增删，整体重编码。
	if len(dl.zonesAdded) > 0 || len(dl.zonesRemoved) > 0 {
		b.zoneIDs = append([]string(nil), b.zoneIDs...)
		for _, id := range sortedKeys(dl.zonesRemoved) {
			b.zoneIDs = removeSorted(b.zoneIDs, id)
		}
		for _, id := range sortedKeys(dl.zonesAdded) {
			if _, live := d.Zones[id]; live {
				b.zoneIDs = insertSorted(b.zoneIDs, id)
			}
		}
		b.zoneWork = b.encodeZoneList(d, b.zoneIDs)
	}
	return b.freeze(revision)
}

func insertSorted(ids []string, id string) []string {
	i := sort.SearchStrings(ids, id)
	if i < len(ids) && ids[i] == id {
		ids[i] = id
		return ids
	}
	ids = append(ids, "")
	copy(ids[i+1:], ids[i:])
	ids[i] = id
	return ids
}

func insertCode(codes []string, pos int, code string) []string {
	codes = append(codes, "")
	copy(codes[pos+1:], codes[pos:])
	codes[pos] = code
	return codes
}

func removeCodeAt(codes []string, pos int) []string {
	if pos < 0 || pos >= len(codes) {
		return codes
	}
	return append(codes[:pos], codes[pos+1:]...)
}

func removeSorted(ids []string, id string) []string {
	i := sort.SearchStrings(ids, id)
	if i < len(ids) && ids[i] == id {
		ids = append(ids[:i], ids[i+1:]...)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids
}

// deterministicBuildTime 是固定的构建时间标签，保证跨机器逐字段一致。
const deterministicBuildTime = "deterministic"

// entitySlicesCOW 为受影响图层复制实体 ID 与编码切片，避免写入共享后备数组。
func (b *artifactBuilder) entitySlicesCOW(layers map[string]struct{}) {
	for layerID := range layers {
		if ids := b.entityIDs[layerID]; ids != nil {
			b.entityIDs[layerID] = append([]string(nil), ids...)
		}
		if codes := b.entityWork[layerID]; codes != nil {
			b.entityWork[layerID] = append([]string(nil), codes...)
		}
	}
}

// artifactsEqual 判断两个产物的派生内容是否逐字段一致。
// Chunks 使用持久化映射，比较其摊平后的键值内容而非内部链结构；
// Revision 属于执行历史而非派生内容，可选忽略（revA/ revB 传入 0 比较）。
func artifactsEqual(a, b *Artifact) bool {
	return artifactsEqualRev(a, b, false)
}

// artifactsEqualRev 与 artifactsEqual 相同，但 includeRev=true 时也比较 Revision。
func artifactsEqualRev(a, b *Artifact, includeRev bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.FormatVersion != b.FormatVersion || a.BuiltAt != b.BuiltAt {
		return false
	}
	if includeRev && a.Revision != b.Revision {
		return false
	}
	if len(a.Chunks) != len(b.Chunks) {
		return false
	}
	for layerID, ma := range a.Chunks {
		mb, ok := b.Chunks[layerID]
		if !ok {
			return false
		}
		fa, fb := ma.flatten(), mb.flatten()
		if len(fa) != len(fb) {
			return false
		}
		for k, v := range fa {
			if fb[k] != v {
				return false
			}
		}
	}
	if len(a.EntityIndex) != len(b.EntityIndex) {
		return false
	}
	for layerID, la := range a.EntityIndex {
		lb, ok := b.EntityIndex[layerID]
		if !ok || len(la) != len(lb) {
			return false
		}
		for i := range la {
			if la[i] != lb[i] {
				return false
			}
		}
	}
	if len(a.ZoneList) != len(b.ZoneList) {
		return false
	}
	for i := range a.ZoneList {
		if a.ZoneList[i] != b.ZoneList[i] {
			return false
		}
	}
	return true
}
