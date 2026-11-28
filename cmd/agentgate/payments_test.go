package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ws4charlie/agent-gate/internal/x402"
)

// The payments file must name its network: an empty one no longer means the
// simulated chain. readPaymentsFile is what serve, console, report and
// reconcile all read the file with.
func TestPaymentsNetworkRequired(t *testing.T) {
	for _, tc := range []struct {
		name, json, wantErr string
	}{
		{"missing", `{"tools":{}}`, `"network" is required: sim, anvil or base-sepolia`},
		{"empty", `{"network":""}`, `"network" is required: sim, anvil or base-sepolia`},
		{"sim", `{"network":"sim"}`, ""},
		{"anvil", `{"network":"anvil"}`, ""},
		{"base-sepolia", `{"network":"base-sepolia"}`, ""},
		{"junk", `{"network":"mainnet"}`, `"network" is "mainnet": want sim, anvil or base-sepolia`},
		{"case", `{"network":"Sim"}`, `"network" is "Sim"`},
		{"space", `{"network":" sim"}`, `"network" is " sim"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "payments.json")
			if err := os.WriteFile(p, []byte(tc.json), 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := readPaymentsFile(p)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("got %v, want ok", err)
				}
				var reported uint64 // what an anvil node reports; sim and base-sepolia ask none here
				if f.Network == x402.NetworkAnvil {
					reported = x402.AnvilChainID
				}
				if _, err := chainIDFor(f.Network, reported); err != nil {
					t.Fatalf("chainIDFor(%q): %v", f.Network, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
	if _, err := chainIDFor("", x402.AnvilChainID); err == nil {
		t.Fatal(`chainIDFor(""): want an error, not the simulated chain`)
	}
}
