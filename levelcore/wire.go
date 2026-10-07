package levelcore

import (
	"encoding/json"
	"fmt"
)

// commandEnvelope is the tagged-union wire form of a Command:
//
//	{"type":"set_tile","data":{...}}
//
// composites encode their sub-commands recursively.
type commandEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func envelopeOf(cmd Command) (commandEnvelope, error) {
	env := commandEnvelope{Type: cmd.name()}
	switch c := cmd.(type) {
	case *Composite:
		subs := make([]commandEnvelope, 0, len(c.Commands))
		for _, s := range c.Commands {
			se, err := envelopeOf(s)
			if err != nil {
				return commandEnvelope{}, err
			}
			subs = append(subs, se)
		}
		raw, err := json.Marshal(struct {
			Commands []commandEnvelope `json:"commands"`
		}{subs})
		if err != nil {
			return commandEnvelope{}, err
		}
		env.Data = raw
	default:
		raw, err := json.Marshal(cmd)
		if err != nil {
			return commandEnvelope{}, err
		}
		env.Data = raw
	}
	return env, nil
}

// decode parses an envelope. When primed is true (V1 stored history)
// the serialized prev-state is trusted as-is; when false (V0 replay)
// the first apply snapshots the pre-state from the live document.
func (e commandEnvelope) decode(primed bool) (Command, error) {
	if f, ok := commandRegistry[e.Type]; ok {
		cmd := f()
		if err := json.Unmarshal(e.Data, cmd); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Type, err)
		}
		if primed {
			markPrimed(cmd)
		}
		return cmd, nil
	}
	if e.Type == "composite" {
		var raw struct {
			Commands []commandEnvelope `json:"commands"`
		}
		if err := json.Unmarshal(e.Data, &raw); err != nil {
			return nil, err
		}
		subs := make([]Command, 0, len(raw.Commands))
		for i, se := range raw.Commands {
			s, err := se.decode(primed)
			if err != nil {
				return nil, fmt.Errorf("composite[%d]: %w", i, err)
			}
			subs = append(subs, s)
		}
		return &Composite{Commands: subs}, nil
	}
	return nil, fmt.Errorf("unknown command type %q", e.Type)
}

var commandRegistry = map[string]func() Command{
	"set_tile":         func() Command { return &SetTile{} },
	"set_tiles":        func() Command { return &SetTiles{} },
	"entity_add":       func() Command { return &EntityAdd{} },
	"entity_remove":    func() Command { return &EntityRemove{} },
	"entity_move":      func() Command { return &EntityMove{} },
	"entity_set_type":  func() Command { return &EntitySetType{} },
	"entity_set_owner": func() Command { return &EntitySetOwner{} },
	"region_add":       func() Command { return &RegionAdd{} },
	"region_remove":    func() Command { return &RegionRemove{} },
	"region_update":    func() Command { return &RegionUpdate{} },
}

// markPrimed flags a freshly decoded command so its first apply keeps
// the serialized prev-state instead of re-snapshotting (which matters
// for V0 replay: there the prevs are empty but should be populated on
// the first real apply; in V1 stored history they are primed).
func markPrimed(cmd Command) {
	switch c := cmd.(type) {
	case *SetTile:
		c.primed = c.HasPrev
	case *SetTiles:
		c.primed = len(c.Prevs) == len(c.Cells)
	case *EntityAdd:
		c.primed = true
	case *EntityRemove:
		c.primed = c.HasPrev
	case *EntityMove:
		c.primed = true
	case *EntitySetType:
		c.primed = true
	case *EntitySetOwner:
		c.primed = true
	case *RegionAdd:
		c.primed = true
	case *RegionRemove:
		c.primed = c.HasPrev
	case *RegionUpdate:
		c.primed = true
	}
}

// ---------------------------------------------------------------- doc wire

func docToWire(d *Doc) docWire {
	w := docWire{Width: d.Width, Height: d.Height}
	layerIDs := sortedStringKeys(d.Layers)
	for _, id := range layerIDs {
		w.Layers = append(w.Layers, layerWire{ID: id, Tiles: append([]Tile(nil), d.Layers[id].Tiles...)})
	}
	entIDs := sortedStringKeys(d.Entities)
	for _, id := range entIDs {
		w.Entities = append(w.Entities, entityWire{d.Entities[id]})
	}
	regIDs := sortedStringKeys(d.Regions)
	for _, id := range regIDs {
		w.Regions = append(w.Regions, regionWire{d.Regions[id]})
	}
	return w
}

func wireToDoc(w *docWire) *Doc {
	d := &Doc{
		Width:    w.Width,
		Height:   w.Height,
		Layers:   map[string]Layer{},
		Entities: map[string]Entity{},
		Regions:  map[string]Region{},
	}
	for _, l := range w.Layers {
		d.Layers[l.ID] = Layer{Tiles: append([]Tile(nil), l.Tiles...)}
	}
	for _, e := range w.Entities {
		d.Entities[e.ID] = e.Entity
	}
	for _, r := range w.Regions {
		d.Regions[r.ID] = r.Region
	}
	return d
}

func historyToWire(h *history) []entryWire {
	out := make([]entryWire, 0, len(h.done))
	for _, e := range h.done {
		env, err := envelopeOf(e.Command)
		if err != nil {
			// every concrete command is encodable; this is invariant
			panic("levelcore: cannot encode history command: " + err.Error())
		}
		out = append(out, entryWire{
			Seq:        e.Seq,
			Clock:      e.Clock,
			MergeCount: e.MergeCount,
			Kind:       e.Kind,
			Command:    env,
		})
	}
	return out
}
