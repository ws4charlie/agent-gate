# Infrastructure

The GCP deployment is described as code in `infra/terraform`. It covers the network, the SSH firewall rule, the VM, its service account and the one secret it may read. It does not cover what runs inside the VM or the secret's contents.

The code was written after the deployment existed, so it adopts the live resources instead of creating new ones. A plan against the real project should show no changes. Any difference is drift, and either the code or the project is wrong.

## What the policies guard



- the VM may have a public address only while every inbound rule admits Google's IAP range alone;
- nothing accepts traffic from the whole internet, and SSH comes only through the IAP range;
- the VM stays a Shielded VM;
- no one is granted the owner or editor role;
- no secret value is stored in Terraform;
- no startup script carries app configuration.

Each rule has a passing and a failing test.

## What CI does not do

CI never talks to GCP and holds no credentials. It checks formatting, validity and policy only. Plans and applies run on the lead's machine, with state in a bucket the human creates once.
