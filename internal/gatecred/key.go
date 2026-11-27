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
	"syscall"
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

// currentUID is the user the key file must belong to (a test hook).
var currentUID = os.Getuid

// LoadKeyFile is LoadKey with the path and kid given. The file is opened once
// without following a symlink, and every check runs on the open file, so it
// cannot be swapped between the check and the read. It must be a regular
// file owned by this user, mode 0600 or stricter, in a directory no one else
// can write to (else someone could replace it).
func LoadKeyFile(path, kid string) (Key, error) {
	if kid == "" {
		return Key{}, fmt.Errorf("gatecred: no kid: set %s to the kid Vault's operators registered", EnvKid)
	}
	if err := validKid(kid); err != nil {
		return Key{}, fmt.Errorf("gatecred: %s: %s", EnvKid, err.Reason)
	}
	if err := checkKeyDir(filepath.Dir(path)); err != nil {
		return Key{}, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return Key{}, fmt.Errorf("gatecred: key file %s is a symlink; use the real file", path)
		}
		return Key{}, fmt.Errorf("gatecred: no key file (agentgate gatecred keygen): %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Key{}, fmt.Errorf("gatecred: key file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Key{}, fmt.Errorf("gatecred: key file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return Key{}, fmt.Errorf("gatecred: key file %s has mode %04o; need 0600 or stricter (chmod 600)", path, perm)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != currentUID() {
		return Key{}, fmt.Errorf("gatecred: key file %s is not owned by this user", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 256))
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
	defer clear(priv)
	return NewKey(kid, priv)
}

// checkKeyDir refuses a directory that group or others can write to: anyone
// who can write there can replace the key file.
func checkKeyDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("gatecred: key directory: %w", err)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("gatecred: key directory %s has mode %04o; group or others can write to it (chmod 700)", dir, perm)
	}
	return nil
}

// NewKeyFile creates a fresh seed at path (mode 0600, refuses to overwrite)
// and returns the public key. A missing directory is created 0700; an
// existing one that group or others can write to is refused. The seed is
// never returned or printed.
func NewKeyFile(path string) (ed25519.PublicKey, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := checkKeyDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	defer clear(priv)
	seed := priv.Seed()
	defer clear(seed)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
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
	if fi, err := os.Stat(filepath.Dir(path)); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(w, "note:       directory %s has mode %04o; others can list it (the key file itself is 0600). chmod 700 it if you can.\n", filepath.Dir(path), fi.Mode().Perm())
	}
	fmt.Fprintf(w, "next: set %s=%s for the gateway, and send the kid and public_key to Vault's operators for the signed gate setting.\n", EnvKid, kid)
	return nil
}

// NewKey makes a signing key from a kid and an Ed25519 private key. It
// refuses a bad kid, a key of the wrong size, a key whose public half is not
// the one its seed derives, and the vectors' TEST ONLY keys (judged by the
// derived public half). The key is copied into the Key's signer; nothing
// outside this package can read or print it.
func NewKey(kid string, priv ed25519.PrivateKey) (Key, error) {
	if err := validKid(kid); err != nil {
		return Key{}, fmt.Errorf("gatecred: kid: %s", err.Reason)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return Key{}, fmt.Errorf("gatecred: private key must be %d bytes", ed25519.PrivateKeySize)
	}
	derived := ed25519.NewKeyFromSeed(priv.Seed())
	defer clear(derived)
	if subtle.ConstantTimeCompare(derived[ed25519.SeedSize:], priv[ed25519.SeedSize:]) != 1 {
		return Key{}, errors.New("gatecred: private key's public half does not match its seed")
	}
	if IsTestKey(derived.Public().(ed25519.PublicKey)) {
		return Key{}, errors.New("gatecred: refusing a TEST ONLY key from the shared vectors; it signs nothing real")
	}
	return keyFrom(kid, derived), nil
}
