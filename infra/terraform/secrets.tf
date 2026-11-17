# Only the secret container. Versions and payload are never managed by Terraform.
resource "google_secret_manager_secret" "agentgate_env" {
  secret_id = "agentgate-env"

  replication {
    user_managed {
      replicas {
        location = "us-central1"
      }
    }
  }
}
