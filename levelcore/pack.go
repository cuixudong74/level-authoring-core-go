package levelcore

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
)

// 二进制包格式（大端，全部定长字段）：
//
//	偏移  长度  字段
//	0     4     magic "LVPK"
//	4     2     version (uint16 BE)
//	6     4     payloadLen (uint32 BE)
//	10    N     payload（规范化 JSON，见 packPayload）
//	10+N  32    sha256(payload)
const (
	packMagic     = "LVPK"
	packHeaderLen = 10
	packHashLen   = 32
)

// packPayload 是 JSON 载荷的确定性结构。
type packPayload struct {
	FormatVersion uint16          `json:"formatVersion"`
	Document      json.RawMessage `json:"document"`
	Artifact      json.RawMessage `json:"artifact"`
}

// docDTO 是文档序列化/反序列化的数据形态。
type docDTO struct {
	SchemaVersion uint16          `json:"schemaVersion"`
	MapWidth      int32           `json:"mapWidth"`
	MapHeight     int32           `json:"mapHeight"`
	Layers        []layerDTO      `json:"layers"`
	Entities      []entityDTO     `json:"entities"`
	Zones         []zoneDTO       `json:"zones"`
	Tiles         []layerTilesDTO `json:"tiles"`
}

type layerDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Width  int32  `json:"width"`
	Height int32  `json:"height"`
}

type pointDTO struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
}

type rectDTO struct {
	MinX int32 `json:"minX"`
	MinY int32 `json:"minY"`
	MaxX int32 `json:"maxX"`
	MaxY int32 `json:"maxY"`
}

type entityDTO struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	LayerID string   `json:"layerId"`
	Pos     pointDTO `json:"pos"`
	Solid   bool     `json:"solid"`
	Props   []kvDTO  `json:"props"`
}

type zoneDTO struct {
	ID     string  `json:"id"`
	Kind   string  `json:"kind"`
	Bounds rectDTO `json:"bounds"`
	Props  []kvDTO `json:"props"`
}

type kvDTO struct {
	K string `json:"k"`
	V string `json:"v"`
}

type layerTilesDTO struct {
	LayerID string     `json:"layerId"`
	Chunks  []chunkDTO `json:"chunks"`
}

type chunkDTO struct {
	CX    uint32    `json:"cx"`
	CY    uint32    `json:"cy"`
	Cells []cellDTO `json:"cells"`
}

type cellDTO struct {
	Idx uint64 `json:"idx"`
	Val uint16 `json:"v"`
}

// artifactDTO 是产物序列化形态（所有列表均已排序）。
type artifactDTO struct {
	FormatVersion uint16             `json:"formatVersion"`
	Revision      uint64             `json:"revision"`
	BuiltAt       string             `json:"builtAt"`
	Chunks        []artifactLayerDTO `json:"chunks"`
	EntityIndex   []entityLayerDTO   `json:"entityIndex"`
	ZoneList      []string           `json:"zoneList"`
}

type artifactLayerDTO struct {
	LayerID string         `json:"layerId"`
	Chunks  []chunkCodeDTO `json:"chunks"`
}

type chunkCodeDTO struct {
	Key  string `json:"key"`
	Code string `json:"code"`
}

type entityLayerDTO struct {
	LayerID  string   `json:"layerId"`
	Entities []string `json:"entities"`
}

// Pack 把文档与产物确定性序列化为带版本的二进制包。
func Pack(d *Document, a *Artifact) ([]byte, error) {
	if d == nil {
		return nil, rejectf(-1, RejectInvalidValue, "document must not be nil")
	}
	docJSON, err := MarshalDocument(d)
	if err != nil {
		return nil, err
	}
	var artJSON json.RawMessage
	if a != nil {
		artJSON, err = marshalArtifact(a)
		if err != nil {
			return nil, err
		}
	} else {
		artJSON = json.RawMessage("null")
	}
	payload := packPayload{
		FormatVersion: FormatVersion,
		Document:      json.RawMessage(docJSON),
		Artifact:      artJSON,
	}
	body, err := jsonMarshalSorted(payload)
	if err != nil {
		return nil, wrapReject(-1, RejectCorrupt, err, "encode payload")
	}
	out := make([]byte, 0, packHeaderLen+len(body)+packHashLen)
	buf := make([]byte, packHeaderLen)
	copy(buf[0:4], packMagic)
	binary.BigEndian.PutUint16(buf[4:6], FormatVersion)
	binary.BigEndian.PutUint32(buf[6:10], uint32(len(body)))
	out = append(out, buf...)
	out = append(out, body...)
	sum := sha256.Sum256(body)
	out = append(out, sum[:]...)
	return out, nil
}

