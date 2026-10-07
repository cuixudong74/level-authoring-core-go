package levelcore

import (
	"fmt"
	"sort"
)

// Consistency rules enforced on every edit (both single and batched):
//
//  1. Bounds      - every referenced cell, entity footprint and region
//     area must lie inside the document and be non-empty.
//  2. Existence   - mutations must reference layers/entities/regions
//     that exist; additions must not reuse an ID.
//  3. Reference   - an entity owner must be an existing region ID.
//  4. Ownership   - a region may only be removed when no entity is
//     owned by it.
//  5. Placement   - two entities must not overlap each other.
//
// Empty terrain cells are allowed to overlap across layers; layers are
// independent terrains and never conflict with entities.
//
// validate checks cmd against the current document. It never mutates.
// The returned *Error carries the command index inside the batch.
func validate(d *Doc, a *artifacts, cmd Command, idx int) error {
	switch c := cmd.(type) {
	case *SetTile:
		if err := checkCell(d, c.Layer, c.X, c.Y, idx); err != nil {
			return err
		}
	case *SetTiles:
		if c.Layer == "" {
			return rej(KindInvalidArgument, idx, "set_tiles", "empty layer id")
		}
		if len(c.Cells) == 0 {
			return rej(KindInvalidArgument, idx, "set_tiles", "no cells")
		}
		for _, cell := range c.Cells {
			if err := checkCell(d, c.Layer, cell.X, cell.Y, idx); err != nil {
				return err
			}
		}
	case *EntityAdd:
		if err := checkEntityShape(d, c.Entity, idx, "entity_add"); err != nil {
			return err
		}
		if _, exists := d.Entities[c.Entity.ID]; exists {
			return rej(KindDuplicate, idx, "entity_add",
				"entity id already exists: "+c.Entity.ID)
		}
		if c.Entity.Region != "" {
			if _, ok := d.Regions[c.Entity.Region]; !ok {
				return rej(KindBadReference, idx, "entity_add",
					"owner region does not exist: "+c.Entity.Region)
			}
		}
		if err := checkOverlap(d, a, c.Entity.ID, rectOf(c.Entity), idx, "entity_add"); err != nil {
			return err
		}
	case *EntityRemove:
		if _, ok := d.Entities[c.ID]; !ok {
			return rej(KindNotFound, idx, "entity_remove", "entity not found: "+c.ID)
		}
	case *EntityMove:
		ent, ok := d.Entities[c.ID]
		if !ok {
			return rej(KindNotFound, idx, "entity_move", "entity not found: "+c.ID)
		}
		if !d.rectInBounds(c.To) {
			return rej(KindOutOfBounds, idx, "entity_move",
				fmt.Sprintf("target %+v outside %dx%d", c.To, d.Width, d.Height))
		}
		ent.X, ent.Y, ent.Width, ent.Height = c.To.X, c.To.Y, c.To.Width, c.To.Height
		if err := checkOverlap(d, a, c.ID, c.To, idx, "entity_move"); err != nil {
			return err
		}
	case *EntitySetType:
		if _, ok := d.Entities[c.ID]; !ok {
			return rej(KindNotFound, idx, "entity_set_type", "entity not found: "+c.ID)
		}
		if c.Type == "" {
			return rej(KindInvalidArgument, idx, "entity_set_type", "empty type")
		}
	case *EntitySetOwner:
		if _, ok := d.Entities[c.ID]; !ok {
			return rej(KindNotFound, idx, "entity_set_owner", "entity not found: "+c.ID)
		}
		if c.Region != "" {
			if _, ok := d.Regions[c.Region]; !ok {
				return rej(KindBadReference, idx, "entity_set_owner",
					"owner region does not exist: "+c.Region)
			}
		}
	case *RegionAdd:
		if c.Region.ID == "" {
			return rej(KindInvalidArgument, idx, "region_add", "empty region id")
		}
		if _, exists := d.Regions[c.Region.ID]; exists {
			return rej(KindDuplicate, idx, "region_add",
				"region id already exists: "+c.Region.ID)
		}
		if !d.rectInBounds(c.Region.Area) {
			return rej(KindOutOfBounds, idx, "region_add",
				fmt.Sprintf("region area %+v outside %dx%d", c.Region.Area, d.Width, d.Height))
		}
	case *RegionRemove:
		if _, ok := d.Regions[c.ID]; !ok {
			return rej(KindNotFound, idx, "region_remove", "region not found: "+c.ID)
		}
		if owners := entitiesOwnedBy(d, c.ID); len(owners) > 0 {
			sort.Strings(owners)
			return rej(KindIllegalOwner, idx, "region_remove",
				fmt.Sprintf("region %s still owns entities: %v", c.ID, owners))
		}
	case *RegionUpdate:
		r, ok := d.Regions[c.ID]
		if !ok {
			return rej(KindNotFound, idx, "region_update", "region not found: "+c.ID)
		}
		next := c.Next
		next.ID = c.ID
		if !d.rectInBounds(next.Area) {
			return rej(KindOutOfBounds, idx, "region_update",
				fmt.Sprintf("region area %+v outside %dx%d", next.Area, d.Width, d.Height))
		}
		_ = r
	case *Composite:
		// Composite validity is guaranteed by the per-command
		// simulation loop in Editor.commit; this case exists only so a
		// stray validate call reports something useful.
		return rej(KindInvalidArgument, idx, "validate", "composite must be validated per sub-command")
	default:
		return rej(KindInvalidArgument, idx, "validate", "unknown command type")
	}
	return nil
}

