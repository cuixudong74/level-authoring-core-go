package levelcore

import "fmt"

// touchSet records the document parts a command affects so artifacts
// can be rebuilt incrementally.
type touchSet struct {
	tiles    map[string]map[int]struct{}
	entities map[string]struct{}
	regions  map[string]struct{}
	rects    []Rect // entity footprints (old and new) near changed entities
}

func newTouchSet() touchSet {
	return touchSet{
		tiles:    map[string]map[int]struct{}{},
		entities: map[string]struct{}{},
		regions:  map[string]struct{}{},
	}
}

func (t touchSet) addTile(d *Doc, layer string, x, y int) {
	idx := y*d.Width + x
	s := t.tiles[layer]
	if s == nil {
		s = map[int]struct{}{}
		t.tiles[layer] = s
	}
	s[idx] = struct{}{}
}

func (t touchSet) addEntity(id string) { t.entities[id] = struct{}{} }
func (t touchSet) addRegion(id string) { t.regions[id] = struct{}{} }
func (t touchSet) addRect(r Rect)      { t.rects = append(t.rects, r) }

func (t touchSet) merge(o touchSet) {
	for layer, cells := range o.tiles {
		s := t.tiles[layer]
		if s == nil {
			s = map[int]struct{}{}
			t.tiles[layer] = s
		}
		for c := range cells {
			s[c] = struct{}{}
		}
	}
	for id := range o.entities {
		t.entities[id] = struct{}{}
	}
	for id := range o.regions {
		t.regions[id] = struct{}{}
	}
	t.rects = append(t.rects, o.rects...)
}

// Cell is one terrain cell inside a SetTiles batch.
type Cell struct {
	X       int    `json:"x"`
	Y       int    `json:"y"`
	Terrain string `json:"terrain"`
}

// Command is an invertible, deterministic document edit.
//
// The first apply primes the command's "previous" fields from the
// document; subsequent apply/undo calls use those stored values. This
// lets a command be constructed without knowing the prior state while
// keeping undo exact and JSON serialization complete.
type Command interface {
	apply(d *Doc) error
	undo(d *Doc) error
	mergeKey() string
	mergeWith(next Command) (Command, bool)
	touches(d *Doc) touchSet
	name() string
}

// ---------------------------------------------------------------- SetTile

// SetTile writes one terrain cell.
type SetTile struct {
	Layer        string `json:"layer"`
	X            int    `json:"x"`
	Y            int    `json:"y"`
	Terrain      string `json:"terrain"`
	Prev         Tile   `json:"prev"`
	HasPrev      bool   `json:"has_prev"`
	LayerExisted bool   `json:"layer_existed"`
	primed       bool
}

func (c *SetTile) name() string { return "set_tile" }

func (c *SetTile) apply(d *Doc) error {
	if !d.inBounds(c.X, c.Y) {
		return newError(KindOutOfBounds, "set_tile",
			fmt.Sprintf("cell (%d,%d) outside %dx%d", c.X, c.Y, d.Width, d.Height), -1)
	}
	if c.Layer == "" {
		return newError(KindInvalidArgument, "set_tile", "empty layer id", -1)
	}
	_, existed := d.Layers[c.Layer]
	d.EnsureLayer(c.Layer)
	if !c.primed {
		c.Prev = d.Layers[c.Layer].Tiles[c.Y*d.Width+c.X]
		c.HasPrev = true
		c.LayerExisted = existed
		c.primed = true
	}
	d.Layers[c.Layer].Tiles[c.Y*d.Width+c.X] = Tile{Terrain: c.Terrain}
	return nil
}

func (c *SetTile) undo(d *Doc) error {
	l, ok := d.Layers[c.Layer]
	if !ok {
		return newError(KindNotFound, "set_tile.undo", "layer missing: "+c.Layer, -1)
	}
	t := Tile{}
	if c.HasPrev {
		t = c.Prev
	}
	l.Tiles[c.Y*d.Width+c.X] = t
	if c.primed && !c.LayerExisted {
		delete(d.Layers, c.Layer)
	}
	return nil
}

func (c *SetTile) mergeKey() string { return "tile:" + c.Layer + ":" + itoa(c.X) + "," + itoa(c.Y) }

