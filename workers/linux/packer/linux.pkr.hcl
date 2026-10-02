# SPDX-License-Identifier: FSL-1.1-ALv2

variable "region" {
  type    = string
  default = "us-west-1"
}

variable "source_ami" {
  type        = string
  description = "Exact, provider-scoped source AMI from the private source-amis.json lock; update deliberately for OS changes."
  validation {
    condition     = can(regex("^ami-[0-9a-f]{8,17}$", var.source_ami))
    error_message = "The source_ami variable must be an exact AMI ID, not a latest-matching filter."
  }
}

variable "arch" {
  type = string
  validation {
    condition     = contains(["x86_64", "arm64"], var.arch)
    error_message = "The arch variable must be x86_64 or arm64."
  }
}

variable "variant" {
  type    = string
  default = "ubuntu"
  validation {
    condition     = contains(["ubuntu", "al2023"], var.variant)
    error_message = "The variant variable must be ubuntu or al2023."
  }
}

variable "instance_type" {
  type        = string
  default     = ""
  description = "Builder type; empty = 2-vCPU m7i.large (x86_64) / m7g.large (arm64)."
}

variable "root_volume_size" {
  type        = number
  default     = 8
  description = "GiB; the smallest size the source snapshot allows (both Ubuntu and AL2023 ship 8 GiB) - smaller is faster (NFR-C4)."
}

variable "vpc_id" {
  type = string
}

variable "subnet_id" {
  type        = string
  description = "Public subnet: the builder needs a public IPv4 for SSH from the admin /32 and for downloads."
}

variable "security_group_ids" {
  type    = list(string)
  default = []
}

variable "admin_cidrs" {
  type = list(string)
  validation {
    condition     = length(var.admin_cidrs) > 0 && alltrue([for c in var.admin_cidrs : c != "0.0.0.0/0" && c != "::/0"])
    error_message = "The admin_cidrs variable must list specific CIDRs (no 0.0.0.0/0 or ::/0)."
  }
}

variable "image_version" {
  type = string
}

variable "generation" {
  type = string
}

variable "smoke_dir" {
  type        = string
  default     = ""
  description = "Directory with hello-<arch>-{static,dynamic} binaries for the qemu smoke test (x86_64 Ubuntu); empty = skip."
}

variable "worker_agent_path" {
  type        = string
  default     = ""
  description = "Optional local cucina-worker-agent binary for this architecture."
}

variable "amis_file" {
  type    = string
  default = ""
}