// Unpack 读入二进制包：识别旧版本/损坏/校验和错误，安全返回而不崩溃，
// 不产生任何需要人工清理的状态。
func Unpack(data []byte) (*Document, *Artifact, error) {
	if len(data) < packHeaderLen+packHashLen {
		return nil, nil, rejectf(-1, RejectCorrupt, "packet too short: %d bytes", len(data))
	}
	if string(data[0:4]) != packMagic {
		return nil, nil, rejectf(-1, RejectCorrupt, "bad magic: %q", string(data[0:4]))
	}
	version := binary.BigEndian.Uint16(data[4:6])
	if version < MinSupportedVersion {
		return nil, nil, rejectf(-1, RejectUnknownVersion,
			"package version %d is older than supported minimum %d", version, MinSupportedVersion)
	}
	if version > MaxSupportedVersion {
		return nil, nil, rejectf(-1, RejectUnknownVersion,
			"package version %d is newer than supported maximum %d", version, MaxSupportedVersion)
	}
	n := binary.BigEndian.Uint32(data[6:10])
	end := packHeaderLen + int64(n)
	if int64(len(data)) != end+packHashLen {
		return nil, nil, rejectf(-1, RejectCorrupt,
			"length mismatch: header declares %d payload bytes, packet has %d", n,
			int64(len(data))-packHeaderLen-packHashLen)
	}
	body := data[packHeaderLen:end]
	stored := data[end : end+packHashLen]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], stored) {
		return nil, nil, rejectf(-1, RejectChecksum, "payload checksum mismatch")
	}
	var payload packPayload
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&payload); err != nil {
		return nil, nil, wrapReject(-1, RejectCorrupt, err, "decode payload")
	}
	d, err := unmarshalDocumentBytes(payload.Document)
	if err != nil {
		return nil, nil, err
	}
	var art *Artifact
	if len(payload.Artifact) > 0 && string(payload.Artifact) != "null" {
		art, err = unmarshalArtifact(payload.Artifact)
		if err != nil {
			return nil, nil, err
		}
	}
	return d, art, nil
}

// MarshalDocument 以规范化 JSON 编码文档（数组有序、键有序、无多余空白）。
func MarshalDocument(d *Document) ([]byte, error) {
	dto := docToDTO(d)
	return jsonMarshalSorted(dto)
}

// UnmarshalDocument 读入 JSON 文档，拒绝不支持的版本与损坏数据，
// 成功后重建索引并执行全量约束校验。
func UnmarshalDocument(data []byte) (*Document, error) {
	return unmarshalDocumentBytes(json.RawMessage(data))
}

func unmarshalDocumentBytes(raw json.RawMessage) (*Document, error) {
	if len(raw) == 0 {
		return nil, rejectf(-1, RejectCorrupt, "empty document payload")
	}
	var dto docDTO
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&dto); err != nil {
		return nil, wrapReject(-1, RejectCorrupt, err, "decode document")
	}
	d := dtoToDocument(&dto)
	if err := DefaultConstraints().ValidateAll(d); err != nil {
		return nil, err
	}
	d.rebuildIndex()
	return d, nil
}

