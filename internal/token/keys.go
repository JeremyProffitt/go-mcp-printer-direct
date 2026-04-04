package token

import (
	"crypto/ed25519"
	"crypto/sha256"
	"log/slog"
)

type KeyPair struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
}

func LoadKeyPair(seed string) *KeyPair {
	var seedBytes []byte

	if seed == "" {
		slog.Warn("JWT_SIGNING_KEY not set, using deterministic development key")
		h := sha256.Sum256([]byte("mcp-printer-direct-dev-key"))
		seedBytes = h[:]
	} else {
		h := sha256.Sum256([]byte(seed))
		seedBytes = h[:]
	}

	privateKey := ed25519.NewKeyFromSeed(seedBytes)
	publicKey := privateKey.Public().(ed25519.PublicKey)

	return &KeyPair{
		PrivateKey: privateKey,
		PublicKey:  publicKey,
	}
}