func (c *SetTile) mergeWith(next Command) (Command, bool) {
	n, ok := next.(*SetTile)
	if !ok || n.mergeKey() != c.mergeKey() {
		return nil, false
	}
	return &SetTile{
		Layer: c.Layer, X: c.X, Y: c.Y,
		Terrain: n.Terrain, Prev: c.Prev, HasPrev: c.HasPrev,
		LayerExisted: c.LayerExisted, primed: true,
	}, true
}

func (c *SetTile) touches(d *Doc) touchSet {
	t := newTouchSet()
	t.addTile(d, c.Layer, c.X, c.Y)
	return t
}

// ---------------------------------------------------------------- SetTiles

// SetTiles writes several terrain cells on one layer atomically.
type SetTiles struct {
	Layer        string `json:"layer"`
	Cells        []Cell `json:"cells"`
	Prevs        []Tile `json:"prevs"`
	LayerExisted bool   `json:"layer_existed"`
	primed       bool
}

func (c *SetTiles) name() string { return "set_tiles" }

func (c *SetTiles) apply(d *Doc) error {
	if c.Layer == "" {
		return newError(KindInvalidArgument, "set_tiles", "empty layer id", -1)
	}
	if len(c.Cells) == 0 {
		return newError(KindInvalidArgument, "set_tiles", "no cells", -1)
	}
	for i, cell := range c.Cells {
		if !d.inBounds(cell.X, cell.Y) {
			return newError(KindOutOfBounds, "set_tiles",
				fmt.Sprintf("cell (%d,%d) outside %dx%d", cell.X, cell.Y, d.Width, d.Height), i)
		}
	}
	_, existed := d.Layers[c.Layer]
	d.EnsureLayer(c.Layer)
	if !c.primed {
		c.LayerExisted = existed
		c.Prevs = make([]Tile, len(c.Cells))
		for i, cell := range c.Cells {
			c.Prevs[i] = d.Layers[c.Layer].Tiles[cell.Y*d.Width+cell.X]
		}
		c.primed = true
	}
	for _, cell := range c.Cells {
		d.Layers[c.Layer].Tiles[cell.Y*d.Width+cell.X] = Tile{Terrain: cell.Terrain}
	}
	return nil
}

func (c *SetTiles) undo(d *Doc) error {
	l, ok := d.Layers[c.Layer]
	if !ok {
		return newError(KindNotFound, "set_tiles.undo", "layer missing: "+c.Layer, -1)
	}
	prevs := c.Prevs
	if len(prevs) != len(c.Cells) {
		prevs = make([]Tile, len(c.Cells))
	}
	for i := len(c.Cells) - 1; i >= 0; i-- {
		cell := c.Cells[i]
		l.Tiles[cell.Y*d.Width+cell.X] = prevs[i]
	}
	if c.primed && !c.LayerExisted {
		delete(d.Layers, c.Layer)
	}
	return nil
}

func (c *SetTiles) mergeKey() string { return "tiles:" + c.Layer }

func (c *SetTiles) mergeWith(next Command) (Command, bool) {
	n, ok := next.(*SetTiles)
	if !ok || n.Layer != c.Layer {
		return nil, false
	}
	// Union cells by position; first-command prevs win.
	type key struct{ x, y int }
	order := []key{}
	cellAt := map[key]Cell{}
	prevAt := map[key]Tile{}
	hasPrev := map[key]bool{}
	for i, cell := range c.Cells {
		k := key{cell.X, cell.Y}
		if _, seen := cellAt[k]; !seen {
			order = append(order, k)
		}
		cellAt[k] = cell
		if c.primed && i < len(c.Prevs) {
			prevAt[k] = c.Prevs[i]
			hasPrev[k] = true
		}
	}
	for i, cell := range n.Cells {
		k := key{cell.X, cell.Y}
		if _, seen := cellAt[k]; !seen {
			order = append(order, k)
			if n.primed && i < len(n.Prevs) {
				prevAt[k] = n.Prevs[i]
				hasPrev[k] = true
			}
		}
		cellAt[k] = cell
	}
	m := &SetTiles{Layer: c.Layer, LayerExisted: c.LayerExisted, primed: true}
	for _, k := range order {
		cell := cellAt[k]
		m.Cells = append(m.Cells, cell)
		if hasPrev[k] {
			m.Prevs = append(m.Prevs, prevAt[k])
		} else {
			m.Prevs = append(m.Prevs, Tile{})
		}
	}
	return m, true
}

