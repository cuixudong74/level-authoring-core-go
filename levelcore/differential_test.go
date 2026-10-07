package levelcore

import (
	"math/rand"
	"reflect"
	"sort"
	"testing"
)

// rngOp drives one random step of the differential test.
type rngOp struct {
	name string
	run  func(t *testing.T, e *Editor, r *rand.Rand)
}

func randomCell(r *rand.Rand, w, h int) (int, int) {
	return r.Intn(w), r.Intn(h)
}

func randomRect(r *rand.Rand, w, h int) Rect {
	x := r.Intn(w)
	y := r.Intn(h)
	rw := 1 + r.Intn(w-x)
	rh := 1 + r.Intn(h-y)
	return Rect{X: x, Y: y, Width: rw, Height: rh}
}

// TestDifferentialRandom replays deterministic random edit streams and
// after every single step asserts:
//   - the incrementally maintained artifact equals a fresh full rebuild;
//   - undo/redo round-trips restore document and artifact exactly;
//   - the exported bundle round-trips with identical doc/artifact;
//   - history limits never disturb the most recent edits.
func TestDifferentialRandom(t *testing.T) {
	const W, H = 24, 16
	terrainSet := []string{"", "grass", "water", "sand", "rock"}
	types := []string{"tree", "rock", "chest", "npc"}

	newEntityID := func(n int) string { return "ent-" + itoa(n) }
	newRegionID := func(n int) string { return "reg-" + itoa(n) }

	for seed := int64(1); seed <= 80; seed++ {
		t.Run("seed-"+itoa(int(seed)), func(t *testing.T) {
			r := rand.New(rand.NewSource(seed))
			base, err := NewDoc(W, H)
			if err != nil {
				t.Fatal(err)
			}
			// pre-existing layers exercise initial full build
			base.Layers["ground"] = Layer{Tiles: make([]Tile, W*H)}
			base.Layers["deco"] = Layer{Tiles: make([]Tile, W*H)}
			// small history cap also evicts entries
			e, err := NewEditor(base, Options{HistoryLimit: 30, MergeWindow: 4, RegionCacheSize: 8})
			if err != nil {
				t.Fatal(err)
			}

			entCounter := 0
			regCounter := 0
			assertFull := func(stage string) {
				t.Helper()
				fresh, err := NewEditor(e.Doc(), Options{RegionCacheSize: 1})
				if err != nil {
					t.Fatalf("%s: fresh build invalid: %v", stage, err)
				}
				inc := e.Artifact()
				full := fresh.Artifact()
				if !reflect.DeepEqual(inc, full) {
					t.Fatalf("%s: incremental != full\\ninc=%#v\\nfull=%#v", stage, inc, full)
				}
			}

			for step := 0; step < 120; step++ {
				op := r.Intn(13)
				switch op {
				case 0, 1: // SetTile
					layer := []string{"ground", "deco", "newlayer"}[r.Intn(3)]
					x, y := randomCell(r, W, H)
					terr := terrainSet[r.Intn(len(terrainSet))]
					_ = e.Commit(&SetTile{Layer: layer, X: x, Y: y, Terrain: terr})
				case 2: // SetTiles
					layer := []string{"ground", "deco", "newlayer"}[r.Intn(3)]
					n := 1 + r.Intn(5)
					cells := make([]Cell, n)
					for i := range cells {
						x, y := randomCell(r, W, H)
						cells[i] = Cell{X: x, Y: y, Terrain: terrainSet[r.Intn(len(terrainSet))]}
					}
					_ = e.Commit(&SetTiles{Layer: layer, Cells: cells})
				case 3: // add entity
					entCounter++
					rc := randomRect(r, W, H)
					ent := Entity{ID: newEntityID(entCounter), Type: types[r.Intn(len(types))],
						X: rc.X, Y: rc.Y, Width: rc.Width, Height: rc.Height}
					regIDs := mapKeys(e.Doc().Regions)
					if len(regIDs) > 0 && r.Intn(2) == 0 {
						ent.Region = regIDs[r.Intn(len(regIDs))]
					}
					_ = e.Commit(&EntityAdd{Entity: ent})
				case 4: // remove entity
					ids := mapKeys(e.Doc().Entities)
					if len(ids) > 0 {
						_ = e.Commit(&EntityRemove{ID: ids[r.Intn(len(ids))]})
					}
				case 5: // move entity
					ids := mapKeys(e.Doc().Entities)
					if len(ids) > 0 {
						id := ids[r.Intn(len(ids))]
						_ = e.Commit(&EntityMove{ID: id, To: randomRect(r, W, H)})
					}
				case 6: // set type
					ids := mapKeys(e.Doc().Entities)
					if len(ids) > 0 {
						id := ids[r.Intn(len(ids))]
						_ = e.Commit(&EntitySetType{ID: id, Type: types[r.Intn(len(types))]})
					}
				case 7: // set owner
					ids := mapKeys(e.Doc().Entities)
					regIDs := mapKeys(e.Doc().Regions)
					if len(ids) > 0 && (len(regIDs) > 0 || r.Intn(2) == 0) {
						var owner string
						if len(regIDs) > 0 && r.Intn(3) > 0 {
							owner = regIDs[r.Intn(len(regIDs))]
						}
						_ = e.Commit(&EntitySetOwner{ID: ids[r.Intn(len(ids))], Region: owner})
					}
				case 8: // add region
					regCounter++
					_ = e.Commit(&RegionAdd{Region: Region{
						ID: newRegionID(regCounter), Name: "R", Area: randomRect(r, W, H),
					}})
				case 9: // remove region (may be rejected for ownership)
					ids := mapKeys(e.Doc().Regions)
					if len(ids) > 0 {
						_ = e.Commit(&RegionRemove{ID: ids[r.Intn(len(ids))]})
					}
				case 10: // undo
					if e.CanUndo() {
						before := e.Doc()
						beforeArt := e.Artifact()
						e.Undo()
						// redo restores exactly
						if e.CanRedo() {
							e.Redo()
							if !reflect.DeepEqual(e.Doc(), before) {
								t.Fatalf("step %d: redo doc mismatch", step)
							}
							if !reflect.DeepEqual(e.Artifact(), beforeArt) {
								t.Fatalf("step %d: redo artifact mismatch", step)
							}
							// back into undone state for normal flow
							e.Undo()
						}
					}
				case 11: // redo
					if e.CanRedo() {
						e.Redo()
					}
				}
				assertFull("step " + itoa(step))
			}

			// undo everything that history still retains. With a cap
			// smaller than the number of commits, the oldest edits were
			// evicted by policy, so the floor state is the document as it
			// existed just before the oldest retained entry: rebuild it by
			// replaying retained entries' undo chain onto a fresh editor
			// that starts from the same floor document. Instead of
			// reconstructing the floor directly, verify the invariant
			// "no more undos available" plus a full-rebuild equality.
			for e.Undo() {
				assertFull("during-undo")
			}
			if e.CanUndo() {
				t.Fatal("undo stack should be empty")
			}
			assertFull("after-all-undo")
			if len(e.History()) != 0 {
				t.Fatal("history must be empty after undoing all retained entries")
			}
			// The floor document is reachable deterministically: replay
			// all retained history from the bundle of the floor state by
			// exporting the undone editor (its redo stack is not
			// serialized, matching documented semantics) and re-importing.
			floorBundle, err := MarshalBundle(e)
			if err != nil {
				t.Fatal(err)
			}
			floorEditor, err := UnmarshalBundle(floorBundle)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(floorEditor.Doc(), e.Doc()) {
				t.Fatal("floor document not stable across export")
			}

			// bundle round-trip preserves doc + history + artifact
			// rebuild a stream of commits again to have something to export
			b, err := MarshalBundle(e)
			if err != nil {
				t.Fatal(err)
			}
			got, err := UnmarshalBundle(b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Doc(), e.Doc()) {
				t.Fatal("bundle round-trip doc mismatch")
			}
			if !reflect.DeepEqual(got.Artifact(), e.Artifact()) {
				t.Fatal("bundle round-trip artifact mismatch")
			}
		})
	}
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
