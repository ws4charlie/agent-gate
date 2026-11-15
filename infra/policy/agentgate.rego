# Checks on the Terraform in infra/terraform, run by conftest (--parser hcl2).
# conftest hands each .tf file over as parsed HCL: resource.<type>.<name> is a
# list-wrapped block, so nested blocks may appear as arrays.
package terraform.agentgate

import rego.v1

# Every resource of a type: [name, body].
resources(type) := {[name, body] |
	some name, bodies in object.get(input, ["resource", type], {})
	some body in as_list(bodies)
}

as_list(x) := x if is_array(x)

as_list(x) := [x] if not is_array(x)

blocks(body, key) := [b |
	some v in as_list(object.get(body, key, []))
	some b in as_list(v)
]

deny contains msg if {
	some [name, body] in resources("google_compute_instance")
	some ni in blocks(body, "network_interface")
	count(blocks(ni, "access_config")) > 0
	msg := sprintf("google_compute_instance.%s has an access_config, which gives it a public IP", [name])
}

public_range := {"0.0.0.0/0", "::/0"}

deny contains msg if {
	some [name, body] in resources("google_compute_firewall")
	object.get(body, "direction", "INGRESS") == "INGRESS"
	some r in body.source_ranges
	r in public_range
	msg := sprintf("google_compute_firewall.%s allows ingress from %s", [name, r])
}

deny contains msg if {
	some [name, body] in resources("google_compute_firewall")
	object.get(body, "direction", "INGRESS") == "INGRESS"
	some a in blocks(body, "allow")
	"22" in a.ports
	some r in body.source_ranges
	r != "35.235.240.0/20"
	msg := sprintf("google_compute_firewall.%s allows tcp:22 from %s; only the IAP range 35.235.240.0/20 is allowed", [name, r])
}

shield_flags := ["enable_secure_boot", "enable_vtpm", "enable_integrity_monitoring"]

deny contains msg if {
	some [name, body] in resources("google_compute_instance")
	count(blocks(body, "shielded_instance_config")) == 0
	msg := sprintf("google_compute_instance.%s has no shielded_instance_config", [name])
}

deny contains msg if {
	some [name, body] in resources("google_compute_instance")
	some s in blocks(body, "shielded_instance_config")
	some flag in shield_flags
	object.get(s, flag, false) != true
	msg := sprintf("google_compute_instance.%s must set %s = true", [name, flag])
}

iam_types := {
	"google_project_iam_member",
	"google_project_iam_binding",
	"google_project_iam_policy",
	"google_folder_iam_member",
	"google_folder_iam_binding",
	"google_organization_iam_member",
	"google_organization_iam_binding",
	"google_service_account_iam_member",
	"google_service_account_iam_binding",
	"google_secret_manager_secret_iam_member",
	"google_secret_manager_secret_iam_binding",
}

broad_roles := {"roles/owner", "roles/editor"}

deny contains msg if {
	some type in iam_types
	some [name, body] in resources(type)
	body.role in broad_roles
	msg := sprintf("%s.%s grants %s, which is too broad", [type, name, body.role])
}

deny contains msg if {
	some [name, _] in resources("google_secret_manager_secret_version")
	msg := sprintf("google_secret_manager_secret_version.%s: secret payloads must never be in Terraform", [name])
}

deny contains msg if {
	some [name, body] in resources("google_compute_instance")
	some m in as_list(object.get(body, "metadata", {}))
	"startup-script" in object.keys(m)
	msg := sprintf("google_compute_instance.%s sets metadata startup-script; app config stays out of Terraform", [name])
}
