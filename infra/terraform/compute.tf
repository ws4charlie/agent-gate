resource "google_compute_instance" "agentgate" {
  name         = "agentgate"
  machine_type = var.machine_type
  zone         = var.zone

  tags = ["agentgate"]

  can_ip_forward      = false
  deletion_protection = false

  boot_disk {
    auto_delete = true
    device_name = "persistent-disk-0"

    initialize_params {
      image = "projects/debian-cloud/global/images/debian-12-bookworm-v20261102"
      size  = 30
      type  = "pd-standard"
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.agentgate.id

    # Public IP kept on purpose: no inbound rule from the internet (see the OPA rule).
    access_config {
      network_tier = "PREMIUM"
    }
  }

  scheduling {
    on_host_maintenance = "MIGRATE"
    automatic_restart   = true
    preemptible         = false
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
