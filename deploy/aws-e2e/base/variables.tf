# SPDX-License-Identifier: FSL-1.1-ALv2

variable "state_dir" {
  description = "Absolute directory that holds the per-layer local state files (outside the repo), e.g. ~/.config/cucina/aws-e2e. Required at `tofu init`."
  type        = string

  validation {
    condition     = startswith(var.state_dir, "/")
    error_message = "state_dir must be an absolute path."
  }
}

variable "region" {
  description = "AWS region. The campaign is pinned to us-west-1 (§12)."
  type        = string
  default     = "us-west-1"

  validation {
    condition     = var.region == "us-west-1"
    error_message = "The e2e environment is restricted to us-west-1 (PROMPT.md §12)."
  }
}

variable "run_id" {
  description = "Value of the cucina:run tag (env.sh: CUCINA_RUN_ID)."
  type        = string
  default     = "e2e-20261002a"

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{2,62}$", var.run_id))
    error_message = "run_id must be lowercase alphanumerics and dashes, 3-63 characters."
  }
}

variable "expires" {
  description = "Value of the cucina:expires tag, UTC ISO-8601 (env.sh: CUCINA_EXPIRES)."
  type        = string
  default     = "2026-10-09T00:00:00Z"

  validation {
    condition     = can(regex("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$", var.expires))
    error_message = "expires must be a UTC ISO-8601 timestamp such as 2026-10-09T00:00:00Z."
  }
}

variable "admin_cidrs" {
  description = "IPv4 CIDRs allowed to reach the control plane from outside the VPC: the dev Mac's public IP as /32. Passed at apply time by scripts/up.sh, never committed."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for c in var.admin_cidrs : can(cidrhost(c, 0)) && !strcontains(c, ":") && tonumber(split("/", c)[1]) >= 24])
    error_message = "Every admin CIDR must be an IPv4 CIDR with a prefix length of /24 or longer (no 0.0.0.0/0, no wide ranges)."
  }
}

variable "mac_site_cidrs" {
  description = "IPv4 CIDRs of Mac sites that connect to the worker endpoint over the internet. null means: same as admin_cidrs (the dev Mac is both the admin and the one Mac site)."
  type        = list(string)
  default     = null

  validation {
    condition     = var.mac_site_cidrs == null ? true : alltrue([for c in var.mac_site_cidrs : can(cidrhost(c, 0)) && !strcontains(c, ":") && tonumber(split("/", c)[1]) >= 24])
    error_message = "Every Mac-site CIDR must be an IPv4 CIDR with a prefix length of /24 or longer."
  }
}

variable "az" {
  description = "Availability zone for every resource. Empty means: pick the first of allowed_azs that offers all required_instance_types."
  type        = string
  default     = ""

  validation {
    condition     = var.az == "" || contains(["us-west-1b", "us-west-1c"], var.az)
    error_message = "az must be empty, us-west-1b or us-west-1c."
  }
}

variable "allowed_azs" {
  description = "Candidate AZs, in order of preference."
  type        = list(string)
  default     = ["us-west-1b", "us-west-1c"]
}

variable "required_instance_types" {
  description = "Instance types that must all be offered in the chosen AZ (§10.1 pools, k3s node and clients)."
  type        = list(string)
  default = [
    "c8i.8xlarge", "c7i.8xlarge", "c7a.8xlarge",
    "c8g.8xlarge", "c7g.8xlarge",
    "m6id.8xlarge", "c7gd.8xlarge",
    "m8i.2xlarge", "m7i.xlarge",
  ]
}

variable "vpc_cidr" {
  description = "IPv4 CIDR of the dedicated VPC (RFC 1918)."
  type        = string
  default     = "10.42.0.0/16"

  validation {
    condition     = can(cidrhost(var.vpc_cidr, 0)) && tonumber(split("/", var.vpc_cidr)[1]) <= 20
    error_message = "vpc_cidr must be a valid IPv4 CIDR no smaller than /20 (two /24-or-larger subnets are carved from it)."
  }
}

variable "client_endpoint_ports" {
  description = "TCP ports of the client-facing TLS endpoints on the control-plane node: remote execution 443, STS 8443, management API 8444 (charts/cucina endpoints.*). Reachable from admin_cidrs and the clients security group."
  type        = list(number)
  default     = [443, 8443, 8444]
}

variable "worker_endpoint_ports" {
  description = "TCP ports of the worker-facing endpoints: storage 8981, scheduler 8983, enrollment 8445, Mac hosts 8446 (charts/cucina endpoints.worker/hosts). Reachable from the workers security group and from mac_site_cidrs."
  type        = list(number)
  default     = [8981, 8983, 8445, 8446]
}

variable "worker_scrape_port_ranges" {
  description = "TCP port ranges on workers that the control plane may scrape (Prometheus HTTP SD: node/windows exporters, bb_worker diagnostics)."
  type = list(object({
    from = number
    to   = number
  }))
  default = [
    { from = 9100, to = 9199 },
    { from = 9980, to = 9989 },
  ]
}

variable "k3s_api_port" {
  description = "Kubernetes API port of the k3s node (admin CIDRs only)."
  type        = number
  default     = 6443
}

variable "builder_remote_ports" {
  description = "TCP ports opened from admin_cidrs on the Packer builders security group (SSH, WinRM HTTP/HTTPS, RDP)."
  type        = list(number)
  default     = [22, 5985, 5986, 3389]
}

variable "client_remote_ports" {
  description = "TCP ports opened from admin_cidrs on the clients security group. Default none: use SSM Session Manager."
  type        = list(number)
  default     = []
}

variable "ecr_repositories" {
  description = "Repository names under the cucina/ namespace."
  type        = list(string)
  default     = ["controller", "sts"]
}

variable "fast_launch_prep_instance_type" {
  description = "Instance type of Fast Launch prep instances (billed only while a snapshot is being prepared)."
  type        = string
  default     = "m7i.large"
}
