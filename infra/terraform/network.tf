resource "google_compute_network" "agentgate" {
  name                    = "agentgate-net"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "agentgate" {
  name          = "agentgate-subnet" # lead: confirm name from plan
  region        = var.region
  network       = google_compute_network.agentgate.id
  ip_cidr_range = var.network_cidr
}
