package levelcore

// Tile is a single terrain cell on one layer. A zero-valued Tile is an
// empty cell (Terrain == TerrainEmpty).
type Tile struct {
	Terrain string `json:"terrain"`
}

// Standard terrain sentinel: empty cell.
const TerrainEmpty = ""

// Entity is a placed object. Its footprint is the inclusive rectangle
// [X,X+Width) x [Y,Y+Height). Width and Height are always >= 1 for a
// valid entity. Region is the owning region ID (may be empty).
type Entity struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Region string `json:"region"`
}

// Rect is an inclusive-exclusive rectangle [X,X+Width) x [Y,Y+Height).
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (r Rect) contains(x, y int) bool {
	return x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height
}

func rectsOverlap(a, b Rect) bool {
	return a.X < b.X+b.Width && b.X < a.X+a.Width &&
		a.Y < b.Y+b.Height && b.Y < a.Y+a.Height
}

// Region is a named marked area.
type Region struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Area Rect   `json:"area"`
}

// Doc is the editable level document: multi-layer terrain, entity
// placement and region markers.
type Doc struct {
	Width    int               `json:"width"`
	Height   int               `json:"height"`
	Layers   map[string]Layer  `json:"layers"`
	Entities map[string]Entity `json:"entities"`
	Regions  map[string]Region `json:"regions"`
}

// Layer is a row-major terrain grid of Width*Height cells; cells are
// indexed by y*Width+x.
type Layer struct {
	Tiles []Tile `json:"tiles"`
}

// NewDoc creates an empty document with a positive, bounded size.
func NewDoc(width, height int) (*Doc, error) {
	if width <= 0 || height <= 0 {
		return nil, newError(KindInvalidArgument, "NewDoc", "width and height must be positive", -1)
	}
	d := &Doc{
		Width:    width,
		Height:   height,
		Layers:   map[string]Layer{},
		Entities: map[string]Entity{},
		Regions:  map[string]Region{},
	}
	return d, nil
}

// Clone returns a deep, independent copy of the document.
func (d *Doc) Clone() *Doc {
	c := &Doc{
		Width:    d.Width,
		Height:   d.Height,
		Layers:   make(map[string]Layer, len(d.Layers)),
		Entities: make(map[string]Entity, len(d.Entities)),
		Regions:  make(map[string]Region, len(d.Regions)),
	}
	for id, l := range d.Layers {
		t := make([]Tile, len(l.Tiles))
		copy(t, l.Tiles)
		c.Layers[id] = Layer{Tiles: t}
	}
	for id, e := range d.Entities {
		c.Entities[id] = e
	}
	for id, r := range d.Regions {
		c.Regions[id] = r
	}
	return c
}

// EnsureLayer creates the named terrain layer with empty tiles if it
// does not yet exist.
func (d *Doc) EnsureLayer(id string) {
	if _, ok := d.Layers[id]; ok {
		return
	}
	d.Layers[id] = Layer{Tiles: make([]Tile, d.Width*d.Height)}
}

// inBounds reports whether a single cell lies within the document.
func (d *Doc) inBounds(x, y int) bool {
	return x >= 0 && y >= 0 && x < d.Width && y < d.Height
}

// rectInBounds reports whether r is non-empty and fully inside the doc.
func (d *Doc) rectInBounds(r Rect) bool {
	if r.Width <= 0 || r.Height <= 0 {
		return false
	}
	return r.X >= 0 && r.Y >= 0 &&
		r.X+r.Width <= d.Width && r.Y+r.Height <= d.Height
}
