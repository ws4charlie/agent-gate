package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/ws4charlie/agent-gate/internal/gatecred"
)

// gatecredCmd: gatecred keygen [--file path] [--kid name]. It makes the
// Ed25519 key Gate signs QuorumVault credentials with and prints the kid and
// public key for Vault's operators. The seed never leaves the file.
func gatecredCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "keygen" {
		return errors.New("gatecred: need keygen [--file path] [--kid name]")
	}
	fs := flag.NewFlagSet("gatecred keygen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", envOr(gatecred.EnvKeyFile, gatecred.DefaultKeyFile), "seed file to create (mode 0600, gitignored; default $"+gatecred.EnvKeyFile+")")
	kid := fs.String("kid", "gate-"+time.Now().UTC().Format("20060102"), "key id Vault will know this key by (A-Z a-z 0-9 . _ -, 1-64)")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("gatecred keygen: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("gatecred keygen: unexpected argument %q", fs.Arg(0))
	}
	return gatecred.Keygen(stdout, *file, *kid)
}