variable "artifacts_dir" {
  type    = string
  default = env("CUCINA_IMAGES_ARTIFACTS")
  validation {
    condition     = length(var.artifacts_dir) > 0
    error_message = "Set CUCINA_IMAGES_ARTIFACTS (the Makefile does) or pass -var artifacts_dir=..."
  }
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

locals {
  pins          = jsondecode(file("${path.root}/../versions.json")).pins
  distro        = local.pins[var.variant]
  family        = "linux-${var.variant}-${var.arch}"
  instance_type = var.instance_type != "" ? var.instance_type : (var.arch == "x86_64" ? "m7i.large" : "m7g.large")
  qemu          = var.arch == "x86_64" && var.variant == "ubuntu"
  with_agent    = var.worker_agent_path != "" && fileexists(var.worker_agent_path)
  run_smoke     = local.qemu && var.smoke_dir != ""
  campaign_tags = {
    "cucina:env"     = var.cucina_env
    "cucina:run"     = var.cucina_run
    "cucina:expires" = var.cucina_expires
  }
  image_tags = merge(local.campaign_tags, {
    "Name"                 = "cucina-${local.family}-${var.image_version}"
    "cucina:image-family"  = local.family
    "cucina:image-version" = var.image_version
    "cucina:generation"    = var.generation
    "cucina:os"            = "linux"
    "cucina:arch"          = var.arch
    "cucina:variant"       = var.variant
    "cucina:role"          = "worker-image"
    "cucina:bb-version"    = local.pins.buildbarn.bb_remote_execution
    "cucina:worker-agent"  = local.with_agent ? "yes" : "no"
    "cucina:source-ami"    = "{{ .SourceAMI }}"
    "cucina:source-name"   = "{{ .SourceAMIName }}"
  })
  build_tags = merge(local.campaign_tags, {
    "Name"                = "cucina-packer-${local.family}"
    "cucina:role"         = "image-builder"
    "cucina:image-family" = local.family
  })
}

source "amazon-ebs" "linux" {
  region                                = var.region
  source_ami                            = var.source_ami
  instance_type                         = local.instance_type
  vpc_id                                = var.vpc_id
  subnet_id                             = var.subnet_id
  associate_public_ip_address           = true
  security_group_ids                    = var.security_group_ids
  temporary_security_group_source_cidrs = var.admin_cidrs
  ssh_username                          = local.distro.ssh_username
  ssh_interface                         = "public_ip"
  ssh_timeout                           = "15m"
  ena_support                           = true

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }
  imds_support = "v2.0"

  launch_block_device_mappings {
    device_name           = local.distro.root_device
    volume_size           = var.root_volume_size
    volume_type           = "gp3"
    delete_on_termination = true
  }

  ami_name        = "cucina-${local.family}-${var.image_version}"
  ami_description = "Cucina Linux worker (${local.distro.release}, ${var.arch}, Buildbarn ${local.pins.buildbarn.bb_remote_execution})"
  aws_polling {
    delay_seconds = 10
    max_attempts  = 180
  }

  tags            = local.image_tags
  snapshot_tags   = local.image_tags
  run_tags        = local.build_tags
  run_volume_tags = local.build_tags
}

