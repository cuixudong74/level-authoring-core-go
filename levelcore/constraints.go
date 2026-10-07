package levelcore

import "sort"

// ConstraintSet 是文档必须满足的一致性约束集合。
//
// 约束规则（错误码稳定可区分）：
//  1. INVALID_VALUE  地图/图层尺寸必须为正；ID 非空；区域矩形非空。
//  2. OUT_OF_BOUNDS  图层不超出地图；实体坐标不超出其图层；区域不超出地图。
//  3. ILLEGAL_OWNER  实体必须归属已存在的图层；删除图层前其上不得有实体。
//  4. NOT_FOUND      被引用（移动/删除）的对象必须存在。
//  5. DUPLICATE_ID   图层/实体/区域的 ID 不得重复。
//  6. OVERLAP        同一图层上 Solid 实体不得占据同一格（单点模型）。
type ConstraintSet struct{}

// DefaultConstraints 返回默认约束集。
func DefaultConstraints() *ConstraintSet { return &ConstraintSet{} }

// ValidateAll 对整份文档做全量校验。
// 遍历顺序固定（LayerOrder/ZoneOrder 与排序后的实体 ID），
// 因此任意机器上对同一文档返回的首个违例完全一致。
func (c *ConstraintSet) ValidateAll(d *Document) *RejectError {
	if d.MapWidth <= 0 || d.MapHeight <= 0 {
		return rejectf(-1, RejectInvalidValue, "map size must be positive, got %dx%d", d.MapWidth, d.MapHeight)
	}
	if d.SchemaVersion < MinSupportedVersion || d.SchemaVersion > MaxSupportedVersion {
		return rejectf(-1, RejectUnknownVersion, "document schema version %d out of support [%d,%d]",
			d.SchemaVersion, MinSupportedVersion, MaxSupportedVersion)
	}

	// 图层：顺序、尺寸、越界。
	seen := make(map[string]bool, len(d.Layers))
	for _, id := range d.LayerOrder {
		l, ok := d.Layers[id]
		if !ok {
			return rejectf(-1, RejectNotFound, "layer order references missing layer %q", id)
		}
		if seen[id] {
			return rejectf(-1, RejectDuplicateID, "layer id %q appears more than once in order", id)
		}
		seen[id] = true
		if l.Width <= 0 || l.Height <= 0 {
			return rejectf(-1, RejectInvalidValue, "layer %q size must be positive, got %dx%d", id, l.Width, l.Height)
		}
		if l.Width > d.MapWidth || l.Height > d.MapHeight {
			return rejectf(-1, RejectOutOfBounds, "layer %q size %dx%d exceeds map %dx%d",
				id, l.Width, l.Height, d.MapWidth, d.MapHeight)
		}
	}
	if len(seen) != len(d.Layers) {
		ids := make([]string, 0, len(d.Layers))
		for id := range d.Layers {
			if !seen[id] {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		return rejectf(-1, RejectNotFound, "layers not present in order: %v", ids)
	}

	// 实体：按 ID 排序后校验归属、越界、重叠。
	entIDs := make([]string, 0, len(d.Entities))
	for id := range d.Entities {
		entIDs = append(entIDs, id)
	}
	sort.Strings(entIDs)
	occ := make(map[string]string, len(d.Entities))
	counts := make(map[string]int)
	for _, id := range entIDs {
		e := d.Entities[id]
		if e.ID != id {
			return rejectf(-1, RejectCorrupt, "entity key %q does not match id %q", id, e.ID)
		}
		if e.Kind == "" {
			return rejectf(-1, RejectInvalidValue, "entity %q kind must not be empty", id)
		}
		l, ok := d.Layers[e.LayerID]
		if !ok {
			return rejectf(-1, RejectIllegalOwner, "entity %q refers to unknown layer %q", id, e.LayerID)
		}
		if e.Pos.X < 0 || e.Pos.Y < 0 || e.Pos.X >= l.Width || e.Pos.Y >= l.Height {
			return rejectf(-1, RejectOutOfBounds,
				"entity %q at (%d,%d) outside layer %q size %dx%d",
				id, e.Pos.X, e.Pos.Y, e.LayerID, l.Width, l.Height)
		}
		counts[e.LayerID]++
		if e.Solid {
			k := occKey(e.LayerID, e.Pos)
			if holder, taken := occ[k]; taken {
				return rejectf(-1, RejectOverlap,
					"entity %q overlaps solid entity %q on layer %q at (%d,%d)",
					id, holder, e.LayerID, e.Pos.X, e.Pos.Y)
			}
			occ[k] = id
		}
	}

	// 区域：顺序、重复、矩形合法性、越界。
	zseen := make(map[string]bool, len(d.Zones))
	for _, id := range d.ZoneOrder {
		z, ok := d.Zones[id]
		if !ok {
			return rejectf(-1, RejectNotFound, "zone order references missing zone %q", id)
		}
		if zseen[id] {
			return rejectf(-1, RejectDuplicateID, "zone id %q appears more than once in order", id)
		}
		zseen[id] = true
		if z.Kind == "" {
			return rejectf(-1, RejectInvalidValue, "zone %q kind must not be empty", id)
		}
		if b := z.Bounds; b.Empty() {
			return rejectf(-1, RejectInvalidValue, "zone %q bounds must be non-empty", id)
		}
		if e := verifyZoneBounds(d, id, z.Bounds); e != nil {
			return e
		}
	}
	if len(zseen) != len(d.Zones) {
		ids := make([]string, 0)
		for id := range d.Zones {
			if !zseen[id] {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		return rejectf(-1, RejectNotFound, "zones not present in order: %v", ids)
	}
	return nil
}

// delta 表示一次提交对文档造成的增量，用于局部约束校验与产物增量重建。
type delta struct {
	// tileChunks: layerID -> 变更的地形块坐标集合
	tileChunks map[string]map[[2]uint32]struct{}
	// layersAdded / layersRemoved 记录图层增删。
	layersAdded   map[string]struct{}
	layersRemoved map[string]struct{}
	// entitiesPlaced / entitiesRemoved / entitiesMoved 记录实体变更。
	entitiesPlaced  map[string]struct{}
	entitiesRemoved map[string]struct{}
	entitiesMoved   map[string]struct{}
	// entityLayer 记录受影响实体当前/曾经所属的图层，供产物增量定位。
	entityLayer map[string]string
	// zonesAdded / zonesRemoved 记录区域增删。
	zonesAdded   map[string]struct{}
	zonesRemoved map[string]struct{}
}

func newDelta() *delta {
	return &delta{
		tileChunks:      map[string]map[[2]uint32]struct{}{},
		layersAdded:     map[string]struct{}{},
		layersRemoved:   map[string]struct{}{},
		entitiesPlaced:  map[string]struct{}{},
		entitiesRemoved: map[string]struct{}{},
		entitiesMoved:   map[string]struct{}{},
		entityLayer:     map[string]string{},
		zonesAdded:      map[string]struct{}{},
		zonesRemoved:    map[string]struct{}{},
	}
}

// markTile 在增量中登记一个发生变化的地形块。
func (dl *delta) markTile(layerID string, cx, cy uint32) {
	m := dl.tileChunks[layerID]
	if m == nil {
		m = map[[2]uint32]struct{}{}
		dl.tileChunks[layerID] = m
	}
	m[chunkKey2(cx, cy)] = struct{}{}
}

// ValidateDelta 只校验增量涉及的对象，复杂度只与"本次改动量"相关，
// 不随文档总规模线性增长。调用方为已在最终文档上完成全部 apply 的批次。
// before 为批次前文档，用于判断改动前后归属等信息；可为 nil（全量语境）。
func (c *ConstraintSet) ValidateDelta(d *Document, before *Document, dl *delta) *RejectError {
	// 1) 新增图层：尺寸与越界。
	for _, id := range sortedKeys(dl.layersAdded) {
		l := d.Layers[id]
		if l == nil {
			continue
		}
		if l.Width <= 0 || l.Height <= 0 {
			return rejectf(-1, RejectInvalidValue, "layer %q size must be positive, got %dx%d", id, l.Width, l.Height)
		}
		if l.Width > d.MapWidth || l.Height > d.MapHeight {
			return rejectf(-1, RejectOutOfBounds, "layer %q size %dx%d exceeds map %dx%d",
				id, l.Width, l.Height, d.MapWidth, d.MapHeight)
		}
	}
	// 2) 删除图层：最终文档中该图层不得仍被实体引用（计数索引 O(1)）。
	for _, id := range sortedKeys(dl.layersRemoved) {
		if n := d.layerEntities[id]; n > 0 {
			return rejectf(-1, RejectIllegalOwner,
				"cannot remove layer %q: %d entities still belong to it", id, n)
		}
	}
	// 3) 受影响实体：归属、越界、重叠。
	affected := make(map[string]struct{},
		len(dl.entitiesPlaced)+len(dl.entitiesMoved)+len(dl.entitiesRemoved))
	for id := range dl.entitiesPlaced {
		affected[id] = struct{}{}
	}
	for id := range dl.entitiesMoved {
		affected[id] = struct{}{}
	}
	for id := range dl.entitiesRemoved {
		affected[id] = struct{}{}
	}
	for _, id := range sortedKeys(affected) {
		// 已删除的实体：它持有的格子与归属已被释放，不再参与最终校验。
		if _, gone := dl.entitiesRemoved[id]; gone {
			if _, still := d.Entities[id]; still {
				// 批次中先删后建等情形：以最终存在为准继续校验。
			} else {
				continue
			}
		}
		e := d.Entities[id]
		if e == nil {
			continue
		}
		if err := verifyEntityBounds(d, e); err != nil {
			return err
		}
	}
	// 4) 新增区域：合法性与越界。
	for _, id := range sortedKeys(dl.zonesAdded) {
		z := d.Zones[id]
		if z == nil {
			continue
		}
		if err := verifyZoneBounds(d, id, z.Bounds); err != nil {
			return err
		}
	}
	return nil
}
