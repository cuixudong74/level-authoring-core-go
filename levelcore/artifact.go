package levelcore

import (
	"fmt"
	"sort"
)

// Artifact is the derived build product of a document. It is fully a
// function of the document: full and incremental rebuilds produce
// byte-identical results.
type Artifact struct {
	// TileCounts[layer][terrain] is the number of cells of that
	// terrain on the layer (the empty terrain "" is counted as well).
	TileCounts map[string]map[string]int `json:"tile_counts"`
	// RegionIndex[regionID] lists the IDs of entities whose anchor
	// cell (top-left corner) lies inside the region area, sorted.
	RegionIndex map[string][]string `json:"region_index"`
	// EntityIndex holds derived per-entity information.
	EntityIndex map[string]EntityInfo `json:"entity_index"`
	// Digest is a deterministic 64-bit FNV-1a additive checksum over
	// the content above, rendered as lowercase hex.
	Digest string `json:"digest"`
}

// EntityInfo is the derived per-entity description.
type EntityInfo struct {
	CenterX int `json:"center_x"`
	CenterY int `json:"center_y"`
	// Inside reports whether the entity anchor is contained by its
	// owning region (false when it has no owner or the owner is gone).
	Inside bool `json:"inside_owner_region"`
}

const defaultGridSize = 32

// artifacts maintains the derived product together with the auxiliary
// indices that keep incremental rebuilds local:
//
//   - gridEnt/gridReg: spatial hash grids mapping cells to the entities
//     and regions that overlap them, so overlap and containment queries
//     cost proportional to local density instead of document size;
//   - regMembers: per-region set of contained entity IDs;
//   - additive FNV checksums: the digest is updated by removing the
//     old contribution and adding the new one for only changed items;
//   - regionCache: LRU cache of rendered (sorted) region member lists.
type artifacts struct {
	doc      *Doc
	gridSize int

	art *Artifact

	gridEnt map[int64][]string
	gridReg map[int64][]string

	regMembers map[string]map[string]struct{}
	// ownerIndex[regionID] = entities whose owner is that region
	// (independent of spatial containment)
	ownerIndex map[string]map[string]struct{}

	// additive digest state
	dsum         uint64
	entContrib   uint64
	regContrib   uint64
	tileContrib  uint64
	layerContrib uint64
	layerCount   int

	regionCache *lru[string, []string]

	// pending state captured before an apply/undo
	pend *pending
}

type pending struct {
	tiles    map[string]map[int]Tile
	existed  map[string]bool // layer existed before the change
	entities map[string]entitySnap
	regions  map[string]regionSnap
}

type entitySnap struct {
	ent    Entity
	exists bool
}

type regionSnap struct {
	reg    Region
	exists bool
}

func newArtifacts(d *Doc, cacheSize int) *artifacts {
	if cacheSize < 0 {
		cacheSize = 0
	}
	a := &artifacts{
		doc:         d,
		gridSize:    defaultGridSize,
		gridEnt:     map[int64][]string{},
		gridReg:     map[int64][]string{},
		regMembers:  map[string]map[string]struct{}{},
		ownerIndex:  map[string]map[string]struct{}{},
		regionCache: newLRU[string, []string](cacheSize),
	}
	a.full()
	return a
}

// ------------------------------------------------------------------- build

// full rebuilds the artifact and every auxiliary index from scratch.
func (a *artifacts) full() {
	d := a.doc
	a.art = &Artifact{
		TileCounts:  map[string]map[string]int{},
		RegionIndex: map[string][]string{},
		EntityIndex: map[string]EntityInfo{},
	}
	a.gridEnt = map[int64][]string{}
	a.gridReg = map[int64][]string{}
	a.regMembers = map[string]map[string]struct{}{}
	a.ownerIndex = map[string]map[string]struct{}{}
	a.regionCache = newLRU[string, []string](a.regionCache.capacity)

	// terrain
	a.tileContrib = 0
	layers := sortedKeysOfLayers(d)
	for _, lid := range layers {
		counts := map[string]int{}
		layer := d.Layers[lid]
		for _, t := range layer.Tiles {
			counts[t.Terrain]++
		}
		a.art.TileCounts[lid] = counts
		for terrain, n := range counts {
			a.tileContrib += tilePairHash(lid, terrain, n)
		}
	}
	a.layerCount = len(layers)
	a.layerContrib = mixHash(fnva64("layers"), uint64(a.layerCount))

	// entities: register first so regions can query them
	a.entContrib = 0
	entIDs := sortedStringKeys(d.Entities)
	for _, id := range entIDs {
		e := d.Entities[id]
		a.gridAdd(a.gridEnt, rectOf(e), id)
	}
	for _, id := range entIDs {
		a.art.EntityIndex[id] = a.entityInfoOf(d.Entities[id])
		a.entContrib += entityHash(d, d.Entities[id])
		if e := d.Entities[id]; e.Region != "" {
			a.ownerAdd(e.Region, id)
		}
	}

	// regions
	a.regContrib = 0
	regIDs := sortedStringKeys(d.Regions)
	for _, id := range regIDs {
		r := d.Regions[id]
		a.gridAdd(a.gridReg, r.Area, id)
		members := a.queryEntities(r.Area)
		set := map[string]struct{}{}
		for _, m := range members {
			set[m] = struct{}{}
		}
		a.regMembers[id] = set
		a.regionCache.put(id, members)
		a.art.RegionIndex[id] = cloneStrings(members)
		a.regContrib += a.regionHash(id)
	}

	a.art.Digest = a.finishDigest()
}

