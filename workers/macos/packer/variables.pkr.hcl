# SPDX-License-Identifier: FSL-1.1-ALv2

variable "macos_release" {
  type        = string
  default     = "golden-gate"
  description = "Cirrus Labs macOS release name of the base image (golden-gate = macOS 27, tahoe = 26). Hosts must run a macOS >= the guest's."
  validation {
    condition     = can(regex("^[a-z][a-z-]*$", var.macos_release))
    error_message = "The macos_release must be a Cirrus Labs release name such as golden-gate or tahoe."
  }
}

variable "xcode_version" {
  type        = string
  default     = "27.0"
  description = "Xcode version the image carries; also the pool's `xcode-version` platform property. Must have an entry in ../versions.json (build, base image)."
  validation {
    condition     = can(regex("^[0-9]+\\.[0-9]+(\\.[0-9]+)?$", var.xcode_version))
    error_message = "The xcode_version must look like 27.0 or 27.0.1."
  }
}

variable "cucina_version" {
  type        = string
  default     = "0.1.0-dev"
  description = "Cucina release (SemVer, R-OPS-7). With xcode_version it forms the image tag <xcode>-<cucina_version> = the pool generation's image version."
  validation {
    condition     = can(regex("^[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?$", var.cucina_version))
    error_message = "The cucina_version must be SemVer without build metadata (OCI tags cannot contain '+')."
  }
}

variable "base_image" {
  type        = string
  default     = ""
  description = "Override of the base Tart image reference; empty = versions.json (digest-pinned when a digest is recorded)."
}

variable "xcode_app_source" {
  type        = string
  default     = ""
  description = "Absolute host path of an Xcode.app (symlinks resolved) shared read-only into the build VM. Used only when the base image's Xcode build differs from versions.json (see docs/adr/0352-*)."
}

variable "buildbarn_dir" {
  type        = string
  description = "Directory holding bb_worker.darwin_arm64 and bb_runner.darwin_arm64, fetched and SHA-256 verified by ../scripts/fetch-buildbarn.sh."
}

variable "worker_agent_path" {
  type        = string
  default     = ""
  description = "darwin/arm64 cucina-worker-agent binary (go build ./cmd/cucina-worker-agent). Empty = image without the in-VM render call site."
}

variable "worker_agent_sha256" {
  type        = string
  default     = ""
  description = "Expected SHA-256 of worker_agent_path (re-verified inside the guest)."
}

variable "cpu_count" {
  type        = number
  default     = 8
  description = "vCPUs of the build VM (hostd sizes the worker VMs it clones from the image)."
}

variable "memory_gb" {
  type    = number
  default = 16
}

variable "disk_size_gb" {
  type        = number
  default     = 250
  description = "Logical disk size (sparse on the host). Holds the base (~125 GB used), the 40 GiB persistent L1, the native input cache (16 GiB), the file pool and build directories with headroom."
}

variable "ssh_username" {
  type        = string
  default     = "admin"
  description = "Cirrus Labs build-time account. Its password is rotated to an unknown random value at the end of the build."
}

variable "ssh_password" {
  type        = string
  default     = "admin"
  sensitive   = true
  description = "Cirrus Labs' published default password, valid only during the build (the VM is reachable from this host only)."
}
