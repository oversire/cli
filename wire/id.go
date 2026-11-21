package wire

import (
	"crypto/rand"
	"encoding/hex"
)

// RandomHex gives n random bytes as 2n hex characters.
func RandomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
