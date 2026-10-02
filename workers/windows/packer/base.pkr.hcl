# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Stage `base`: Windows Server 2025 + VS 2026 Build Tools (MSVC 14.5x) + Windows 11 SDK + VC++ redist + WinFSP +
# shawl + Git + Bazelisk + system tuning. Deliberately NOT sysprepped: the AMI is the Packer source of the
# `worker` stage and the `windows-client` AMI. EC2Launch state is reset instead, so every instance launched from
# it gets a fresh random Administrator password (GetPasswordData) and runs its user data.

locals {
  pins       = jsondecode(file("${path.root}/../versions.json")).pins
  stage_dir  = "C:/Windows/Temp/cucina"
  script_dir = "${path.root}/../scripts"
  campaign_tags = {
    "cucina:env"     = var.cucina_env
    "cucina:run"     = var.cucina_run
    "cucina:expires" = var.cucina_expires
  }
  base_image_tags = merge(local.campaign_tags, {
    "Name"                 = "cucina-windows-base-${var.image_version}"
    "cucina:image-family"  = "windows-base"
    "cucina:image-version" = var.image_version
    "cucina:generation"    = var.generation
    "cucina:os"            = "windows"
    "cucina:arch"          = "x86_64"
    "cucina:stage"         = "base"
    "cucina:vs"            = local.pins.vs.product_display_version
    "cucina:source-ami"    = "{{ .SourceAMI }}"
    "cucina:source-name"   = "{{ .SourceAMIName }}"
  })
  build_tags = merge(local.campaign_tags, {
    "Name"                = "cucina-packer-windows-base"
    "cucina:role"         = "image-builder"
    "cucina:image-family" = "windows-base"
  })
}

source "amazon-ebs" "base" {
  region = var.region
  source_ami_filter {
    filters = {
      name                = local.pins.windows_source_ami_name
      architecture        = "x86_64"
      root-device-type    = "ebs"
      virtualization-type = "hvm"
    }
    owners      = ["amazon"]
    most_recent = true
  }
  instance_type                         = var.base_instance_type
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
    volume_size           = var.root_volume_size
    volume_type           = "gp3"
    delete_on_termination = true
  }

  ami_name              = "cucina-windows-base-${var.image_version}"
  ami_description       = "Cucina Windows base (WS2025 + VS ${local.pins.vs.product_display_version} Build Tools); not generalised"
  disable_stop_instance = true
  aws_polling {
    delay_seconds = 15
    max_attempts  = 320
  }

  tags            = local.base_image_tags
  snapshot_tags   = local.base_image_tags
  run_tags        = local.build_tags
  run_volume_tags = local.build_tags
}

build {
  name    = "base"
  sources = ["source.amazon-ebs.base"]

  provisioner "powershell" {
    inline = ["New-Item -ItemType Directory -Force -Path ${local.stage_dir}/scripts, ${local.stage_dir}/notices/licenses | Out-Null"]
  }

  provisioner "file" {
    sources = [
      "${local.script_dir}/configure-system.ps1",
      "${local.script_dir}/install-vs.ps1",
      "${local.script_dir}/install-base.ps1",
      "${local.script_dir}/install-notices.ps1",
      "${local.script_dir}/verify-base.ps1",
      "${local.script_dir}/finalize.ps1",
    ]
    destination = "${local.stage_dir}/scripts/"
  }

  provisioner "file" {
    source      = "${path.root}/../versions.json"
    destination = "${local.stage_dir}/versions.json"
  }

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

  # Diagnostics for the build log (OS build, EC2Launch CLI surface, free space).
  provisioner "powershell" {
    inline = [
      "$nt = Get-ItemProperty 'HKLM:/SOFTWARE/Microsoft/Windows NT/CurrentVersion'; Write-Output \"os: $($nt.ProductName) $($nt.DisplayVersion) build $($nt.CurrentBuild).$($nt.UBR)\"",
      "$e = Join-Path $env:ProgramFiles 'Amazon/EC2Launch/EC2Launch.exe'; Write-Output \"ec2launch: $(& $e version)\"; & $e sysprep --help; & $e reset --help",
      "Write-Output \"ssm: $(& (Join-Path $env:ProgramFiles 'Amazon/SSM/amazon-ssm-agent.exe') -version)\"",
      "Get-PSDrive C | Select-Object Used, Free | Format-List",
      "exit 0",
    ]
  }

  provisioner "powershell" {
    inline = ["& ${local.stage_dir}/scripts/configure-system.ps1 -DefenderMode ${var.defender_mode}"]
  }

  # VS Build Tools runs as SYSTEM through a scheduled task (outside WinRM's shell quotas and job object).
  provisioner "powershell" {
    elevated_user     = "SYSTEM"
    elevated_password = ""
    timeout           = "150m"
    inline            = ["& ${local.stage_dir}/scripts/install-vs.ps1 -PinsFile ${local.stage_dir}/versions.json -VersionsOut C:/ProgramData/cucina/image/toolchain.json"]
  }

  provisioner "windows-restart" {
    restart_timeout = "30m"
  }

  provisioner "powershell" {
    timeout = "60m"
    inline  = ["& ${local.stage_dir}/scripts/install-base.ps1 -PinsFile ${local.stage_dir}/versions.json"]
  }

  provisioner "windows-restart" {
    restart_timeout = "30m"
  }

  provisioner "powershell" {
    timeout = "60m"
    inline  = ["& ${local.stage_dir}/scripts/verify-base.ps1 -ImageVersion ${var.image_version}"]
  }

  provisioner "powershell" {
    inline = ["& ${local.stage_dir}/scripts/finalize.ps1 -Mode reset"]
  }

  post-processor "manifest" {
    output     = "${var.artifacts_dir}/windows-base-manifest.json"
    strip_path = true
    custom_data = {
      family        = "windows-base"
      image_version = var.image_version
      generation    = var.generation
    }
  }

  post-processor "shell-local" {
    inline = [
      "'${path.root}/../../linux/scripts/record-ami.sh' windows-base '${var.artifacts_dir}/windows-base-manifest.json' '${var.amis_file}'",
    ]
  }
}
