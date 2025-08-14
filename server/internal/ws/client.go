package ws

import (
	"crypto/rand"
	"encoding/hex"
)

func randID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
