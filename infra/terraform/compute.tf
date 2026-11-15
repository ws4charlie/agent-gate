resource "google_compute_instance" "agentgate" {
  name         = "agentgate"
  machine_type = var.machine_type
  zone         = var.zone

  boot_disk {
    initialize_params {
      image = "debian-cloud/debian-12" # lead: confirm from plan
    }
  }

  # No access_config block: the VM has no public IP.
  network_interface {
    subnetwork = google_compute_subnetwork.agentgate.id
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  service_account {
    email  = google_service_account.agentgate_vm.email
    scopes = ["cloud-platform"]
  }

  # What runs inside the VM is not managed here, and the image drifts with updates.
  lifecycle {
    ignore_changes = [metadata, boot_disk[0].initialize_params[0].image]
  }
}
