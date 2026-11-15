# lead: confirm from plan that a router and NAT exist; remove this file and
# their import blocks if the VM reaches the internet another way.
resource "google_compute_router" "agentgate" {
  name    = "agentgate-router"
  region  = var.region
  network = google_compute_network.agentgate.id
}

resource "google_compute_router_nat" "agentgate" {
  name                               = "agentgate-nat"
  router                             = google_compute_router.agentgate.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"
}
