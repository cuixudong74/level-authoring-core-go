package levelcore

import "encoding/binary"

func binaryBigEndianPutUint16(b []byte, v uint16) {
	binary.BigEndian.PutUint16(b, v)
}

func sha256Sum(b []byte) []byte {
	s := hashSHA256(b)
	return s[:]
}

func UnpackErr(data []byte) error {
	_, _, err := Unpack(data)
	return err
}
