package levelcore

import "crypto/sha256"

func hashSHA256(b []byte) [32]byte { return sha256.Sum256(b) }
