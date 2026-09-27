package spool

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/bits"
	"strings"
)

const commonsInvitePrefix = "knit-commons:v1:"

func BlobID(data []byte) [32]byte {
	return sha256.Sum256(data)
}

// ScopeDigest is the protocol's order-independent FNV-1a-64 XOR fold.
func ScopeDigest(ids [][]byte) uint64 {
	var digest uint64
	for _, id := range ids {
		var h uint64 = 0xcbf29ce484222325
		for _, b := range id {
			h ^= uint64(b)
			h *= 0x00000100000001b3
		}
		digest ^= h
	}
	return digest
}

func PowDay(unixMillis int64) int64 {
	if unixMillis >= 0 {
		return unixMillis / 86_400_000
	}
	return (unixMillis - 86_400_000 + 1) / 86_400_000
}

func PowHash(scope []byte, day, nonce uint64) [32]byte {
	input := make([]byte, 0, len("knit/spool/v1/pow")+32+16)
	input = append(input, "knit/spool/v1/pow"...)
	input = append(input, scope...)
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], day)
	input = append(input, number[:]...)
	binary.BigEndian.PutUint64(number[:], nonce)
	input = append(input, number[:]...)
	return sha256.Sum256(input)
}

func HasLeadingZeroBits(hash []byte, required int) bool {
	if required <= 0 {
		return true
	}
	if required > len(hash)*8 {
		return false
	}
	whole, remainder := required/8, required%8
	for _, b := range hash[:whole] {
		if b != 0 {
			return false
		}
	}
	return remainder == 0 || hash[whole]>>(8-remainder) == 0
}

func VerifyPow(scope []byte, day, nonce uint64, nowMillis int64, difficulty int) bool {
	if len(scope) != ScopeIDBytes || difficulty < 0 || difficulty > 256 {
		return false
	}
	today := PowDay(nowMillis)
	stampDay := int64(day)
	if stampDay < today-1 || stampDay > today+1 {
		return false
	}
	h := PowHash(scope, day, nonce)
	return HasLeadingZeroBits(h[:], difficulty)
}

// MinePow finds the smallest nonce satisfying the requested difficulty.
func MinePow(scope []byte, day uint64, difficulty int) (uint64, bool) {
	if len(scope) != ScopeIDBytes || difficulty < 0 || difficulty > 32 {
		return 0, false
	}
	if difficulty == 0 {
		return 0, true
	}
	for nonce := uint64(0); ; nonce++ {
		h := PowHash(scope, day, nonce)
		if HasLeadingZeroBits(h[:], difficulty) {
			return nonce, true
		}
		if nonce == ^uint64(0) {
			return 0, false
		}
	}
}

func CommonsScopeID(secret []byte) [32]byte {
	input := append([]byte("knit/spool/v1/commons"), secret...)
	return sha256.Sum256(input)
}

func EncodeCommonsInvite(secret []byte) (string, [32]byte, error) {
	if len(secret) != 32 {
		return "", [32]byte{}, errors.New("commons secret must be 32 bytes")
	}
	id := CommonsScopeID(secret)
	return commonsInvitePrefix + base64.RawURLEncoding.EncodeToString(secret), id, nil
}

func DecodeCommonsInvite(invite string) ([]byte, [32]byte, error) {
	if !strings.HasPrefix(invite, commonsInvitePrefix) {
		return nil, [32]byte{}, errors.New("invite must use knit-commons:v1")
	}
	secret, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(invite, commonsInvitePrefix))
	if err != nil || len(secret) != 32 {
		return nil, [32]byte{}, errors.New("invite must contain a 32-byte secret")
	}
	return secret, CommonsScopeID(secret), nil
}

func NewCommonsInvite() (string, [32]byte, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", [32]byte{}, err
	}
	return EncodeCommonsInvite(secret)
}

func HexID(id []byte) string { return hex.EncodeToString(id) }

func LeadingZeroBits(hash []byte) int {
	count := 0
	for _, b := range hash {
		if b == 0 {
			count += 8
			continue
		}
		count += bits.LeadingZeros8(b)
		break
	}
	return count
}
