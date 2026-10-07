package levelcore

import "testing"

// 旧版本二进制必须被识别为 UNKNOWN_VERSION（安全拒绝，不崩溃）。
func TestOldVersionRejected(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	pk, err := Pack(e.Document(), e.Artifact())
	if err != nil {
		t.Fatal(err)
	}
	old := append([]byte(nil), pk...)
	binaryBigEndianPutUint16(old[4:6], MinSupportedVersion-1)
	copy(old[len(old)-32:], sha256Sum(old[10:len(old)-32]))
	_, _, err = Unpack(old)
	re, ok := AsReject(err)
	if !ok || re.Code != RejectUnknownVersion {
		t.Fatalf("want UNKNOWN_VERSION for old packet, got %v", err)
	}
}

// 未来版本必须安全拒绝。
func TestFutureVersionRejected(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	pk, _ := Pack(e.Document(), e.Artifact())
	future := append([]byte(nil), pk...)
	binaryBigEndianPutUint16(future[4:6], MaxSupportedVersion+1)
	copy(future[len(future)-32:], sha256Sum(future[10:len(future)-32]))
	if _, _, err := Unpack(future); err == nil {
		t.Fatal("future version must be rejected")
	}
}

// JSON 文档中的旧/未知 schemaVersion 必须被识别。
func TestJSONUnknownSchemaVersion(t *testing.T) {
	raw := []byte(`{"schemaVersion":999,"mapWidth":8,"mapHeight":8,"layers":[],"entities":[],"zones":[],"tiles":[]}`)
	_, err := UnmarshalDocument(raw)
	re, ok := AsReject(err)
	if !ok || re.Code != RejectUnknownVersion {
		t.Fatalf("want UNKNOWN_VERSION, got %v", err)
	}
}

// 损坏 JSON、长度不符、错误 magic、校验和错误均安全返回。
func TestCorruptInputsSafe(t *testing.T) {
	e := newTestEditor(t, 16, 16)
	pk, _ := Pack(e.Document(), e.Artifact())

	cases := map[string][]byte{
		"nil":          nil,
		"empty":        {},
		"short":        pk[:3],
		"truncated":    pk[:len(pk)-4],
		"badLengthHdr": append(append([]byte(nil), pk[:6]...), 0xFF, 0xFF, 0xFF, 0xFF),
	}
	for name, data := range cases {
		if _, _, err := Unpack(data); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	badMagic := append([]byte(nil), pk...)
	copy(badMagic[0:4], "XXXX")
	if re, ok := AsReject(UnpackErr(badMagic)); !ok || re.Code != RejectCorrupt {
		t.Fatalf("magic: %v", UnpackErr(badMagic))
	}
	flip := append([]byte(nil), pk...)
	flip[15] ^= 0x01
	if re, ok := AsReject(UnpackErr(flip)); !ok || re.Code != RejectChecksum {
		t.Fatalf("flip: %v", UnpackErr(flip))
	}
}
