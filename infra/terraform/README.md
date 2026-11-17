# AgentGate Terraform

Describes the existing GCP deployment. Nothing here changes the live project until someone runs `apply`.

1. Once, by hand: create a GCS bucket for state, with versioning on.
2. Create `backend.hcl` (not committed) with `bucket = "<bucket>"` and `prefix = "agentgate"`, then run `terraform init -backend-config=backend.hcl`.
3. Run `terraform plan`. The `import` blocks adopt the existing resources, so the plan should show only imports and no other changes.
4. Any other change in the plan is drift between this code and the live project. Fix the code, not the project, unless the change is intended.

Secret versions and the VM's contents are never managed here.
