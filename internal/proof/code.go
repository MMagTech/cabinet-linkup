package proof

import (
	"crypto/rand"
	"math/big"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	codeLength = 12
	codeGroup  = 4

	// What a code is drawn from. The letters a reader accepts are wider (PROTOCOL.md),
	// so a console's check and this set can each be read on their own.
	codeLetters = "ACDEFGHJKMNPQRTUVWXYZ2345679"
	codeAccepts = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
)

// The slow hash's fixed parameters: never negotiated, so a fake server cannot ask for a
// weaker one.
const (
	hashTime   = 3
	hashMemory = 64 * 1024
	hashLanes  = 1
	hashLength = 32
)

var hashSalt = []byte("cabinet-linkup code 1")

// NewCode draws a code, grouped for reading: K7MR-4QXT-9HPV.
func NewCode() string {
	var b strings.Builder
	for i := range codeLength {
		if i > 0 && i%codeGroup == 0 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeLetters))))
		if err != nil {
			panic(err)
		}
		b.WriteByte(codeLetters[n.Int64()])
	}
	return b.String()
}

// TidyCode turns what someone typed into the twelve characters that are hashed: upper case,
// with dashes, spaces and anything else not in a code removed. It says false if that does not
// leave exactly twelve.
func TidyCode(typed string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(typed) {
		if strings.ContainsRune(codeAccepts, r) {
			b.WriteRune(r)
		}
	}
	return b.String(), b.Len() == codeLength
}

// SlowHash is K, the pre-shared key both sides prove they hold. Only the server's copy is
// worked out ahead of time, when the code is made, so a guess costs the guesser the hash and
// the server almost nothing.
func SlowHash(tidy string) []byte {
	return argon2.IDKey([]byte(tidy), hashSalt, hashTime, hashMemory, hashLanes, hashLength)
}
