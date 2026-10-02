# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Stage `worker`: base AMI + bb_worker/bb_runner services (shawl), boot orchestration + bootstrap hook, dead-man
# switch, low-privilege runner account; generalised with EC2Launch v2 `sysprep --shutdown` (R-POOL-5) so it is
# eligible for EC2 Fast Launch (enabled by the post-processor when enable_fast_launch = true).

locals {
  worker_tags = merge(local.campaign_tags, {
    "Name"                 = "cucina-windows-worker-${var.image_version}"
    "cucina:image-family"  = "windows-worker"
    "cucina:image-version" = var.image_version
    "cucina:generation"    = var.generation
    "cucina:os"            = "windows"
    "cucina:arch"          = "x86_64"
    "cucina:stage"         = "worker"
    "cucina:role"          = "worker-image"
    "cucina:bb-version"    = local.pins.buildbarn.bb_remote_execution
    "cucina:worker-agent"  = length(local.agent_sources) > 0 ? "yes" : "no"
    "cucina:source-ami"    = "{{ .SourceAMI }}"
    "cucina:source-name"   = "{{ .SourceAMIName }}"
  })
  worker_build_tags = merge(local.campaign_tags, {
    "Name"                = "cucina-packer-windows-worker"
    "cucina:role"         = "image-builder"
    "cucina:image-family" = "windows-worker"
  })
  # worker_agent_path: a directory of cucina-worker-agent.zip.part-* files (the Makefile splits the zip; Packer's
  # WinRM upload fails on large files), or a single .exe/.zip file.
  agent_parts   = var.worker_agent_path != "" ? fileset(var.worker_agent_path, "cucina-worker-agent.zip.part-*") : []
  agent_sources = length(local.agent_parts) > 0 ? [for f in sort(local.agent_parts) : "${var.worker_agent_path}/${f}"] : ((var.worker_agent_path != "" && fileexists(var.worker_agent_path)) ? [var.worker_agent_path] : [])
}

source "amazon-ebs" "worker" {
  region     = var.region
  source_ami = var.base_ami
  dynamic "source_ami_filter" {
    for_each = var.base_ami == "" ? [1] : []
    content {
      filters = {
        "tag:cucina:image-family" = "windows-base"
        "tag:cucina:env"          = var.cucina_env
        "state"                   = "available"
      }
      owners      = ["self"]
      most_recent = true
    }
  }
  instance_type                         = var.worker_instance_type
  vpc_id                                = var.vpc_id
  subnet_id                             = var.subnet_id
  associate_public_ip_address           = true
  security_group_ids                    = var.security_group_ids
  temporary_security_group_source_cidrs = var.admin_cidrs

  communicator             = "winrm"
  winrm_username           = "Administrator"
  winrm_use_ssl            = true
  winrm_insecure           = true
  winrm_port               = 5986
  winrm_timeout            = "40m"
  windows_password_timeout = "30m"
  user_data_file           = "${path.root}/winrm-bootstrap.ps1"

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }
  imds_support = "v2.0"

  launch_block_device_mappings {
    device_name           = "/dev/sda1"
    volume_size           = var.worker_root_volume_size
    volume_type           = "gp3"
    delete_on_termination = true
  }

  ami_name              = "cucina-windows-worker-${var.image_version}"
  ami_description       = "Cucina Windows worker (WS2025, VS ${local.pins.vs.product_display_version} Build Tools, Buildbarn ${local.pins.buildbarn.bb_remote_execution}); sysprepped"
  disable_stop_instance = true
  aws_polling {
    delay_seconds = 15
    max_attempts  = 320
  }

  tags            = local.worker_tags
  snapshot_tags   = local.worker_tags
  run_tags        = local.worker_build_tags
  run_volume_tags = local.worker_build_tags
}

