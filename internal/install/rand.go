package install

import "crypto/rand"

func randRead(b *[32]byte) (int, error) {
	return rand.Read(b[:])
}