build {
  name    = "linux"
  sources = ["source.amazon-ebs.linux"]

  provisioner "shell" {
    inline = ["mkdir -p /var/tmp/cucina/agent /var/tmp/cucina/smoke /var/tmp/cucina/notices/licenses"]
  }

  provisioner "file" {
    source      = "${path.root}/../files"
    destination = "/var/tmp/cucina"
  }

  provisioner "file" {
    source      = "${path.root}/../versions.json"
    destination = "/var/tmp/cucina/versions.json"
  }

  # R-ARTIFACT: payload inputs, not links to documentation outside the image.
  provisioner "file" {
    sources     = ["${path.root}/../../../LICENSE.md", "${path.root}/../../../THIRD_PARTY_NOTICES.md"]
    destination = "/var/tmp/cucina/notices/"
  }

  provisioner "file" {
    source      = "${path.root}/../../../tools/notices/texts/"
    destination = "/var/tmp/cucina/notices/licenses/"
  }

  provisioner "file" {
    source      = "${path.root}/../scripts/install-notices.sh"
    destination = "/var/tmp/cucina/install-notices.sh"
  }

  provisioner "file" {
    sources     = local.with_agent ? [var.worker_agent_path] : ["${path.root}/../versions.json"]
    destination = "/var/tmp/cucina/agent/"
  }

  provisioner "file" {
    sources     = local.run_smoke ? [for a in ["riscv64", "s390x", "armhf"] : "${var.smoke_dir}/hello-${a}-static"] : ["${path.root}/../versions.json"]
    destination = "/var/tmp/cucina/smoke/"
  }

  provisioner "file" {
    sources     = local.run_smoke ? [for a in ["riscv64", "s390x", "armhf"] : "${var.smoke_dir}/hello-${a}-dynamic"] : ["${path.root}/../versions.json"]
    destination = "/var/tmp/cucina/smoke/"
  }

  provisioner "shell" {
    execute_command  = "chmod +x {{ .Path }}; sudo -E env {{ .Vars }} bash {{ .Path }}"
    environment_vars = ["ARCH=${var.arch}", "VARIANT=${var.variant}", "IMAGE_VERSION=${var.image_version}", "GENERATION=${var.generation}"]
    script           = "${path.root}/../scripts/provision.sh"
    timeout          = "45m"
  }

  # Reboot once so the smoke test and the unit checks run against the image's own boot (masked units,
  # binfmt registrations restored by systemd at boot, fstab/grub changes). Staging lives in /var/tmp because
  # /tmp is a tmpfs on Ubuntu 26.04.
  # During the build the agent must never power the builder off (it has no Cucina boot data); cleanup.sh removes
  # this drop-in before the image is captured.
  provisioner "shell" {
    execute_command = "sudo -E bash {{ .Path }}"
    inline = [
      "for u in bb-runner cucina-worker-agent; do mkdir -p /etc/systemd/system/$u.service.d; printf '[Service]\\nEnvironment=CUCINA_AGENT_NO_POWEROFF=1\\n' > /etc/systemd/system/$u.service.d/99-image-build.conf; done",
    ]
  }

  provisioner "shell" {
    execute_command   = "sudo -E bash {{ .Path }}"
    inline            = ["systemctl reboot"]
    expect_disconnect = true
  }

  provisioner "shell" {
    pause_before    = "20s"
    execute_command = "sudo -E env {{ .Vars }} bash {{ .Path }}"
    environment_vars = [
      "RUN_SMOKE=${local.run_smoke}",
    ]
    inline = [
      "set -eu",
      "test -s /usr/share/doc/cucina/LICENSE.md && test -s /usr/share/doc/cucina/THIRD_PARTY_NOTICES.md",
      "for f in /var/tmp/cucina/notices/licenses/*.txt; do cmp \"$f\" \"/usr/share/doc/cucina/licenses/$(basename \"$f\")\"; done",
      "systemctl is-system-running --wait || true",
      "systemctl --failed --no-legend",
      "systemctl is-active bb-runner.service bb-worker.service cucina-worker-agent.service || true",
      "journalctl -b -u bb-runner.service -u cucina-worker-agent.service -o cat --no-pager | tail -n 25 || true",
      "systemd-analyze || true",
      "systemd-analyze blame | head -n 15",
      "systemctl is-enabled bb-runner.service bb-worker.service cucina-format-instance-store.service cucina-deadman.timer amazon-ssm-agent.service",
      "/opt/cucina/bin/cucina-deadman --dry-run",
      "if [ \"$RUN_SMOKE\" = true ]; then /opt/cucina/bin/cucina-qemu-smoke-test /var/tmp/cucina/smoke; fi",
      "if [ -x /opt/cucina/bin/cucina-worker-agent ]; then rc=0; /opt/cucina/bin/cucina-worker-agent selftest --out /etc/cucina/selftest.json || rc=$?; jq -c '{ok, checks: [.checks[] | {name, ok}]}' /etc/cucina/selftest.json; [ $rc -eq 0 ] || exit 1; fi",
      "if [ -x /opt/cucina/bin/cucina-worker-agent ]; then test -e /run/cucina/not-a-worker && echo 'bootstrap: not a worker (no user data) as expected'; fi",
      "chronyc -n sources 2>/dev/null | tail -n 4 || true",
    ]
  }

  provisioner "file" {
    direction   = "download"
    source      = "/etc/cucina/image.json"
    destination = "${var.artifacts_dir}/${local.family}-${var.image_version}.json"
  }

  provisioner "shell" {
    execute_command  = "sudo -E env {{ .Vars }} bash {{ .Path }}"
    environment_vars = ["VARIANT=${var.variant}"]
    script           = "${path.root}/../scripts/cleanup.sh"
  }

  post-processor "manifest" {
    output     = "${var.artifacts_dir}/${local.family}-manifest.json"
    strip_path = true
    custom_data = {
      family        = local.family
      image_version = var.image_version
      generation    = var.generation
    }
  }

  post-processor "shell-local" {
    inline = [
      "'${path.root}/../scripts/record-ami.sh' ${local.family} '${var.artifacts_dir}/${local.family}-manifest.json' '${var.amis_file}'",
      "'${path.root}/../scripts/update-versions.sh' ${local.family} '${var.artifacts_dir}/${local.family}-${var.image_version}.json'",
    ]
  }
}