// ----------------------------------------------------------------- changes

// prepare captures the current values of everything the given touch
// set describes. It must be called before the command is applied or
// undone.
func (a *artifacts) prepare(t touchSet) {
	d := a.doc
	p := &pending{
		tiles:    map[string]map[int]Tile{},
		existed:  map[string]bool{},
		entities: map[string]entitySnap{},
		regions:  map[string]regionSnap{},
	}
	for layer, cells := range t.tiles {
		l, ok := d.Layers[layer]
		p.existed[layer] = ok
		snap := map[int]Tile{}
		if ok {
			for idx := range cells {
				snap[idx] = l.Tiles[idx]
			}
		} else {
			for idx := range cells {
				snap[idx] = Tile{}
			}
		}
		p.tiles[layer] = snap
	}
	for id := range t.entities {
		e, ok := d.Entities[id]
		p.entities[id] = entitySnap{ent: e, exists: ok}
	}
	for id := range t.regions {
		r, ok := d.Regions[id]
		p.regions[id] = regionSnap{reg: r, exists: ok}
	}
	a.pend = p
}

// commit diffs the captured before-state against the current document
// and applies the delta to the artifact and all indices. Cost is
// proportional to the touched set plus local spatial density.
func (a *artifacts) commit() {
	d := a.doc
	p := a.pend
	a.pend = nil

	// 1) terrain deltas
	for layer, cells := range p.tiles {
		cur, layerExists := d.Layers[layer]
		if !layerExists {
			// The layer disappeared on undo; every captured cell
			// reverted, and the layer contribution is removed whole.
			if counts, ok := a.art.TileCounts[layer]; ok {
				for terrain, n := range counts {
					a.tileContrib -= tilePairHash(layer, terrain, n)
				}
				delete(a.art.TileCounts, layer)
			}
			continue
		}
		counts := a.art.TileCounts[layer]
		if counts == nil {
			counts = map[string]int{}
			a.art.TileCounts[layer] = counts
		}
		if !p.existed[layer] {
			// The layer was born during this edit: every untouched cell
			// starts as empty terrain.
			empty := d.Width * d.Height
			counts[TerrainEmpty] = empty
			a.tileContrib += tilePairHash(layer, TerrainEmpty, empty)
		}
		for idx, before := range cells {
			after := cur.Tiles[idx]
			if before.Terrain == after.Terrain {
				continue
			}
			a.applyTerrainDelta(layer, counts, before.Terrain, after.Terrain)
		}
	}
	a.syncLayerCount(d)

	// 2) region deltas (before entity deltas, so new regions already
	// occupy the spatial grid when moved entities query it)
	for id, before := range p.regions {
		after, exists := d.Regions[id]
		switch {
		case !before.exists && exists:
			a.gridAdd(a.gridReg, after.Area, id)
			members := a.queryEntities(after.Area)
			set := make(map[string]struct{}, len(members))
			for _, m := range members {
				set[m] = struct{}{}
			}
			a.regMembers[id] = set
			a.regionCache.put(id, members)
			a.art.RegionIndex[id] = cloneStrings(members)
			a.regContrib += a.regionHash(id)
		case before.exists && !exists:
			a.gridRemove(a.gridReg, before.reg.Area, id)
			a.regContrib -= a.regionHash(id)
			delete(a.regMembers, id)
			delete(a.art.RegionIndex, id)
			a.regionCache.invalidate(id)
		case before.exists && exists:
			if before.reg.Area != after.Area {
				a.gridRemove(a.gridReg, before.reg.Area, id)
				a.gridAdd(a.gridReg, after.Area, id)
				newMembers := a.queryEntities(after.Area)
				a.replaceMembers(id, newMembers)
				// containment flags of owned entities may flip. The
				// old contribution must be computed against the OLD
				// region area; the doc already carries the new one.
				oldRegions := map[string]Region{id: before.reg}
				for ownerID := range a.ownerIndex[id] {
					ent := a.doc.Entities[ownerID]
					a.entContrib -= entityHashWithRegions(oldRegions, a.doc, ent)
					a.art.EntityIndex[ownerID] = a.entityInfoOf(ent)
					a.entContrib += entityHash(a.doc, ent)
				}
			}
		}
	}

	// 3) entity deltas
	for id, before := range p.entities {
		after, exists := d.Entities[id]
		switch {
		case !before.exists && exists:
			a.onEntityAdded(id, after)
		case before.exists && !exists:
			a.onEntityRemoved(id, before.ent)
		case before.exists && exists:
			if rectOf(before.ent) != rectOf(after) {
				a.gridRemove(a.gridEnt, rectOf(before.ent), id)
				a.gridAdd(a.gridEnt, rectOf(after), id)
				a.moveMembership(id, before.ent, after)
			}
			if before.ent.Region != after.Region {
				if before.ent.Region != "" {
					a.ownerRemove(before.ent.Region, id)
				}
				if after.Region != "" {
					a.ownerAdd(after.Region, id)
				}
			}
			a.entContrib -= entityHash(d, before.ent)
			a.art.EntityIndex[id] = a.entityInfoOf(after)
			a.entContrib += entityHash(d, after)
		}
	}

	a.art.Digest = a.finishDigest()
}

