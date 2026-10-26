package common

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
)

// SHA256Hex returns the hexadecimal SHA-256 digest for data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SHA256HexReader streams r and returns its hexadecimal SHA-256 digest.
func SHA256HexReader(r io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
