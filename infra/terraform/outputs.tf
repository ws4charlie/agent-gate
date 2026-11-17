output "instance_name" {
  value = google_compute_instance.agentgate.name
}

output "service_account_email" {
  value = google_service_account.agentgate_vm.email
}

output "network" {
  value = google_compute_network.agentgate.name
}
