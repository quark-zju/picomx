package pop3

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
)

// Credentials verifies one username and high-entropy app password.
type Credentials struct {
	configured   bool
	usernameHash [sha256.Size]byte
	passwordHash [sha256.Size]byte
}

// NewCredentials builds fail-closed credentials from an expected SHA-256 digest.
func NewCredentials(username, passwordSHA256 string) (Credentials, error) {
	username = strings.TrimSpace(username)
	passwordSHA256 = strings.TrimSpace(passwordSHA256)
	if username == "" || passwordSHA256 == "" {
		return Credentials{}, nil
	}
	digest, err := hex.DecodeString(passwordSHA256)
	if err != nil || len(digest) != sha256.Size {
		return Credentials{}, errors.New("POP3 password SHA-256 must be 64 hexadecimal characters")
	}
	credentials := Credentials{configured: true, usernameHash: sha256.Sum256([]byte(username))}
	copy(credentials.passwordHash[:], digest)
	return credentials, nil
}

func (c Credentials) authenticate(username, password string) bool {
	usernameHash := sha256.Sum256([]byte(username))
	passwordHash := sha256.Sum256([]byte(password))
	valid := subtle.ConstantTimeCompare(usernameHash[:], c.usernameHash[:]) &
		subtle.ConstantTimeCompare(passwordHash[:], c.passwordHash[:]) &
		subtle.ConstantTimeByteEq(byte(boolInt(c.configured)), 1)
	return valid == 1
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
