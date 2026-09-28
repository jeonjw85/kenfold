package oauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (RFC 9106's second recommended option, memory-constrained:
// 64 MiB, 3 passes). They are stored with each hash, so they can be raised
// later without invalidating existing passwords.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16

	// MinPasswordRunes is the shortest owner password accepted. The consent
	// page is the only thing between the internet and the memory store.
	MinPasswordRunes = 12
	maxPasswordBytes = 1024
)

// ErrWeakPassword is returned for passwords shorter than MinPasswordRunes.
var ErrWeakPassword = fmt.Errorf("the owner password must be at least %d characters", MinPasswordRunes)

// HashPassword returns an encoded Argon2id hash of password:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash> (base64, unpadded).
func HashPassword(password string) (string, error) {
	if utf8.RuneCountInString(password) < MinPasswordRunes {
		return "", ErrWeakPassword
	}
	if len(password) > maxPasswordBytes {
		return "", errors.New("the owner password is too long")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword reports whether password matches encoded, in constant time
// with respect to the password.
func CheckPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("unsupported argon2 version")
	}
	var mem, iter uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &iter, &threads); err != nil || mem == 0 || iter == 0 || threads == 0 || mem > 1<<22 || iter > 100 {
		return false, errors.New("bad argon2 parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errors.New("bad salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false, errors.New("bad hash")
	}
	if len(password) > maxPasswordBytes {
		return false, nil
	}
	got := argon2.IDKey([]byte(password), salt, iter, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
