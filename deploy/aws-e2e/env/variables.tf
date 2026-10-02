# SPDX-License-Identifier: FSL-1.1-ALv2

variable "state_dir" {
  description = "Absolute directory that holds the per-layer local state files (outside the repo). Required at `tofu init`; also locates the base layer's state."
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

# --- AMIs (inputs from the images agent / public SSM parameters) --------------------------

variable "windows_client_ami" {
  description = "AMI id of the windows-base image (Windows Server 2025 + VS Build Tools, built by Packer). Empty = do not create the Windows client yet."
  type        = string
  default     = ""

  validation {
    condition     = var.windows_client_ami == "" || can(regex("^ami-[0-9a-f]{8}([0-9a-f]{9})?$", var.windows_client_ami))
    error_message = "windows_client_ami must be empty or an AMI id such as ami-0123456789abcdef0."
  }
}

variable "k3s_ami" {
  description = "Override for the k3s node AMI. Empty = resolve Ubuntu 26.04 from the public SSM parameter."
  type        = string
  default     = ""

  validation {
    condition     = var.k3s_ami == "" || can(regex("^ami-[0-9a-f]{8}([0-9a-f]{9})?$", var.k3s_ami))
    error_message = "k3s_ami must be empty or an AMI id."
  }
}

variable "linux_client_ami" {
  description = "Override for the linux-client AMI. Empty = resolve Ubuntu 26.04 from the public SSM parameter."
  type        = string
  default     = ""

  validation {
    condition     = var.linux_client_ami == "" || can(regex("^ami-[0-9a-f]{8}([0-9a-f]{9})?$", var.linux_client_ami))
    error_message = "linux_client_ami must be empty or an AMI id."
  }
}

variable "ubuntu_ami_ssm_parameter" {
  description = "Public SSM parameter that resolves the current Ubuntu 26.04 LTS amd64 AMI."
  type        = string
  default     = "/aws/service/canonical/ubuntu/server/26.04/stable/current/amd64/hvm/ebs-gp3/ami-id"
}

# --- sizing ------------------------------------------------------------------------------

variable "k3s_instance_type" {
  description = "Instance type of the k3s node. Small functional-test default (ADR 0004); larger tests require an explicit operator request."
  type        = string
  default     = "m7i.large"
}

variable "client_instance_type" {
  description = "Instance type of linux-client and windows-client. Small functional-test default (ADR 0004); larger tests require an explicit operator request."
  type        = string
  default     = "m7i.large"
}

variable "k3s_private_ip_host" {
  description = "Host number of the k3s node's FIXED private IP in the public subnet (10 = 10.42.0.10 for the default VPC CIDR). Fixed so that certificates, chart values and in-VPC clients can name the control plane before the instance exists and across replacements."
  type        = number
  default     = 10

  validation {
    condition     = var.k3s_private_ip_host >= 4 && var.k3s_private_ip_host <= 250 && floor(var.k3s_private_ip_host) == var.k3s_private_ip_host
    error_message = "k3s_private_ip_host must be an integer between 4 and 250 (AWS reserves the first four addresses of a subnet)."
  }
}

variable "k3s_cluster_cidr" {
  description = "Pod CIDR of the k3s cluster. k3s defaults to 10.42.0.0/16, which is the VPC's own range here (flannel's cni0 would take the subnet gateway's address), so it must not overlap the VPC."
  type        = string
  default     = "10.244.0.0/16"

  validation {
    condition     = can(cidrhost(var.k3s_cluster_cidr, 0))
    error_message = "k3s_cluster_cidr must be an IPv4 CIDR."
  }
}

variable "k3s_service_cidr" {
  description = "Service CIDR of the k3s cluster (k3s default: 10.43.0.0/16). Must not overlap the VPC or the pod CIDR."
  type        = string
  default     = "10.245.0.0/16"

  validation {
    condition     = can(cidrhost(var.k3s_service_cidr, 0)) && tonumber(split("/", var.k3s_service_cidr)[1]) <= 24
    error_message = "k3s_service_cidr must be an IPv4 CIDR no smaller than a /24."
  }
}

variable "k3s_ecr_credential_provider" {
  description = "Install a kubelet image credential provider so the node pulls cucina/* images from ECR with its instance profile."
  type        = bool
  default     = true
}

variable "k3s_root_volume_gib" {
  type    = number
  default = 30
}

variable "k3s_data_volume_gib" {
  description = "Size of the data volume mounted at /var/lib/rancher/k3s/storage (Buildbarn storage PVCs)."
  type        = number
  default     = 300

  validation {
    condition     = var.k3s_data_volume_gib >= 100
    error_message = "The Buildbarn storage volume must be at least 100 GiB."
  }
}

variable "k3s_data_volume_iops" {
  type    = number
  default = 6000

  validation {
    condition     = var.k3s_data_volume_iops >= 6000 && var.k3s_data_volume_iops <= 16000
    error_message = "gp3 IOPS must be between 6000 (campaign minimum, §10.1) and 16000."
  }
}

variable "k3s_data_volume_throughput_mibps" {
  type    = number
  default = 500

  validation {
    condition     = var.k3s_data_volume_throughput_mibps >= 500 && var.k3s_data_volume_throughput_mibps <= 1000
    error_message = "gp3 throughput must be between 500 MiB/s (campaign minimum, §10.1) and 1000 MiB/s."
  }
}

variable "linux_client_root_volume_gib" {
  type    = number
  default = 100
}

variable "windows_client_root_volume_gib" {
  description = "Must be at least the windows-base AMI's root snapshot size; EC2Launch extends the partition."
  type        = number
  default     = 150
}

variable "guard_minutes" {
  description = "Runaway guard: the k3s node and the clients power off (instances stop) after this many minutes unless extended. 480 = the 8 h of §12."
  type        = number
  default     = 480

  validation {
    condition     = var.guard_minutes >= 30 && var.guard_minutes <= 720
    error_message = "guard_minutes must be between 30 and 720 (the §12 guard is 8 h = 480)."
  }
}

# --- pinned external inputs (R: pin everything external, PROMPT.md §0.5) -------------------

variable "k3s_version" {
  description = "k3s release (stable channel pin, R-LIB-4)."
  type        = string
  default     = "v1.36.5+k3s1"
}

variable "k3s_sha256_amd64" {
  description = "SHA-256 of the k3s amd64 release binary for k3s_version."
  type        = string
  default     = "d73847bcd3c5fccef0115b372e2f9a91f3032dc84bbf71518a4617565294d313"

  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.k3s_sha256_amd64))
    error_message = "k3s_sha256_amd64 must be a lowercase hex SHA-256."
  }
}

