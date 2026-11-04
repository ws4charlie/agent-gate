package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type Scenario struct {
	ID          string `json:"id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Setup       Setup  `json:"setup"`
	Steps       []Step `json:"steps"`
	Expected    string `json:"expected"` // allow | deny | approval
	Rule        string `json:"rule"`     // the rule id the last step must report ("allow" for allow)
}

// Setup is the agent's scope. Money is in USD. "@payee" stands for the paid
// tool's address and "@other" for an address that is not it.
type Setup struct {
	Models       []string `json:"models"`
	Tools        []string `json:"tools"`
	Payees       []string `json:"payees"`
	Daily        float64  `json:"daily"`
	Task         float64  `json:"task"`
	Velocity     float64  `json:"velocity"`
	ApprovalOver float64  `json:"approval_over"`
}

// Step ops: chat, tool, pay, screen (a corpus file through the output screen),
// kill, revoke, kill_run, approve.
type Step struct {
	Op       string         `json:"op"`
	Model    string         `json:"model,omitempty"`
	Tool     string         `json:"tool,omitempty"` // tool name (server "up") or paid tool
	Args     map[string]any `json:"args,omitempty"`
	Idem     string         `json:"idem,omitempty"` // Idempotency-Key; a replay reuses it
	Task     string         `json:"task,omitempty"` // X-AgentGate-Task
	Run      string         `json:"run,omitempty"`  // X-AgentGate-Run
	File     string         `json:"file,omitempty"` // screen: path under internal/screen/testdata
	Repeat   int            `json:"repeat,omitempty"`
	Parallel bool           `json:"parallel,omitempty"` // run the repeats at once; the outcome is the first refusal, if any
	BadKey   bool           `json:"bad_key,omitempty"`  // use a credential that does not exist
}

func loadScenarios(root string) ([]Scenario, error) {
	files, err := filepath.Glob(filepath.Join(root, "benchmark", "scenarios", "*.json"))
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("no scenarios in %s/benchmark/scenarios: %v", root, err)
	}
	sort.Strings(files)
	var all []Scenario
	seen := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var part []Scenario
		dec := json.NewDecoder(bytesReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&part); err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, s := range part {
			switch {
			case s.ID == "" || seen[s.ID]:
				return nil, fmt.Errorf("%s: empty or duplicate id %q", f, s.ID)
			case s.Expected != "allow" && s.Expected != "deny" && s.Expected != "approval":
				return nil, fmt.Errorf("%s: %s: expected must be allow, deny or approval", f, s.ID)
			case s.Rule == "" || len(s.Steps) == 0 || s.Category == "":
				return nil, fmt.Errorf("%s: %s: needs category, steps and rule", f, s.ID)
			}
			seen[s.ID] = true
			all = append(all, s)
		}
	}
	return all, nil
}
