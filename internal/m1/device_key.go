package m1

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"

	"filippo.io/edwards25519"
)

var inverseCofactor = func() *edwards25519.Scalar {
	encoded := [32]byte{8}
	cofactor, _ := new(edwards25519.Scalar).SetCanonicalBytes(encoded[:])
	return new(edwards25519.Scalar).Invert(cofactor)
}()

func devicePublicKey(value string) ([]byte, bool) {
	key, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(key) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(key) != value {
		return nil, false
	}
	point, err := new(edwards25519.Point).SetBytes(key)
	if err != nil || !bytes.Equal(point.Bytes(), key) {
		return nil, false
	}
	// Clearing the cofactor and undoing it in the prime-order subgroup must
	// recover the same point. The identity itself proves no private key.
	prime := new(edwards25519.Point).MultByCofactor(point)
	if prime.Equal(edwards25519.NewIdentityPoint()) == 1 ||
		new(edwards25519.Point).ScalarMult(inverseCofactor, prime).Equal(point) != 1 {
		return nil, false
	}
	return key, true
}