func (c *SetTiles) touches(d *Doc) touchSet {
	t := newTouchSet()
	for _, cell := range c.Cells {
		t.addTile(d, c.Layer, cell.X, cell.Y)
	}
	return t
}

// --------------------------------------------------------------- EntityAdd

// EntityAdd places a new entity (or restores a previously removed one
// when primed against a document that already contains the ID).
type EntityAdd struct {
	Entity  Entity `json:"entity"`
	HasPrev bool   `json:"has_prev"`
	Prev    Entity `json:"prev"`
	primed  bool
}

func (c *EntityAdd) name() string { return "entity_add" }

func (c *EntityAdd) apply(d *Doc) error {
	if !c.primed {
		old, exists := d.Entities[c.Entity.ID]
		c.HasPrev, c.Prev = exists, old
		c.primed = true
	}
	d.Entities[c.Entity.ID] = c.Entity
	return nil
}

func (c *EntityAdd) undo(d *Doc) error {
	if c.HasPrev {
		d.Entities[c.Entity.ID] = c.Prev
	} else {
		delete(d.Entities, c.Entity.ID)
	}
	return nil
}

func (c *EntityAdd) mergeKey() string                  { return "" }
func (c *EntityAdd) mergeWith(Command) (Command, bool) { return nil, false }

func (c *EntityAdd) touches(d *Doc) touchSet {
	t := newTouchSet()
	t.addEntity(c.Entity.ID)
	t.addRect(rectOf(c.Entity))
	return t
}

// ------------------------------------------------------------ EntityRemove

// EntityRemove removes an entity by ID; the removed entity is primed
// on first apply for exact undo.
type EntityRemove struct {
	ID      string `json:"id"`
	HasPrev bool   `json:"has_prev"`
	Entity  Entity `json:"entity"`
	primed  bool
}

func (c *EntityRemove) name() string { return "entity_remove" }

func (c *EntityRemove) apply(d *Doc) error {
	if !c.primed {
		ent, ok := d.Entities[c.ID]
		if !ok {
			return newError(KindNotFound, "entity_remove", "entity not found: "+c.ID, -1)
		}
		c.Entity, c.HasPrev, c.primed = ent, true, true
	}
	delete(d.Entities, c.ID)
	return nil
}

func (c *EntityRemove) undo(d *Doc) error {
	d.Entities[c.ID] = c.Entity
	return nil
}

func (c *EntityRemove) mergeKey() string                  { return "" }
func (c *EntityRemove) mergeWith(Command) (Command, bool) { return nil, false }

func (c *EntityRemove) touches(d *Doc) touchSet {
	t := newTouchSet()
	t.addEntity(c.ID)
	if c.HasPrev {
		t.addRect(rectOf(c.Entity))
	}
	return t
}

// -------------------------------------------------------------- EntityMove

// EntityMove changes an entity footprint (position and/or size).
type EntityMove struct {
	ID     string `json:"id"`
	From   Rect   `json:"from"`
	To     Rect   `json:"to"`
	primed bool
}

func (c *EntityMove) name() string { return "entity_move" }

func (c *EntityMove) apply(d *Doc) error {
	ent, ok := d.Entities[c.ID]
	if !ok {
		return newError(KindNotFound, "entity_move", "entity not found: "+c.ID, -1)
	}
	if !c.primed {
		c.From = rectOf(ent)
		c.primed = true
	}
	ent.X, ent.Y, ent.Width, ent.Height = c.To.X, c.To.Y, c.To.Width, c.To.Height
	d.Entities[c.ID] = ent
	return nil
}

func (c *EntityMove) undo(d *Doc) error {
	ent, ok := d.Entities[c.ID]
	if !ok {
		return newError(KindNotFound, "entity_move.undo", "entity not found: "+c.ID, -1)
	}
	ent.X, ent.Y, ent.Width, ent.Height = c.From.X, c.From.Y, c.From.Width, c.From.Height
	d.Entities[c.ID] = ent
	return nil
}

func (c *EntityMove) mergeKey() string { return "move:" + c.ID }

