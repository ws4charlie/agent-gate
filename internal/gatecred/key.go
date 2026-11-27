package gatecred

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Where the key lives. The file holds the 32-byte Ed25519 seed as hex and is
// gitignored (data/ and *.key); the kid comes from the environment.
const (
	EnvKeyFile     = "AG_GATE_CRED_KEY_FILE"
	EnvKid         = "AG_GATE_CRED_KID"
	DefaultKeyFile = "data/gate-cred.key"
)

// testSeedStrings derive the TEST ONLY keys of the shared vectors
// (seed = SHA-256 of the string). Their seeds are public, so a key equal to
// one of them must never sign anything real. Key 3 is not in the vectors'
// key set but is derived the same way.
var testSeedStrings = []string{
	"TEST-ONLY-qv-gate-credential-key-1",
	"TEST-ONLY-qv-gate-credential-key-2",
	"TEST-ONLY-qv-gate-credential-key-3",
}

// IsTestKey reports whether pub is one of the vectors' TEST ONLY keys.
func IsTestKey(pub ed25519.PublicKey) bool {
	for _, s := range testSeedStrings {
		seed := sha256.Sum256([]byte(s))
		test := ed25519.NewKeyFromSeed(seed[:]).Public().(ed25519.PublicKey)
		if subtle.ConstantTimeCompare(test, pub) == 1 {
			return true
		}
	}
	return false
}

// LoadKey reads the signing key: the seed file at $AG_GATE_CRED_KEY_FILE
// (default data/gate-cred.key), mode 0600 or stricter, and the kid in
// $AG_GATE_CRED_KID. It refuses a missing kid, a file others can read or
// write, and the vectors' TEST ONLY keys.
func LoadKey() (Key, error) {
	path := os.Getenv(EnvKeyFile)
	if path == "" {
		path = DefaultKeyFile
	}
	return LoadKeyFile(path, os.Getenv(EnvKid))
}

// LoadKeyFile is LoadKey with the path and kid given.
func LoadKeyFile(path, kid string) (Key, error) {
	if kid == "" {
		return Key{}, fmt.Errorf("gatecred: no kid: set %s to the kid Vault's operators registered", EnvKid)
	}
	if err := validKid(kid); err != nil {
		return Key{}, fmt.Errorf("gatecred: %s: %s", EnvKid, err.Reason)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return Key{}, fmt.Errorf("gatecred: no key file (agentgate gatecred keygen): %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Key{}, fmt.Errorf("gatecred: key file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return Key{}, fmt.Errorf("gatecred: key file %s has mode %04o; need 0600 or stricter (chmod 600)", path, perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Key{}, fmt.Errorf("gatecred: read key file: %w", err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	clear(b)
	if err != nil || len(seed) != ed25519.SeedSize {
		// The error text never quotes the file: it may hold a seed.
		return Key{}, fmt.Errorf("gatecred: key file %s must hold a %d-byte seed as hex", path, ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	k := Key{Kid: kid, Priv: priv}
	if IsTestKey(k.Public()) {
		return Key{}, errors.New("gatecred: refusing a TEST ONLY key from the shared vectors; it signs nothing real")
	}
	return k, nil
}

// NewKeyFile creates a fresh seed at path (mode 0600, refuses to overwrite)
// and returns the public key. The seed is never returned or printed.
func NewKeyFile(path string) (ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	seed := priv.Seed()
	defer clear(seed)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("gatecred: %w (refusing to overwrite an existing key)", err)
	}
	if _, err := f.WriteString(hex.EncodeToString(seed) + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return pub, nil
}

// ValidKid reports whether kid has the contract's form (1-64 of A-Z a-z 0-9 . _ -).
func ValidKid(kid string) error {
	if e := validKid(kid); e != nil {
		return e
	}
	return nil
}

// Keygen creates a new signing key at path and prints what Vault's operators
// need: the kid and the public key (32 bytes, base64url without padding).
// The seed stays in the file and is never printed.
func Keygen(w io.Writer, path, kid string) error {
	if err := ValidKid(kid); err != nil {
		return err
	}
	pub, err := NewKeyFile(path)
	if err != nil {
		return err
	}
	if IsTestKey(pub) { // cannot happen with a random seed; refuse anyway
		os.Remove(path)
		return errors.New("gatecred: generated a TEST ONLY key")
	}
	fmt.Fprintf(w, "kid:        %s\npublic_key: %s\nkey file:   %s (mode 0600; keep it out of git and backups you do not control)\n", kid, EncodePublicKey(pub), path)
	fmt.Fprintf(w, "next: set %s=%s for the gateway, and send the kid and public_key to Vault's operators for the signed gate setting.\n", EnvKid, kid)
	return nil
}
