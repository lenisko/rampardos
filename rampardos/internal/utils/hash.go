package utils

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// PersistentHashString generates a stable hash for a string
func PersistentHashString(s string) string {
	hash := sha256.Sum256([]byte(s))
	encoded := base64.StdEncoding.EncodeToString(hash[:])
	return strings.ReplaceAll(encoded, "/", "_")
}
