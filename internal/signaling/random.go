package signaling

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/google/uuid"
)

// crockfordAlphabet excludes I, L, O, U to avoid confusion with 1/0 and profanity.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func randomUserCode() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	out := make([]byte, 9)
	for i := 0; i < 4; i++ {
		out[i] = crockfordAlphabet[int(b[i])&0x1f]
	}
	out[4] = '-'
	for i := 0; i < 4; i++ {
		out[5+i] = crockfordAlphabet[int(b[4+i])&0x1f]
	}
	return string(out)
}

func randomToken(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func randomUUID() string { return uuid.NewString() }
