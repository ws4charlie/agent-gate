# SSH only through Google IAP. Tailscale needs no inbound rule.
resource "google_compute_firewall" "allow_iap_ssh" {
  name      = "agentgate-iap-only"
  network   = google_compute_network.agentgate.id
  direction = "INGRESS"
  priority  = 1000

  target_tags = ["agentgate"]

  source_ranges = ["35.235.240.0/20"]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}
