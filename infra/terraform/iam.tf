resource "google_service_account" "agentgate_vm" {
  account_id   = "agentgate-vm"
  display_name = "AgentGate VM (reads one secret)"
}

# The only grant: read the one secret. No project-level roles.
resource "google_secret_manager_secret_iam_member" "agentgate_env_accessor" {
  secret_id = google_secret_manager_secret.agentgate_env.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.agentgate_vm.email}"
}
