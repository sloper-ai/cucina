# SPDX-License-Identifier: FSL-1.1-ALv2

variable "region" {
  type    = string
  default = "us-west-1"
}

# Network for the build instance. The Makefile reads these from the e2e environment outputs
# (~/.config/cucina/aws-e2e/base-outputs.json); nothing environment-specific is committed.
variable "vpc_id" {
  type = string
}

variable "subnet_id" {
  type        = string
  description = "Public subnet: the builder needs a public IPv4 for WinRM from the admin /32 and for downloads."
}

variable "security_group_ids" {
  type        = list(string)
  default     = []
  description = "Existing builder SG (WinRM 5986 from the admin /32 only). Empty = Packer creates a temporary SG restricted to admin_cidrs."
}

variable "admin_cidrs" {
  type        = list(string)
  description = "CIDRs allowed to reach the temporary builder SG (this Mac's /32). Never 0.0.0.0/0."
  validation {
    condition     = length(var.admin_cidrs) > 0 && alltrue([for c in var.admin_cidrs : c != "0.0.0.0/0" && c != "::/0"])
    error_message = "The admin_cidrs variable must list specific CIDRs (no 0.0.0.0/0 or ::/0)."
  }
}

variable "base_instance_type" {
  type    = string
  default = "m7i.large"
}

variable "worker_instance_type" {
  type    = string
  default = "m7i.large"
}

variable "root_volume_size" {
  type        = number
  default     = 40
  description = "GiB. Windows Server 2025 (~15 GiB) + VS Build Tools/SDK (~8 GiB) + headroom; the worker stage inherits it."
}

variable "worker_root_volume_size" {
  type        = number
  default     = 60
  description = "GiB for the worker AMI: base (~31 GiB used) + headroom for C:\\bb\\tmp, the pagefile and logs when a pool has no data volume. EBS snapshots (and Fast Launch snapshots) bill used blocks only."
}

variable "image_version" {
  type        = string
  description = "Version label (tag cucina:image-version); maps to a pool generation (R-POOL-8)."
}

variable "generation" {
  type        = string
  description = "Pool generation number for this image family (tag cucina:generation)."
}

variable "base_ami" {
  type        = string
  default     = ""
  description = "Worker stage: explicit base AMI ID. Empty = newest own AMI tagged cucina:image-family=windows-base."
}

variable "defender_mode" {
  type        = string
  default     = "exclusions"
  description = "exclusions (default) = Defender path/process exclusions; devdrive = also format data volumes as a Dev Drive (ReFS) at boot."
  validation {
    condition     = contains(["exclusions", "devdrive"], var.defender_mode)
    error_message = "The defender_mode variable must be exclusions or devdrive."
  }
}

variable "worker_agent_path" {
  type        = string
  default     = ""
  description = "Optional cucina-worker-agent (windows_amd64): a directory of cucina-worker-agent.zip.part-* files (see Makefile), a .zip or an .exe. Empty or missing = the bootstrap hook is a no-op."
}

variable "sysprep_gate_file" {
  type        = string
  default     = ""
  description = "Optional local file the worker build waits for (content must start with 'pass') before sysprep, e.g. the Bazel/MSVC verification result."
}

variable "enable_fast_launch" {
  type        = bool
  default     = false
  description = "Enable EC2 Fast Launch on the new worker AMI via scripts/fast-launch.sh (post-processor)."
}

variable "fast_launch_target_count" {
  type        = number
  default     = 4
  description = "Pre-provisioned snapshots = the Windows pool max (R-POOL-2)."
}

variable "fast_launch_template_id" {
  type        = string
  default     = ""
  description = "Launch template for Fast Launch prep instances (private subnet, small type). Empty = fast-launch.sh creates a tagged one."
}

variable "amis_file" {
  type    = string
  default = ""
}

variable "cucina_env" {
  type    = string
  default = "e2e"
}

variable "cucina_run" {
  type    = string
  default = env("CUCINA_RUN_ID")
  validation {
    condition     = length(var.cucina_run) > 0
    error_message = "Set CUCINA_RUN_ID (source .work/env.sh) or pass -var cucina_run=..."
  }
}

variable "cucina_expires" {
  type    = string
  default = env("CUCINA_EXPIRES")
  validation {
    condition     = length(var.cucina_expires) > 0
    error_message = "Set CUCINA_EXPIRES (source .work/env.sh) or pass -var cucina_expires=..."
  }
}

variable "artifacts_dir" {
  type        = string
  default     = env("CUCINA_IMAGES_ARTIFACTS")
  description = "Local directory for build artifacts (manifests, version inventories); outside the repo."
  validation {
    condition     = length(var.artifacts_dir) > 0
    error_message = "Set CUCINA_IMAGES_ARTIFACTS (the Makefile does) or pass -var artifacts_dir=..."
  }
}