func (c *EntityMove) mergeWith(next Command) (Command, bool) {
	n, ok := next.(*EntityMove)
	if !ok || n.ID != c.ID {
		return nil, false
	}
	return &EntityMove{ID: c.ID, From: c.From, To: n.To, primed: true}, true
}

func (c *EntityMove) touches(d *Doc) touchSet {
	t := newTouchSet()
	t.addEntity(c.ID)
	t.addRect(c.From)
	t.addRect(c.To)
	return t
}

// ------------------------------------------------------------ EntitySetType

// EntitySetType changes the entity type.
type EntitySetType struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	PrevType string `json:"prev_type"`
	primed   bool
}

func (c *EntitySetType) name() string { return "entity_set_type" }

func (c *EntitySetType) apply(d *Doc) error {
	ent, ok := d.Entities[c.ID]
	if !ok {
		return newError(KindNotFound, "entity_set_type", "entity not found: "+c.ID, -1)
	}
	if !c.primed {
		c.PrevType = ent.Type
		c.primed = true
	}
	ent.Type = c.Type
	d.Entities[c.ID] = ent
	return nil
}

func (c *EntitySetType) undo(d *Doc) error {
	ent, ok := d.Entities[c.ID]
	if !ok {
		return newError(KindNotFound, "entity_set_type.undo", "entity not found: "+c.ID, -1)
	}
	ent.Type = c.PrevType
	d.Entities[c.ID] = ent
	return nil
}

func (c *EntitySetType) mergeKey() string { return "etype:" + c.ID }

func (c *EntitySetType) mergeWith(next Command) (Command, bool) {
	n, ok := next.(*EntitySetType)
	if !ok || n.ID != c.ID {
		return nil, false
	}
	return &EntitySetType{ID: c.ID, Type: n.Type, PrevType: c.PrevType, primed: true}, true
}

func (c *EntitySetType) touches(d *Doc) touchSet {
	t := newTouchSet()
	t.addEntity(c.ID)
	return t
}

// ----------------------------------------------------------- EntitySetOwner

// EntitySetOwner changes the owning region of an entity.
type EntitySetOwner struct {
	ID         string `json:"id"`
	Region     string `json:"region"`
	PrevRegion string `json:"prev_region"`
	primed     bool
}

func (c *EntitySetOwner) name() string { return "entity_set_owner" }

func (c *EntitySetOwner) apply(d *Doc) error {
	ent, ok := d.Entities[c.ID]
	if !ok {
		return newError(KindNotFound, "entity_set_owner", "entity not found: "+c.ID, -1)
	}
	if !c.primed {
		c.PrevRegion = ent.Region
		c.primed = true
	}
	ent.Region = c.Region
	d.Entities[c.ID] = ent
	return nil
}

func (c *EntitySetOwner) undo(d *Doc) error {
	ent, ok := d.Entities[c.ID]
	if !ok {
		return newError(KindNotFound, "entity_set_owner.undo", "entity not found: "+c.ID, -1)
	}
	ent.Region = c.PrevRegion
	d.Entities[c.ID] = ent
	return nil
}

func (c *EntitySetOwner) mergeKey() string { return "eowner:" + c.ID }

func (c *EntitySetOwner) mergeWith(next Command) (Command, bool) {
	n, ok := next.(*EntitySetOwner)
	if !ok || n.ID != c.ID {
		return nil, false
	}
	return &EntitySetOwner{ID: c.ID, Region: n.Region, PrevRegion: c.PrevRegion, primed: true}, true
}

func (c *EntitySetOwner) touches(d *Doc) touchSet {
	t := newTouchSet()
	t.addEntity(c.ID)
	// membership of the old and the new owning region may change
	if c.primed && c.PrevRegion != "" {
		t.addRegion(c.PrevRegion)
	}
	if c.Region != "" {
		t.addRegion(c.Region)
	}
	return t
}

// --------------------------------------------------------------- RegionAdd

// RegionAdd creates a marked region.
type RegionAdd struct {
	Region  Region `json:"region"`
	HasPrev bool   `json:"has_prev"`
	Prev    Region `json:"prev"`
	primed  bool
}

func (c *RegionAdd) name() string { return "region_add" }

func (c *RegionAdd) apply(d *Doc) error {
	if !c.primed {
		old, exists := d.Regions[c.Region.ID]
		c.HasPrev, c.Prev, c.primed = exists, old, true
	}
	d.Regions[c.Region.ID] = c.Region
	return nil
}

