package levelcore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Bundle format versions.
const (
	// BundleVersionV1 is the current format: full document + history.
	BundleVersionV1 = 1
	// BundleVersionV0 is the legacy replay-only format:
	// {"version":0,"width":..,"height":..,"commands":[<wire commands>]}
	BundleVersionV0 = 0
)

// Maximum accepted bundle size (1 GiB) to keep corrupted length fields
// from driving unbounded allocations.
const maxBundleBytes = 1 << 30

// bundleV1 is the current envelope.
type bundleV1 struct {
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
	Payload string `json:"payload"` // hex(canonical JSON of payloadV1)
}

type payloadV1 struct {
	Format  int         `json:"format"`
	Doc     docWire     `json:"doc"`
	History []entryWire `json:"history"`
	Clock   int         `json:"clock"`
	Options optionsWire `json:"options"`
}

type docWire struct {
	Width    int          `json:"width"`
	Height   int          `json:"height"`
	Layers   []layerWire  `json:"layers"`
	Entities []entityWire `json:"entities"`
	Regions  []regionWire `json:"regions"`
}

type layerWire struct {
	ID    string `json:"id"`
	Tiles []Tile `json:"tiles"`
}

type entityWire struct{ Entity }
type regionWire struct{ Region }

type entryWire struct {
	Seq        int             `json:"seq"`
	Clock      int             `json:"clock"`
	MergeCount int             `json:"merge_count"`
	Kind       string          `json:"kind"`
	Command    commandEnvelope `json:"command"`
}

type optionsWire struct {
	HistoryLimit    int `json:"history_limit"`
	MergeWindow     int `json:"merge_window"`
	RegionCacheSize int `json:"region_cache_size"`
}

type bundleV0 struct {
	Version  int               `json:"version"`
	Width    int               `json:"width"`
	Height   int               `json:"height"`
	Commands []commandEnvelope `json:"commands"`
}

// MarshalBundle deterministically serializes the editor state into a
// versioned, checksummed bundle. Equal editor states always produce
// byte-identical output on any machine.
func MarshalBundle(e *Editor) ([]byte, error) {
	p := payloadV1{
		Format:  BundleVersionV1,
		Doc:     docToWire(e.doc),
		History: historyToWire(e.hist),
		Clock:   e.clock,
		Options: optionsWire{
			HistoryLimit:    e.hist.max,
			MergeWindow:     e.mergeWin,
			RegionCacheSize: e.arts.regionCache.capacity,
		},
	}
	body, err := canonicalJSON(p)
	if err != nil {
		return nil, newError(KindCorrupt, "MarshalBundle", err.Error(), -1)
	}
	sum := sha256.Sum256(body)
	env := bundleV1{
		Version: BundleVersionV1,
		SHA256:  hex.EncodeToString(sum[:]),
		Payload: hex.EncodeToString(body),
	}
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, newError(KindCorrupt, "MarshalBundle", err.Error(), -1)
	}
	return out, nil
}

// UnmarshalBundle parses a versioned bundle. It accepts the current
// V1 format and migrates legacy V0 replay bundles. Corrupt, truncated,
// tampered or unsupported input is rejected with a structured *Error
// and no partial state is exposed.
func UnmarshalBundle(data []byte) (*Editor, error) {
	if len(data) > maxBundleBytes {
		return nil, newError(KindCorrupt, "UnmarshalBundle", "bundle too large", -1)
	}
	head := struct {
		Version int `json:"version"`
	}{}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, newError(KindCorrupt, "UnmarshalBundle", "invalid JSON: "+err.Error(), -1)
	}
	switch head.Version {
	case BundleVersionV1:
		return unmarshalV1(data)
	case BundleVersionV0:
		return unmarshalV0(data)
	default:
		return nil, newError(KindUnsupportedVersion, "UnmarshalBundle",
			fmt.Sprintf("unsupported version %d", head.Version), -1)
	}
}

