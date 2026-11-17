resource "google_compute_network" "agentgate" {
  name                    = "agentgate-net"
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"
}

resource "google_compute_subnetwork" "agentgate" {
  name                     = "agentgate-sub"
  region                   = var.region
  network                  = google_compute_network.agentgate.id
  ip_cidr_range            = var.network_cidr
  private_ip_google_access = false
}