// syncLayerCount keeps the layer-count contribution consistent with
// layers born from SetTile commands.
func (a *artifacts) syncLayerCount(d *Doc) {
	n := len(d.Layers)
	if n != a.layerCount {
		a.layerContrib = mixHash(fnva64("layers"), uint64(n))
		a.layerCount = n
	}
}

// applyTerrainDelta adjusts the terrain histogram and its additive
// checksum for one cell changing oldTerrain -> newTerrain.
func (a *artifacts) applyTerrainDelta(layer string, counts map[string]int, oldTerrain, newTerrain string) {
	a.tileContrib -= tilePairHash(layer, oldTerrain, counts[oldTerrain])
	counts[oldTerrain]--
	if counts[oldTerrain] <= 0 {
		delete(counts, oldTerrain)
	} else {
		a.tileContrib += tilePairHash(layer, oldTerrain, counts[oldTerrain])
	}
	old := counts[newTerrain]
	if old > 0 {
		a.tileContrib -= tilePairHash(layer, newTerrain, old)
	}
	counts[newTerrain]++
	a.tileContrib += tilePairHash(layer, newTerrain, counts[newTerrain])
}

func (a *artifacts) onEntityAdded(id string, e Entity) {
	d := a.doc
	a.gridAdd(a.gridEnt, rectOf(e), id)
	a.art.EntityIndex[id] = a.entityInfoOf(e)
	a.entContrib += entityHash(d, e)
	if e.Region != "" {
		a.ownerAdd(e.Region, id)
	}
	for _, rid := range a.regionsAtPoint(e.X, e.Y) {
		a.memberAdd(rid, id)
	}
}

func (a *artifacts) onEntityRemoved(id string, e Entity) {
	d := a.doc
	a.gridRemove(a.gridEnt, rectOf(e), id)
	delete(a.art.EntityIndex, id)
	a.entContrib -= entityHash(d, e)
	if e.Region != "" {
		a.ownerRemove(e.Region, id)
	}
	for rid := range a.regMembers {
		if _, ok := a.regMembers[rid][id]; ok {
			a.memberRemove(rid, id)
		}
	}
}

func (a *artifacts) moveMembership(id string, oldE, newE Entity) {
	if oldE.X == newE.X && oldE.Y == newE.Y {
		return
	}
	before := a.regionsAtPoint(oldE.X, oldE.Y)
	after := a.regionsAtPoint(newE.X, newE.Y)
	for _, rid := range before {
		if !containsString(after, rid) {
			a.memberRemove(rid, id)
		}
	}
	for _, rid := range after {
		if !containsString(before, rid) {
			a.memberAdd(rid, id)
		}
	}
}

