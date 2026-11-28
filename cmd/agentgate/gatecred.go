package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ws4charlie/agent-gate/internal/gatecred"
)

// gatecredCmd: gatecred keygen [--file path] [--kid name] makes the Ed25519
// key Gate signs QuorumVault credentials with and prints the kid and public
// key for Vault's operators; the seed never leaves the file.
// gatecred install --from path --to path copies a key into the form the
// gateway's loader accepts (see gatecredInstall).
func gatecredCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("gatecred: need keygen [--file path] [--kid name] | install --from path --to path")
	}
	switch args[0] {
	case "keygen":
		return gatecredKeygen(args[1:], stdout)
	case "install":
		return gatecredInstallCmd(args[1:], stdout)
	}
	return fmt.Errorf("gatecred: unknown subcommand %q (keygen | install)", args[0])
}

func gatecredKeygen(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("gatecred keygen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", envOr(gatecred.EnvKeyFile, gatecred.DefaultKeyFile), "seed file to create (mode 0600, gitignored; default $"+gatecred.EnvKeyFile+")")
	kid := fs.String("kid", "gate-"+time.Now().UTC().Format("20060102"), "key id Vault will know this key by (A-Z a-z 0-9 . _ -, 1-64)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("gatecred keygen: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("gatecred keygen: unexpected argument %q", fs.Arg(0))
	}
	return gatecred.Keygen(stdout, *file, *kid)
}

func gatecredInstallCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("gatecred install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "key file to copy (e.g. a Secret volume file: symlinks and any owner are fine)")
	to := fs.String("to", envOr(gatecred.EnvKeyFile, ""), "where the gateway reads it (default $"+gatecred.EnvKeyFile+")")
	kid := fs.String("kid", os.Getenv(gatecred.EnvKid), "if set, load the result with the gateway's own checks (default $"+gatecred.EnvKid+")")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("gatecred install: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("gatecred install: unexpected argument %q", fs.Arg(0))
	}
	if *from == "" || *to == "" {
		return errors.New("gatecred install: need --from and --to")
	}
	if err := gatecredInstall(*from, *to); err != nil {
		return err
	}
	if *kid != "" {
		// The gateway's strict loader, unchanged: the install is only done if it accepts the file.
		if _, err := gatecred.LoadKeyFile(*to, *kid); err != nil {
			return fmt.Errorf("gatecred install: the gateway would refuse the result: %w", err)
		}
	}
	fmt.Fprintf(stdout, "gatecred install: %s (mode 0600, owner uid %d)\n", *to, os.Getuid())
	return nil
}

// gatecredInstall copies the seed at from (following symlinks, any owner: a
// Kubernetes Secret volume file is a root-owned symlink, mode 0440 or so)
// into to, as a regular file owned by this user with mode 0600, in a
// directory group and others cannot write to (created 0700 if missing). It
// writes a temporary file and renames it into place, so a symlink or an old
// file at to is replaced, never written through. The source must hold a
// 32-byte hex seed; its content is never printed.
func gatecredInstall(from, to string) error {
	raw, err := readLimited(from, 256)
	if err != nil {
		return fmt.Errorf("gatecred install: read %s: %w", from, err)
	}
	defer clear(raw)
	seed := bytes.TrimSpace(raw)
	if n := hex.DecodedLen(len(seed)); n != 32 {
		return fmt.Errorf("gatecred install: %s must hold a 32-byte seed as hex", from)
	}
	buf := make([]byte, 32)
	defer clear(buf)
	if _, err := hex.Decode(buf, seed); err != nil {
		return fmt.Errorf("gatecred install: %s must hold a 32-byte seed as hex", from)
	}
	dir := filepath.Dir(to)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("gatecred install: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("gatecred install: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("gatecred install: %s is not a directory", dir)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("gatecred install: directory %s has mode %04o; group or others can write to it (chmod 700)", dir, perm)
	}
	tmp, err := os.CreateTemp(dir, ".gate-cred-*")
	if err != nil {
		return fmt.Errorf("gatecred install: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("gatecred install: %w", err)
	}
	if _, err := tmp.Write(append(append([]byte{}, seed...), '\n')); err != nil {
		return fmt.Errorf("gatecred install: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("gatecred install: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("gatecred install: %w", err)
	}
	if err := os.Rename(tmp.Name(), to); err != nil {
		return fmt.Errorf("gatecred install: %w", err)
	}
	ok = true
	return nil
}

// readLimited reads at most max bytes of path (following symlinks) and
// refuses a larger file.
func readLimited(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		clear(b)
		return nil, errors.New("file too large for a seed")
	}
	return b, nil
}