func checkCell(d *Doc, layer string, x, y int, idx int) error {
	if layer == "" {
		return rej(KindInvalidArgument, idx, "set_tile", "empty layer id")
	}
	if !d.inBounds(x, y) {
		return rej(KindOutOfBounds, idx, "set_tile",
			fmt.Sprintf("cell (%d,%d) outside %dx%d", x, y, d.Width, d.Height))
	}
	return nil
}

func checkEntityShape(d *Doc, e Entity, idx int, op string) error {
	if e.ID == "" {
		return rej(KindInvalidArgument, idx, op, "empty entity id")
	}
	if e.Type == "" {
		return rej(KindInvalidArgument, idx, op, "empty entity type for "+e.ID)
	}
	r := rectOf(e)
	if r.Width <= 0 || r.Height <= 0 {
		return rej(KindInvalidArgument, idx, op,
			"entity footprint must be positive: "+e.ID)
	}
	if !d.rectInBounds(r) {
		return rej(KindOutOfBounds, idx, op,
			fmt.Sprintf("entity %s footprint %+v outside %dx%d", e.ID, r, d.Width, d.Height))
	}
	return nil
}

// checkOverlap verifies that moving/adding selfID at rect r does not
// intersect any other existing entity. Candidate lookup uses the
// artifact spatial index so the cost is proportional to local density,
// not total entity count.
func checkOverlap(d *Doc, a *artifacts, selfID string, r Rect, idx int, op string) error {
	var candidates map[string]struct{}
	if a != nil {
		candidates = a.gridCandidates(r)
	} else {
		candidates = make(map[string]struct{}, len(d.Entities))
		for id := range d.Entities {
			candidates[id] = struct{}{}
		}
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		if id != selfID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids) // deterministic rejection message
	for _, id := range ids {
		other := d.Entities[id]
		if rectsOverlap(r, rectOf(other)) {
			return rej(KindOverlap, idx, op,
				fmt.Sprintf("%s overlaps existing entity %s", selfID, id))
		}
	}
	return nil
}

func entitiesOwnedBy(d *Doc, regionID string) []string {
	var out []string
	for id, e := range d.Entities {
		if e.Region == regionID {
			out = append(out, id)
		}
	}
	return out
}

func rej(kind Kind, idx int, op, detail string) *Error {
	return newError(kind, op, detail, idx)
}