func (a *artifacts) ownerAdd(rid, id string) {
	set := a.ownerIndex[rid]
	if set == nil {
		set = map[string]struct{}{}
		a.ownerIndex[rid] = set
	}
	set[id] = struct{}{}
}

func (a *artifacts) ownerRemove(rid, id string) {
	if set, ok := a.ownerIndex[rid]; ok {
		delete(set, id)
		if len(set) == 0 {
			delete(a.ownerIndex, rid)
		}
	}
}

func (a *artifacts) memberAdd(rid, id string) {
	set := a.regMembers[rid]
	if set == nil {
		set = map[string]struct{}{}
		a.regMembers[rid] = set
	}
	if _, ok := set[id]; ok {
		return
	}
	a.regContrib -= a.regionHash(rid)
	set[id] = struct{}{}
	a.regionCache.invalidate(rid)
	a.art.RegionIndex[rid] = a.memberList(rid)
	a.regContrib += a.regionHash(rid)
}

func (a *artifacts) memberRemove(rid, id string) {
	set := a.regMembers[rid]
	if set == nil {
		return
	}
	if _, ok := set[id]; !ok {
		return
	}
	a.regContrib -= a.regionHash(rid)
	delete(set, id)
	a.regionCache.invalidate(rid)
	a.art.RegionIndex[rid] = a.memberList(rid)
	a.regContrib += a.regionHash(rid)
}

// replaceMembers swaps the member set of rid to the given sorted list,
// adjusting digest and rendered index.
func (a *artifacts) replaceMembers(rid string, members []string) {
	a.regContrib -= a.regionHash(rid)
	set := make(map[string]struct{}, len(members))
	for _, m := range members {
		set[m] = struct{}{}
	}
	a.regMembers[rid] = set
	a.regionCache.put(rid, cloneStrings(members))
	a.art.RegionIndex[rid] = cloneStrings(members)
	a.regContrib += a.regionHash(rid)
}

// memberList returns the sorted, cached member list of a region.
func (a *artifacts) memberList(rid string) []string {
	if v, ok := a.regionCache.get(rid); ok {
		return cloneStrings(v)
	}
	set := a.regMembers[rid]
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	a.regionCache.put(rid, cloneStrings(ids))
	return ids
}

// -------------------------------------------------------------- snapshot

// snapshot returns a deep copy of the current artifact.
func (a *artifacts) snapshot() Artifact {
	out := Artifact{
		TileCounts:  map[string]map[string]int{},
		RegionIndex: map[string][]string{},
		EntityIndex: map[string]EntityInfo{},
	}
	for layer, counts := range a.art.TileCounts {
		cp := make(map[string]int, len(counts))
		for k, v := range counts {
			cp[k] = v
		}
		out.TileCounts[layer] = cp
	}
	for rid := range a.regMembers {
		out.RegionIndex[rid] = a.memberList(rid)
	}
	for id, info := range a.art.EntityIndex {
		out.EntityIndex[id] = info
	}
	out.Digest = a.art.Digest
	return out
}

// ------------------------------------------------------------- queries

// gridCandidates returns the entity IDs registered in spatial cells
// overlapped by r.
func (a *artifacts) gridCandidates(r Rect) map[string]struct{} {
	out := map[string]struct{}{}
	for _, k := range a.cellsOf(r) {
		for _, id := range a.gridEnt[k] {
			out[id] = struct{}{}
		}
	}
	return out
}

