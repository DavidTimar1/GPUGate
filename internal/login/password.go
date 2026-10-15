package login

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// HashPath is where the console password's hash lives, relative to the working directory like
// every other piece of working state. A file and not a database column: the database may be
// copied elsewhere, and no column in it holds a secret.
const HashPath = ".state/login-password-hash"

// MinPasswordLength is the fewest characters set-password accepts.
const MinPasswordLength = 12

// Argon2id at the minimum OWASP's Password Storage Cheat Sheet gives: 19 MiB, 2 passes, 1 lane
// [DOC] https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html. Every
// parameter is stored inside the hash string, so raising them later never breaks a hash on file.
const (
	saltLen     = 16
	hashLen     = 32
	argonMemory = 19456 // KiB
	argonTime   = 2
	argonLanes  = 1
	// A stored parameter outside these bounds is a damaged or tampered file, never a hash to
	// verify: verifying costs that much memory and time for every attempt.
	maxMemory = 262144 // KiB
	maxTime   = 10
	maxLanes  = 8
)

var errNoPassword = errors.New("no password is set")

// SetPassword hashes password and replaces the file at path with the result: mode 0600, in a
// directory created with mode 0700 when it does not exist yet. The file is written beside its
// final name and renamed over it, so a running server reading it never sees half a hash. Changing
// the file's contents ends every session (Auth.validSession compares the file's digest).
func SetPassword(path, password string) error {
	if password == "" {
		return errors.New("the password is empty")
	}
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("the password is shorter than %d characters", MinPasswordLength)
	}
	encoded, err := hashPassword(password)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// The folder holds the database and the keys too: closed to every other account.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restrict %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".login-password-hash-*")
	if err != nil {
		return fmt.Errorf("write the password hash: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write the password hash: %w", err)
	}
	if _, err := tmp.WriteString(encoded + "\n"); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write the password hash: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write the password hash: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("write the password hash: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("write the password hash: %w", err)
	}
	return nil
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("mint a salt: %w", err)
	}
	derived := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonLanes, hashLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonLanes,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(derived)), nil
}

var errMalformed = errors.New("the stored password hash is not in the expected form")

// verifyPassword compares password with a stored hash string in constant time. An unreadable
// stored string is an error and never a match.
func verifyPassword(stored, password string) (bool, error) {
	parts := strings.Split(strings.TrimSpace(stored), "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return false, errMalformed
	}
	var mem, t, lanes int
	if n, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &lanes); err != nil || n != 3 {
		return false, errMalformed
	}
	if mem < 1024 || mem > maxMemory || t < 1 || t > maxTime || lanes < 1 || lanes > maxLanes {
		return false, errMalformed
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, errMalformed
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, errMalformed
	}
	got := argon2.IDKey([]byte(password), salt, uint32(t), uint32(mem), uint8(lanes), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// readStored answers the stored hash string, or errNoPassword when the file does not exist.
func readStored(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errNoPassword
	}
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", errNoPassword
	}
	return text, nil
}

// stampOf is what a session carries to prove it was made under the current password: a digest of
// the stored hash string. A password reset changes it, so every older session stops matching.
func stampOf(stored string) string {
	sum := sha256.Sum256([]byte(stored))
	return hex.EncodeToString(sum[:])
}
