// Package podkey mints, stores and retires one file per rented machine holding the random API key
// this program tells the model server to require. The key is never derived from the machine's
// id: RunPod's own vLLM template derives its key from the pod id, and that id is the hostname of
// the pod's public URL, so anyone holding the URL would hold the key. A machine whose file
// cannot be read is reported by every caller as "no key on file for <id>", never guessed at.
package podkey

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

// dir is where every machine's key file lives, relative to the working directory, like
// cmd/gpugate/main.go's dbPath. Untracked working state.
const dir = ".state/pod-keys"

// Generate mints one random key for a launch: 32 bytes from crypto/rand, hex-encoded. Called once
// per launch, before the claim is written and before the provider is asked to create anything.
func Generate() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mint a pod key: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// Path names the one file providerKey/providerID's key lives in; nothing else builds this path.
// providerID is a provider's own answer to a create or list call, so it is untrusted input:
// before either part is joined into a filesystem path both are held to [A-Za-z0-9-], the shape
// of a RunPod id (lowercase alphanumerics) and of a Verda id (a hyphenated UUID). A "/" or ".."
// in that answer would otherwise let a provider's text name a path outside dir.
func Path(providerKey, providerID string) (string, error) {
	if err := checkIDPart(providerKey); err != nil {
		return "", fmt.Errorf("provider key: %w", err)
	}
	if err := checkIDPart(providerID); err != nil {
		return "", fmt.Errorf("provider id: %w", err)
	}
	return dir + "/" + providerKey + "-" + providerID, nil
}

// checkIDPart refuses an empty string or one that contains a character outside [A-Za-z0-9-].
// The error names only the value's length: the value is a provider's text and is never written
// back into a log.
func checkIDPart(s string) error {
	if s == "" {
		return fmt.Errorf("is empty")
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return fmt.Errorf("has length %d and a character outside [A-Za-z0-9-]", len(s))
		}
	}
	return nil
}

// Write creates dir (mode 0700) if it does not exist yet, and writes key to
// Path(providerKey, providerID) (mode 0600). Called once the create answer names the machine's
// id, before the claim is marked confirmed.
func Write(providerKey, providerID, key string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	path, err := Path(providerKey, providerID)
	if err != nil {
		return fmt.Errorf("write a pod key: %w", err)
	}
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Read answers providerKey/providerID's key. A file that cannot be read, almost always because
// it was never written, was already removed by a destroy, or providerID fails Path's check, is
// reported by every caller as "no key on file for <id>".
func Read(providerKey, providerID string) (string, error) {
	path, err := Path(providerKey, providerID)
	if err != nil {
		return "", fmt.Errorf("read a pod key: %w", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// Delete removes providerKey/providerID's key file, once a fresh provider answer confirms the
// machine gone. A file already gone is not an error.
func Delete(providerKey, providerID string) error {
	path, err := Path(providerKey, providerID)
	if err != nil {
		return fmt.Errorf("delete a pod key: %w", err)
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