func (a *artifacts) queryEntities(r Rect) []string {
	set := a.gridCandidates(r)
	ids := make([]string, 0, len(set))
	for id := range set {
		e := a.doc.Entities[id]
		if r.contains(e.X, e.Y) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (a *artifacts) regionsAtPoint(x, y int) []string {
	k := a.pointKey(x, y)
	var out []string
	for _, rid := range a.gridReg[k] {
		if r, ok := a.doc.Regions[rid]; ok && r.Area.contains(x, y) {
			out = append(out, rid)
		}
	}
	return out
}

func (a *artifacts) entityInfoOf(e Entity) EntityInfo {
	info := EntityInfo{
		CenterX: e.X + (e.Width-1)/2,
		CenterY: e.Y + (e.Height-1)/2,
	}
	if e.Region != "" {
		if r, ok := a.doc.Regions[e.Region]; ok {
			info.Inside = r.Area.contains(e.X, e.Y)
		}
	}
	return info
}

// ------------------------------------------------------------------ grid

func (a *artifacts) cellsOf(r Rect) []int64 {
	x0, y0 := r.X/a.gridSize, r.Y/a.gridSize
	x1 := (r.X + r.Width - 1) / a.gridSize
	y1 := (r.Y + r.Height - 1) / a.gridSize
	keys := make([]int64, 0, (x1-x0+1)*(y1-y0+1))
	for cx := x0; cx <= x1; cx++ {
		for cy := y0; cy <= y1; cy++ {
			keys = append(keys, packCell(cx, cy))
		}
	}
	return keys
}

func (a *artifacts) pointKey(x, y int) int64 {
	return packCell(x/a.gridSize, y/a.gridSize)
}

func packCell(cx, cy int) int64 {
	return int64(uint32(cx))<<32 | int64(uint32(cy))
}

func (a *artifacts) gridAdd(grid map[int64][]string, r Rect, id string) {
	for _, k := range a.cellsOf(r) {
		if !containsString(grid[k], id) {
			grid[k] = append(grid[k], id)
		}
	}
}

func (a *artifacts) gridRemove(grid map[int64][]string, r Rect, id string) {
	for _, k := range a.cellsOf(r) {
		list := grid[k]
		out := list[:0]
		for _, v := range list {
			if v != id {
				out = append(out, v)
			}
		}
		if len(out) == 0 {
			delete(grid, k)
		} else {
			grid[k] = out
		}
	}
}

// ---------------------------------------------------------------- digest

const fnvOffset = 1469598103934665603
const fnvPrime = 1099511628211

// fnva64 computes FNV-1a over a string.
func fnva64(s string) uint64 {
	h := uint64(fnvOffset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime
	}
	return h
}

func fnvaBytes(b []byte) uint64 {
	h := uint64(fnvOffset)
	for _, c := range b {
		h ^= uint64(c)
		h *= fnvPrime
	}
	return h
}

// mixHash folds an integer into a running FNV hash.
func mixHash(h, v uint64) uint64 {
	var buf [8]byte
	for i := 0; i < 8; i++ {
		buf[i] = byte(v >> (uint(i) * 8))
		h ^= uint64(buf[i])
		h *= fnvPrime
	}
	return h
}

func tilePairHash(layer, terrain string, count int) uint64 {
	h := fnva64("T")
	h = mixHash(h, fnva64(layer))
	h = mixHash(h, fnva64(terrain))
	h = mixHash(h, uint64(count))
	return h
}

// entityHashWithRegions computes the entity contribution while
// resolving the owner region from an overlay map first. It lets the
// region-resize path hash an entity against the OLD area while the
// document already holds the NEW area.
func entityHashWithRegions(overlay map[string]Region, d *Doc, e Entity) uint64 {
	h := fnva64("E")
	h = mixHash(h, fnva64(e.ID))
	h = mixHash(h, uint64(e.X))
	h = mixHash(h, uint64(e.Y))
	h = mixHash(h, uint64(e.Width))
	h = mixHash(h, uint64(e.Height))
	h = mixHash(h, fnva64(e.Type))
	h = mixHash(h, fnva64(e.Region))
	inside := uint64(0)
	if e.Region != "" {
		if r, ok := overlay[e.Region]; ok {
			if r.Area.contains(e.X, e.Y) {
				inside = 1
			}
		} else if r, ok := d.Regions[e.Region]; ok && r.Area.contains(e.X, e.Y) {
			inside = 1
		}
	}
	h = mixHash(h, inside)
	return h
}

func entityHash(d *Doc, e Entity) uint64 {
	return entityHashWithRegions(nil, d, e)
}

// regionHash is the additive contribution of one region: its identity
// folded with the number and checksum of its current members.
func (a *artifacts) regionHash(rid string) uint64 {
	h := fnva64("R")
	h = mixHash(h, fnva64(rid))
	set := a.regMembers[rid]
	h = mixHash(h, uint64(len(set)))
	var sum uint64
	for id := range set {
		sum += fnva64(id)
	}
	h = mixHash(h, sum)
	return h
}

func (a *artifacts) finishDigest() string {
	sum := a.tileContrib + a.layerContrib + a.entContrib + a.regContrib
	return fmt.Sprintf("%016x", sum)
}

// ---------------------------------------------------------------- helpers

func sortedStringKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOfLayers(d *Doc) []string {
	return sortedStringKeys(d.Layers)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