variable "k3s_install_sha256" {
  description = "SHA-256 of install.sh at the k3s_version tag of k3s-io/k3s (run with INSTALL_K3S_SKIP_DOWNLOAD=true)."
  type        = string
  default     = "46177d4c99440b4c0311b67233823a8e8a2fc09693f6c89af1a7161e152fbfad"

  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.k3s_install_sha256))
    error_message = "k3s_install_sha256 must be a lowercase hex SHA-256."
  }
}

variable "bazelisk_version" {
  type    = string
  default = "1.29.0"
}

variable "bazelisk_linux_amd64_sha256" {
  type    = string
  default = "5a408715e932c0250d28bd84555f12edbf70117de42f9181691c736eacc4a992"

  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.bazelisk_linux_amd64_sha256))
    error_message = "bazelisk_linux_amd64_sha256 must be a lowercase hex SHA-256."
  }
}

variable "bazelisk_windows_amd64_sha256" {
  type    = string
  default = "092a8738d5b41aae7a85c42cc961b1034e3389aba43ffc20c0fabda7b43e095b"

  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.bazelisk_windows_amd64_sha256))
    error_message = "bazelisk_windows_amd64_sha256 must be a lowercase hex SHA-256."
  }
}

variable "git_for_windows_version" {
  description = "Git for Windows release (the tag without the v prefix, e.g. 2.56.0.windows.1)."
  type        = string
  default     = "2.56.0.windows.1"
}

variable "git_for_windows_installer" {
  description = "Installer asset name of git_for_windows_version."
  type        = string
  default     = "Git-2.56.0-64-bit.exe"
}

variable "git_for_windows_sha256" {
  type    = string
  default = "bfe94e7b419b16eee9fecbd1253a98e3d4f49ba8f029630549052278ffe286a6"

  validation {
    condition     = can(regex("^[0-9a-f]{64}$", var.git_for_windows_sha256))
    error_message = "git_for_windows_sha256 must be a lowercase hex SHA-256."
  }
}
