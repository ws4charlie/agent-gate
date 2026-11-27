package gatecred

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const vectorsDir = "../../testdata/gate-credential"

type vectorFile struct {
	DomainPrefix string `json:"domain_prefix"`
	Config       struct {
		Audience string `json:"audience"`
		Skew     int64  `json:"skew_seconds"`
		MaxTTL   int64  `json:"max_ttl_seconds"`
		Keys     []struct {
			Kid        string `json:"kid"`
			PublicKey  string `json:"public_key"`
			SeedString string `json:"seed_string"`
		} `json:"keys"`
	} `json:"config"`
	Now   int64 `json:"now"`
	Cases []struct {
		Name           string          `json:"name"`
		Credential     *string         `json:"credential"`
		SignedBytesHex string          `json:"signed_bytes_hex"`
		Payment        payment         `json:"payment"`
		Now            *int64          `json:"now"`
		Expect         expect          `json:"expect"`
		Reuse          json.RawMessage `json:"then_expect_on_reuse"`
		Substitute     *struct {
			Find    string `json:"find"`
			WithHex string `json:"with_hex"`
		} `json:"substitute"`
	} `json:"cases"`
}

type expect struct {
	OK    bool   `json:"ok"`
	Code  string `json:"code"`
	Field string `json:"field"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vectorsDir, "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func testKey(t *testing.T, seedString, kid string) Key {
	t.Helper()
	seed := sha256.Sum256([]byte(seedString))
	return Key{Kid: kid, priv: ed25519.NewKeyFromSeed(seed[:])}
}

// caseBytes is the credential text after the case's substitute, if any.
func caseBytes(t *testing.T, cred string, sub *struct {
	Find    string `json:"find"`
	WithHex string `json:"with_hex"`
}) []byte {
	b := []byte(cred)
	if sub == nil {
		return b
	}
	with, err := hex.DecodeString(sub.WithHex)
	if err != nil || bytes.Count(b, []byte(sub.Find)) != 1 {
		t.Fatalf("bad substitute %+v", sub)
	}
	return bytes.Replace(b, []byte(sub.Find), with, 1)
}

// TestVectors runs every case through the test verifier. then_expect_on_reuse
// is skipped: it needs a used-credential store, and Gate keeps none (Vault's
// checkers do; Gate only mints).
func TestVectors(t *testing.T) {
	v := loadVectors(t)
	if v.DomainPrefix != DomainPrefix {
		t.Fatalf("domain prefix %q, want %q", v.DomainPrefix, DomainPrefix)
	}
	cfg := verifyConfig{audience: v.Config.Audience, skew: v.Config.Skew, maxTTL: v.Config.MaxTTL, keys: map[string]ed25519.PublicKey{}}
	for _, k := range v.Config.Keys {
		pub, err := base64.RawURLEncoding.Strict().DecodeString(k.PublicKey)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			t.Fatalf("key %s: bad public key", k.Kid)
		}
		if !bytes.Equal(testKey(t, k.SeedString, k.Kid).Public(), pub) {
			t.Fatalf("key %s: seed_string does not derive the listed public key", k.Kid)
		}
		cfg.keys[k.Kid] = pub
	}
	if len(v.Cases) != 73 {
		t.Fatalf("%d cases, want 73 (vectors changed? update this count with the new copy)", len(v.Cases))
	}
	passed, bytesChecked := 0, 0
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			now := v.Now
			if c.Now != nil {
				now = *c.Now
			}
			var cred []byte
			if c.Credential != nil {
				cred = caseBytes(t, *c.Credential, c.Substitute)
			}
			got := verify(cfg, cred, c.Credential != nil, c.Payment, now)
			want := expect{OK: true}
			if !c.Expect.OK {
				want = c.Expect
			}
			if gotE := (expect{OK: got.code == "", Code: got.code, Field: got.field}); gotE != want {
				t.Fatalf("got %+v, want %+v", gotE, want)
			}
			if c.SignedBytesHex != "" {
				if hex.EncodeToString(got.signedBytes) != c.SignedBytesHex {
					t.Fatalf("signed bytes differ:\n got %x\nwant %s", got.signedBytes, c.SignedBytesHex)
				}
				bytesChecked++
			} else if got.signedBytes != nil {
				t.Fatalf("parsed, but the vector has no signed_bytes_hex")
			}
			passed++
		})
	}
	t.Logf("vector cases: %d run, %d passed; signed bytes matched in %d", len(v.Cases), passed, bytesChecked)
}

// TestMintReproducesVectors: for every valid case, Mint with the derived TEST
// key over the credential without sig gives the same signed bytes and the
// same sig (Ed25519 is deterministic).
func TestMintReproducesVectors(t *testing.T) {
	v := loadVectors(t)
	keys := map[string]Key{}
	for _, k := range v.Config.Keys {
		keys[k.Kid] = testKey(t, k.SeedString, k.Kid)
	}
	n := 0
	for _, c := range v.Cases {
		if !c.Expect.OK {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			cred, sig, perr := parseStrict([]byte(*c.Credential))
			if perr != nil {
				t.Fatal(perr)
			}
			if got := hex.EncodeToString(SignedBytes(cred)); got != c.SignedBytesHex {
				t.Fatalf("SignedBytes differ:\n got %s\nwant %s", got, c.SignedBytesHex)
			}
			s, err := Mint(keys[cred.Kid], cred)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			if err := json.Unmarshal(s.JSON, &out); err != nil {
				t.Fatal(err)
			}
			if out["sig"] != base64.RawURLEncoding.EncodeToString(sig) {
				t.Fatalf("sig %v, want %s", out["sig"], base64.RawURLEncoding.EncodeToString(sig))
			}
			if s.CredentialID != cred.CredentialID {
				t.Fatalf("credential_id %s, want %s", s.CredentialID, cred.CredentialID)
			}
			n++
		})
	}
	if n != 17 {
		t.Fatalf("minted %d valid cases, want 17", n)
	}
}

// checkVectorsHash compares the SHA-256 of vectors.json with the hash recorded
// in SOURCE. Updating the vectors = copy a new file from Vault and record its
// new hash and commit; never edit the file by hand.
func checkVectorsHash(vectorsPath, sourcePath string) error {
	b, err := os.ReadFile(vectorsPath)
	if err != nil {
		return err
	}
	src, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	f := strings.Fields(string(src))
	if len(f) < 2 || f[len(f)-2] != "sha256" {
		return fmt.Errorf("SOURCE must end with: sha256 <hex>")
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != f[len(f)-1] {
		return fmt.Errorf("vectors.json sha256 %s, SOURCE records %s: the file was edited; copy it unchanged from quorum-vault", got, f[len(f)-1])
	}
	return nil
}

func TestVectorsUnchanged(t *testing.T) {
	if err := checkVectorsHash(filepath.Join(vectorsDir, "vectors.json"), filepath.Join(vectorsDir, "SOURCE")); err != nil {
		t.Fatal(err)
	}
}

// TestVectorsHashCatchesOneByte: the guard above must fail when one byte of
// the copied file changes.
func TestVectorsHashCatchesOneByte(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(vectorsDir, "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0x01
	p := filepath.Join(t.TempDir(), "vectors.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkVectorsHash(p, filepath.Join(vectorsDir, "SOURCE")); err == nil {
		t.Fatal("a one-byte change passed the hash check")
	}
}
