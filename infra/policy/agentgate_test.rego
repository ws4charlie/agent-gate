package terraform.agentgate_test

import data.terraform.agentgate
import rego.v1

res(contents) := [{"path": "main.tf", "contents": contents}]

shield_ok := {"enable_secure_boot": true, "enable_vtpm": true, "enable_integrity_monitoring": true}

vm(extra) := res({"resource": {"google_compute_instance": {"vm": object.union(
	{
		"network_interface": [{"subnetwork": "s"}],
		"shielded_instance_config": [shield_ok],
	},
	extra,
)}}})

fw(ranges, ports) := res({"resource": {"google_compute_firewall": {"f": {
	"direction": "INGRESS",
	"source_ranges": ranges,
	"allow": [{"protocol": "tcp", "ports": ports}],
}}}})

# public IP
test_no_public_ip_passes if count(agentgate.deny) == 0 with input as vm({})

pub := {"network_interface": [{"access_config": [{}]}]}

fw_file(ranges) := {"resource": {"google_compute_firewall": {"f": {"direction": "INGRESS", "source_ranges": ranges, "allow": [{"protocol": "tcp", "ports": ["22"]}]}}}}

vm_file(extra) := {"resource": {"google_compute_instance": {"vm": object.union({"shielded_instance_config": [shield_ok]}, extra)}}}

test_public_ip_with_iap_only_passes if {
	count(agentgate.deny) == 0 with input as [
		{"path": "compute.tf", "contents": vm_file(pub)},
		{"path": "firewall.tf", "contents": fw_file(["35.235.240.0/20"])},
	]
}

test_public_ip_with_open_firewall_denied if {
	some msg in agentgate.deny with input as [
		{"path": "compute.tf", "contents": vm_file(pub)},
		{"path": "firewall.tf", "contents": fw_file(["203.0.113.0/24"])},
	]
	contains(msg, "public IP")
}

# firewall
test_iap_ssh_passes if count(agentgate.deny) == 0 with input as fw(["35.235.240.0/20"], ["22"])

test_open_ingress_denied if {
	some msg in agentgate.deny with input as fw(["0.0.0.0/0"], ["443"])
	contains(msg, "0.0.0.0/0")
}

test_ssh_from_elsewhere_denied if {
	some msg in agentgate.deny with input as fw(["10.0.0.0/8"], ["22"])
	contains(msg, "tcp:22")
}

# shielded VM
test_shielded_passes if count(agentgate.deny) == 0 with input as vm({})

test_not_shielded_denied if {
	some msg in agentgate.deny with input as vm({"shielded_instance_config": [{"enable_secure_boot": true, "enable_vtpm": false, "enable_integrity_monitoring": true}]})
	contains(msg, "enable_vtpm")
}

test_missing_shielded_denied if count(agentgate.deny) > 0 with input as res({"resource": {"google_compute_instance": {"vm": {}}}})

# broad roles
test_narrow_role_passes if count(agentgate.deny) == 0 with input as res({"resource": {"google_project_iam_member": {"m": {"role": "roles/viewer", "member": "user:a@b.c"}}}})

test_owner_denied if count(agentgate.deny) > 0 with input as res({"resource": {"google_project_iam_member": {"m": {"role": "roles/owner", "member": "user:a@b.c"}}}})

test_editor_denied if count(agentgate.deny) > 0 with input as res({"resource": {"google_project_iam_binding": {"m": {"role": "roles/editor", "members": ["user:a@b.c"]}}}})

# secret versions
test_secret_without_version_passes if count(agentgate.deny) == 0 with input as res({"resource": {"google_secret_manager_secret": {"s": {"secret_id": "x"}}}})

test_secret_version_denied if count(agentgate.deny) > 0 with input as res({"resource": {"google_secret_manager_secret_version": {"v": {"secret": "x"}}}})

# startup-script
test_plain_metadata_passes if count(agentgate.deny) == 0 with input as vm({"metadata": {"enable-oslogin": "TRUE"}})

test_startup_script_denied if count(agentgate.deny) > 0 with input as vm({"metadata": {"startup-script": "echo hi"}})