func docToDTO(d *Document) docDTO {
	dto := docDTO{
		SchemaVersion: d.SchemaVersion,
		MapWidth:      d.MapWidth,
		MapHeight:     d.MapHeight,
		Layers:        make([]layerDTO, 0, len(d.LayerOrder)),
		Entities:      make([]entityDTO, 0, len(d.Entities)),
		Zones:         make([]zoneDTO, 0, len(d.ZoneOrder)),
		Tiles:         make([]layerTilesDTO, 0, len(d.LayerOrder)),
	}
	for _, id := range d.LayerOrder {
		l := d.Layers[id]
		dto.Layers = append(dto.Layers, layerDTO{ID: l.ID, Name: l.Name, Width: l.Width, Height: l.Height})
	}
	entIDs := make([]string, 0, len(d.Entities))
	for id := range d.Entities {
		entIDs = append(entIDs, id)
	}
	sortStrings(entIDs)
	for _, id := range entIDs {
		e := d.Entities[id]
		dto.Entities = append(dto.Entities, entityDTO{
			ID: e.ID, Kind: e.Kind, LayerID: e.LayerID,
			Pos:   pointDTO{X: e.Pos.X, Y: e.Pos.Y},
			Solid: e.Solid,
			Props: propsToDTO(e.Props),
		})
	}
	for _, id := range d.ZoneOrder {
		z := d.Zones[id]
		b := z.Bounds
		dto.Zones = append(dto.Zones, zoneDTO{
			ID: z.ID, Kind: z.Kind,
			Bounds: rectDTO{MinX: b.MinX, MinY: b.MinY, MaxX: b.MaxX, MaxY: b.MaxY},
			Props:  propsToDTO(z.Props),
		})
	}
	// tiles：图层按 LayerOrder；块按 (cy,cx)；单元按 idx。
	for _, layerID := range d.LayerOrder {
		cdto := []chunkDTO{}
		d.iterChunks(layerID, func(cx, cy uint32, ch *tileChunk) {
			idxs := make([]uint64, 0, len(ch.cells))
			for idx := range ch.cells {
				idxs = append(idxs, idx)
			}
			sortU64(idxs)
			cells := make([]cellDTO, 0, len(idxs))
			for _, idx := range idxs {
				cells = append(cells, cellDTO{Idx: idx, Val: ch.cells[idx]})
			}
			cdto = append(cdto, chunkDTO{CX: cx, CY: cy, Cells: cells})
		})
		dto.Tiles = append(dto.Tiles, layerTilesDTO{LayerID: layerID, Chunks: cdto})
	}
	return dto
}

func propsToDTO(m map[string]string) []kvDTO {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	out := make([]kvDTO, 0, len(keys))
	for _, k := range keys {
		out = append(out, kvDTO{K: k, V: m[k]})
	}
	return out
}

func dtoToProps(kv []kvDTO) map[string]string {
	if len(kv) == 0 {
		return nil
	}
	m := make(map[string]string, len(kv))
	for _, p := range kv {
		m[p.K] = p.V
	}
	return m
}

func dtoToDocument(dto *docDTO) *Document {
	d := NewDocument(dto.MapWidth, dto.MapHeight)
	d.SchemaVersion = dto.SchemaVersion
	if d.SchemaVersion == 0 {
		d.SchemaVersion = FormatVersion
	}
	for _, l := range dto.Layers {
		lc := l
		d.Layers[l.ID] = &Layer{ID: lc.ID, Name: lc.Name, Width: lc.Width, Height: lc.Height}
		d.LayerOrder = append(d.LayerOrder, l.ID)
		d.tiles[l.ID] = &tileState{pages: map[uint64]*tilePage{}}
	}
	for _, e := range dto.Entities {
		d.Entities[e.ID] = &Entity{
			ID: e.ID, Kind: e.Kind, LayerID: e.LayerID,
			Pos:   Point{X: e.Pos.X, Y: e.Pos.Y},
			Solid: e.Solid,
			Props: dtoToProps(e.Props),
		}
	}
	for _, z := range dto.Zones {
		b := z.Bounds
		d.Zones[z.ID] = &Zone{
			ID: z.ID, Kind: z.Kind,
			Bounds: Rect{MinX: b.MinX, MinY: b.MinY, MaxX: b.MaxX, MaxY: b.MaxY},
			Props:  dtoToProps(z.Props),
		}
		d.ZoneOrder = append(d.ZoneOrder, z.ID)
	}
	for _, lt := range dto.Tiles {
		st := d.tiles[lt.LayerID]
		if st == nil {
			st = &tileState{pages: map[uint64]*tilePage{}}
			d.tiles[lt.LayerID] = st
		}
		for _, ch := range lt.Chunks {
			m := &tileChunk{cells: map[uint64]uint16{}}
			for _, cell := range ch.Cells {
				m.cells[cell.Idx] = cell.Val
			}
			linear, _ := layerLinear(lt.LayerID, d, ch.CX, ch.CY)
			st.putChunk(linear, m)
		}
	}
	return d
}