func unmarshalV1(data []byte) (*Editor, error) {
	const op = "UnmarshalBundle"
	var env bundleV1
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, newError(KindCorrupt, op, "invalid envelope: "+err.Error(), -1)
	}
	if env.SHA256 == "" || env.Payload == "" {
		return nil, newError(KindCorrupt, op, "missing checksum or payload", -1)
	}
	body, err := hex.DecodeString(env.Payload)
	if err != nil {
		return nil, newError(KindCorrupt, op, "payload is not hex: "+err.Error(), -1)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != env.SHA256 {
		return nil, newError(KindCorrupt, op, "checksum mismatch", -1)
	}
	var p payloadV1
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, newError(KindCorrupt, op, "invalid payload: "+err.Error(), -1)
	}
	if p.Format != BundleVersionV1 {
		return nil, newError(KindUnsupportedVersion, op,
			fmt.Sprintf("unsupported payload format %d", p.Format), -1)
	}

	doc := wireToDoc(&p.Doc)
	opts := Options{
		HistoryLimit:    p.Options.HistoryLimit,
		MergeWindow:     p.Options.MergeWindow,
		RegionCacheSize: p.Options.RegionCacheSize,
	}
	e, err := NewEditor(doc, opts)
	if err != nil {
		return nil, err
	}

	// Rebuild history from the wire log: entries are already primed,
	// so install them directly without touching the document. Entries
	// must be strictly ordered by seq with non-decreasing clocks.
	h := newHistory(opts.withDefaults().HistoryLimit)
	h.nextSeq = 1
	h.done = h.done[:0]
	prevClock := -1
	firstSeq := -1
	for i, ew := range p.History {
		if ew.Seq <= 0 {
			return nil, newError(KindCorrupt, op,
				"non-positive history seq", i)
		}
		if firstSeq < 0 {
			firstSeq = ew.Seq
		}
		if ew.Seq != firstSeq+i {
			return nil, newError(KindCorrupt, op,
				"history entries are not contiguously numbered", i)
		}
		if ew.Clock < prevClock {
			return nil, newError(KindCorrupt, op,
				"history clocks are not monotonic", i)
		}
		prevClock = ew.Clock
		if ew.MergeCount <= 0 {
			return nil, newError(KindCorrupt, op,
				"non-positive merge count", i)
		}
		cmd, err := ew.Command.decode(true)
		if err != nil {
			return nil, newError(KindCorrupt, op,
				"history["+itoa(ew.Seq)+"]: "+err.Error(), i)
		}
		h.done = append(h.done, &Entry{
			Seq:        ew.Seq,
			Clock:      ew.Clock,
			MergeCount: ew.MergeCount,
			Kind:       cmd.name(),
			Command:    cmd,
		})
		h.nextSeq = ew.Seq + 1
	}
	if len(h.done) > h.max {
		h.done = h.done[len(h.done)-h.max:]
	}
	e.hist = h
	e.clock = p.Clock
	return e, nil
}

func unmarshalV0(data []byte) (*Editor, error) {
	const op = "UnmarshalBundle.v0"
	var b bundleV0
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, newError(KindCorrupt, op, "invalid JSON: "+err.Error(), -1)
	}
	if b.Width <= 0 || b.Height <= 0 {
		return nil, newError(KindCorrupt, op, "invalid dimensions", -1)
	}
	doc, err := NewDoc(b.Width, b.Height)
	if err != nil {
		return nil, err
	}
	e, err := NewEditor(doc, Options{})
	if err != nil {
		return nil, err
	}
	// Legacy bundles predate merge tuning: keep default window but
	// replay each command as its own commit. Merged entries from the
	// legacy era cannot exist, so history matches 1:1.
	for i, env := range b.Commands {
		cmd, err := env.decode(false)
		if err != nil {
			return nil, newError(KindCorrupt, op,
				fmt.Sprintf("command[%d]: %s", i, err.Error()), i)
		}
		if err := e.Commit(cmd); err != nil {
			return nil, newError(KindCorrupt, op,
				fmt.Sprintf("command[%d] fails replay: %s", i, err.Error()), i)
		}
	}
	return e, nil
}

// ------------------------------------------------------------- canonical

// canonicalJSON serializes v with sorted map keys and stable struct
// ordering, then re-encodes via a round-trip so whitespace is fixed.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var anyVal any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&anyVal); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(anyVal); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