build {
  name    = "worker"
  sources = ["source.amazon-ebs.worker"]

  provisioner "powershell" {
    inline = ["New-Item -ItemType Directory -Force -Path ${local.stage_dir}/scripts, ${local.stage_dir}/bin, ${local.stage_dir}/notices/licenses | Out-Null"]
  }

  provisioner "file" {
    sources = [
      "${local.script_dir}/install-worker.ps1",
      "${local.script_dir}/install-notices.ps1",
      "${local.script_dir}/verify-worker.ps1",
      "${local.script_dir}/finalize.ps1",
    ]
    destination = "${local.stage_dir}/scripts/"
  }

  provisioner "file" {
    sources = concat([
      "${path.root}/../files/bin/cucina-boot.ps1",
      "${path.root}/../files/bin/cucina-bootstrap.ps1",
      "${path.root}/../files/bin/cucina-deadman.ps1",
      "${path.root}/../files/bin/cucina-format-data-volume.ps1",
    ], local.agent_sources)
    destination = "${local.stage_dir}/bin/"
  }

  provisioner "file" {
    source      = "${path.root}/../versions.json"
    destination = "${local.stage_dir}/versions.json"
  }

  # Refresh the payload even when the source is an older base AMI without notices.
  provisioner "file" {
    sources     = ["${path.root}/../../../LICENSE.md", "${path.root}/../../../THIRD_PARTY_NOTICES.md"]
    destination = "${local.stage_dir}/notices/"
  }

  provisioner "file" {
    source      = "${path.root}/../../../tools/notices/texts/"
    destination = "${local.stage_dir}/notices/licenses/"
  }

  provisioner "powershell" {
    inline = ["& ${local.stage_dir}/scripts/install-notices.ps1 -Source ${local.stage_dir}/notices"]
  }

  provisioner "powershell" {
    timeout = "30m"
    inline = [
      "& ${local.stage_dir}/scripts/install-worker.ps1 -PinsFile ${local.stage_dir}/versions.json -ImageVersion ${var.image_version} -Generation ${var.generation} -DefenderMode ${var.defender_mode}",
    ]
  }

  provisioner "powershell" {
    timeout = "15m"
    inline  = ["& ${local.stage_dir}/scripts/verify-worker.ps1"]
  }

  # "Before sysprep-ing anything, verify rules_cc/Bazel MSVC autodetection": optionally block on the result file
  # written by scripts/verify-bazel.sh (first line "pass").
  provisioner "shell-local" {
    inline = [
      "gate='${var.sysprep_gate_file}'; [ -z \"$gate\" ] && exit 0; echo \"waiting for $gate\"; for i in $(seq 1 180); do [ -s \"$gate\" ] && break; sleep 20; done; head -1 \"$gate\" | grep -q '^pass' || { echo \"sysprep gate: $(head -1 \"$gate\" 2>/dev/null)\"; exit 1; }",
    ]
  }

  provisioner "powershell" {
    inline = ["& ${local.stage_dir}/scripts/finalize.ps1 -Mode sysprep"]
  }

  post-processor "manifest" {
    output     = "${var.artifacts_dir}/windows-worker-manifest.json"
    strip_path = true
    custom_data = {
      family        = "windows-worker"
      image_version = var.image_version
      generation    = var.generation
    }
  }

  post-processor "shell-local" {
    inline = [
      "'${path.root}/../../linux/scripts/record-ami.sh' windows-worker '${var.artifacts_dir}/windows-worker-manifest.json' '${var.amis_file}'",
      "if [ '${var.enable_fast_launch}' = true ]; then ami=$(jq -r '.last_run_uuid as $u | [.builds[] | select(.packer_run_uuid == $u)] | last | .artifact_id | split(\":\")[1]' '${var.artifacts_dir}/windows-worker-manifest.json'); '${path.root}/../scripts/fast-launch.sh' enable --ami \"$ami\" --count ${var.fast_launch_target_count} --launch-template-id '${var.fast_launch_template_id}'; fi",
    ]
  }
}