func (c *RegionAdd) undo(d *Doc) error {
	if c.HasPrev {
		d.Regions[c.Region.ID] = c.Prev
	} else {
		delete(d.Regions, c.Region.ID)
	}
	return nil
}

func (c *RegionAdd) mergeKey() string                  { return "" }
func (c *RegionAdd) mergeWith(Command) (Command, bool) { return nil, false }

func (c *RegionAdd) touches(d *Doc) touchSet {
	t := newTouchSet()
	t.addRegion(c.Region.ID)
	return t
}

// ------------------------------------------------------------ RegionRemove

// RegionRemove removes a region by ID.
type RegionRemove struct {
	ID      string `json:"id"`
	HasPrev bool   `json:"has_prev"`
	Region  Region `json:"region"`
	primed  bool
}

func (c *RegionRemove) name() string { return "region_remove" }

func (c *RegionRemove) apply(d *Doc) error {
	if !c.primed {
		r, ok := d.Regions[c.ID]
		if !ok {
			return newError(KindNotFound, "region_remove", "region not found: "+c.ID, -1)
		}
		c.Region, c.HasPrev, c.primed = r, true, true
	}
	delete(d.Regions, c.ID)
	return nil
}

func (c *RegionRemove) undo(d *Doc) error {
	d.Regions[c.ID] = c.Region
	return nil
}

func (c *RegionRemove) mergeKey() string                  { return "" }
func (c *RegionRemove) mergeWith(Command) (Command, bool) { return nil, false }

func (c *RegionRemove) touches(d *Doc) touchSet {
	t := newTouchSet()
	t.addRegion(c.ID)
	return t
}

// RegionUpdate changes the name and/or area of a region.
type RegionUpdate struct {
	ID     string `json:"id"`
	Prev   Region `json:"prev"`
	Next   Region `json:"next"`
	primed bool
}

func (c *RegionUpdate) name() string { return "region_update" }

func (c *RegionUpdate) apply(d *Doc) error {
	r, ok := d.Regions[c.ID]
	if !ok {
		return newError(KindNotFound, "region_update", "region not found: "+c.ID, -1)
	}
	if !c.primed {
		c.Prev, c.primed = r, true
	}
	c.Next.ID = c.ID
	d.Regions[c.ID] = c.Next
	return nil
}

func (c *RegionUpdate) undo(d *Doc) error {
	d.Regions[c.ID] = c.Prev
	return nil
}

func (c *RegionUpdate) mergeKey() string                  { return "" }
func (c *RegionUpdate) mergeWith(Command) (Command, bool) { return nil, false }

func (c *RegionUpdate) touches(d *Doc) touchSet {
	t := newTouchSet()
	t.addRegion(c.ID)
	t.addRect(c.Prev.Area)
	t.addRect(c.Next.Area)
	return t
}

// --------------------------------------------------------------- Composite

// Composite applies several commands as one atomic history entry.
type Composite struct {
	Commands []Command `json:"-"`
}

func (c *Composite) name() string { return "composite" }

func (c *Composite) apply(d *Doc) error {
	for i, sub := range c.Commands {
		if err := sub.apply(d); err != nil {
			return attachIndex(err, i)
		}
	}
	return nil
}

func (c *Composite) undo(d *Doc) error {
	for i := len(c.Commands) - 1; i >= 0; i-- {
		if err := c.Commands[i].undo(d); err != nil {
			return attachIndex(err, i)
		}
	}
	return nil
}

// composites never merge with neighbours
func (c *Composite) mergeKey() string                  { return "" }
func (c *Composite) mergeWith(Command) (Command, bool) { return nil, false }

func (c *Composite) touches(d *Doc) touchSet {
	t := newTouchSet()
	for _, sub := range c.Commands {
		t.merge(sub.touches(d))
	}
	return t
}

// ------------------------------------------------------------- small helpers

func rectOf(e Entity) Rect { return Rect{X: e.X, Y: e.Y, Width: e.Width, Height: e.Height} }

func attachIndex(err error, idx int) error {
	if ke, ok := err.(*Error); ok {
		cp := *ke
		cp.CommandIndex = idx
		return &cp
	}
	return err
}

// itoa avoids importing strconv solely for merge keys.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
