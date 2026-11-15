# Existing resources, adopted by the first apply. IDs are for project
# stone-column-232922. The ids are written out because import ids cannot use variables.
import {
  to = google_compute_network.agentgate
  id = "projects/stone-column-232922/global/networks/agentgate-net"
}

import {
  to = google_compute_subnetwork.agentgate
  id = "projects/stone-column-232922/regions/us-central1/subnetworks/agentgate-subnet"
}

import {
  to = google_compute_firewall.allow_iap_ssh
  id = "projects/stone-column-232922/global/firewalls/agentgate-allow-iap-ssh"
}

import {
  to = google_compute_router.agentgate
  id = "projects/stone-column-232922/regions/us-central1/routers/agentgate-router"
}

import {
  to = google_compute_router_nat.agentgate
  id = "stone-column-232922/us-central1/agentgate-router/agentgate-nat"
}

import {
  to = google_compute_instance.agentgate
  id = "projects/stone-column-232922/zones/us-central1-a/instances/agentgate"
}

import {
  to = google_service_account.agentgate_vm
  id = "projects/stone-column-232922/serviceAccounts/agentgate-vm@stone-column-232922.iam.gserviceaccount.com"
}

import {
  to = google_secret_manager_secret.agentgate_env
  id = "projects/stone-column-232922/secrets/agentgate-env"
}

import {
  to = google_secret_manager_secret_iam_member.agentgate_env_accessor
  id = "projects/stone-column-232922/secrets/agentgate-env roles/secretmanager.secretAccessor serviceAccount:agentgate-vm@stone-column-232922.iam.gserviceaccount.com"
}
