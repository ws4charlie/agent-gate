variable "project" {
  type    = string
  default = "stone-column-232922"
}

variable "region" {
  type    = string
  default = "us-central1"
}

variable "zone" {
  type    = string
  default = "us-central1-a"
}

variable "network_cidr" {
  description = "Range of the agentgate subnet."
  type        = string
  default     = "10.10.0.0/24" # lead: confirm from plan
}

variable "machine_type" {
  type    = string
  default = "e2-small"
}