// ---- 产物 JSON ----

func marshalArtifact(a *Artifact) ([]byte, error) {
	dto := artifactDTO{
		FormatVersion: a.FormatVersion,
		Revision:      a.Revision,
		BuiltAt:       a.BuiltAt,
		Chunks:        make([]artifactLayerDTO, 0, len(a.Chunks)),
		EntityIndex:   make([]entityLayerDTO, 0, len(a.EntityIndex)),
		ZoneList:      append([]string{}, a.ZoneList...),
	}
	layerIDs := make([]string, 0, len(a.Chunks))
	for id := range a.Chunks {
		layerIDs = append(layerIDs, id)
	}
	sortStrings(layerIDs)
	for _, id := range layerIDs {
		flat := a.Chunks[id].flatten()
		keys := make([]string, 0, len(flat))
		for k := range flat {
			keys = append(keys, k)
		}
		sortStrings(keys)
		rows := make([]chunkCodeDTO, 0, len(keys))
		for _, k := range keys {
			rows = append(rows, chunkCodeDTO{Key: k, Code: flat[k]})
		}
		dto.Chunks = append(dto.Chunks, artifactLayerDTO{LayerID: id, Chunks: rows})
	}
	entLayers := make([]string, 0, len(a.EntityIndex))
	for id := range a.EntityIndex {
		entLayers = append(entLayers, id)
	}
	sortStrings(entLayers)
	for _, id := range entLayers {
		list := append([]string{}, a.EntityIndex[id]...)
		dto.EntityIndex = append(dto.EntityIndex, entityLayerDTO{LayerID: id, Entities: list})
	}
	return jsonMarshalSorted(dto)
}

func unmarshalArtifact(raw json.RawMessage) (*Artifact, error) {
	var dto artifactDTO
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&dto); err != nil {
		return nil, wrapReject(-1, RejectCorrupt, err, "decode artifact")
	}
	if dto.FormatVersion < MinSupportedVersion || dto.FormatVersion > MaxSupportedVersion {
		return nil, rejectf(-1, RejectUnknownVersion, "artifact version %d unsupported", dto.FormatVersion)
	}
	a := &Artifact{
		FormatVersion: dto.FormatVersion,
		Revision:      dto.Revision,
		BuiltAt:       dto.BuiltAt,
		Chunks:        map[string]*chunkMap{},
		EntityIndex:   map[string][]string{},
		ZoneList:      append([]string{}, dto.ZoneList...),
	}
	for _, l := range dto.Chunks {
		m := make(map[string]string, len(l.Chunks))
		for _, c := range l.Chunks {
			m[c.Key] = c.Code
		}
		a.Chunks[l.LayerID] = &chunkMap{overlay: m}
	}
	for _, l := range dto.EntityIndex {
		a.EntityIndex[l.LayerID] = append([]string{}, l.Entities...)
	}
	return a, nil
}

// ---- 确定性 JSON ----

// jsonMarshalSorted 使用编码/json 输出；本包的结构体字段顺序固定，
// 且所有 map 都先转为排序后的数组，因此输出在任意机器上字节一致。
func jsonMarshalSorted(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode 末尾追加换行，去掉以保证字节稳定。
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func sortStrings(s []string) {
	// 简单插入排序成本低；这里统一引用标准库避免重复实现。
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func sortU64(s []uint64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// LevelPack / LevelUnpack 是 Pack / Unpack 的对外别名，便于按包名调用。
func LevelPack(d *Document, a *Artifact) ([]byte, error) { return Pack(d, a) }

// LevelUnpack 见 Unpack。
func LevelUnpack(data []byte) (*Document, *Artifact, error) { return Unpack(data) }
