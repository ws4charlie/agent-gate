# Bucket and prefix are passed at init: terraform init -backend-config=...
terraform {
  backend "gcs" {}
}
