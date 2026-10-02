# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Cucina macOS worker image (R-MAC-3/4/7, R-CACHE-2/-3, R-XPLAT-3/-8, R-POOL-8). In-VM contract: docs/dev/hostd.md §1.
# Operations guide: docs/operations/macos-images.md. Decisions: docs/adr/0350..0353.

locals {
  root     = abspath("${path.root}/..") # workers/macos, absolute whatever the working directory
  versions = jsondecode(file("${local.root}/versions.json"))
  xcode    = local.versions.xcode[var.xcode_version]
  layout   = local.versions.layout
  bb       = local.versions.buildbarn

  # Digest-pinned base when versions.json records the digest of the pulled tag (R-0.5 "pin everything").
  base_pinned = local.xcode.baseImageDigest != "" ? "${regex_replace(local.xcode.baseImage, ":[^:/]+$", "")}@${local.xcode.baseImageDigest}" : local.xcode.baseImage
  base_image  = var.base_image != "" ? var.base_image : local.base_pinned

  image_version = "${var.xcode_version}-${var.cucina_version}"
  vm_name       = var.vm_name_override != "" ? var.vm_name_override : "cucina-worker-macos:${local.image_version}"

  stage       = "/private/var/tmp/cucina-stage" # survives the provisioning reboot; removed by finalize
  xcode_share = "cucina-xcode"
  # Provisioning scripts run as root; the Cirrus build account has passwordless sudo.
  sudo_exec = "chmod +x {{ .Path }}; sudo -n env {{ .Vars }} {{ .Path }}"
  env = [
    "CUCINA_STAGE=${local.stage}",
    "BUILD_USER=${local.layout.builderUser}",
    "BUILD_UID=${local.layout.builderUid}",
    "BUILD_ADMIN=${var.ssh_username}",
    "WORKER_USER=${local.layout.workerUser}",
    "DATA_VOLUME=${local.layout.dataVolume}",
    "IMAGE_VERSION=${local.image_version}",
    "CUCINA_VERSION=${var.cucina_version}",
    "XCODE_VERSION=${var.xcode_version}",
    "XCODE_BUILD=${local.xcode.build}",
    "XCODE_SRC=${var.xcode_app_source != "" ? "/Volumes/My Shared Files/${local.xcode_share}" : ""}",
    "BB_RELEASE=${local.bb.bbRemoteExecution}",
    "BB_WORKER_SHA256=${local.bb.assets.bb_worker.sha256}",
    "BB_RUNNER_SHA256=${local.bb.assets.bb_runner.sha256}",
    "WORKER_AGENT_SHA256=${var.worker_agent_sha256}",
    "BASE_IMAGE=${local.base_image}",
  ]
}

source "tart-cli" "worker" {
  vm_base_name       = local.base_image
  vm_name            = local.vm_name
  cpu_count          = var.cpu_count
  memory_gb          = var.memory_gb
  disk_size_gb       = var.disk_size_gb
  recovery_partition = "delete" # disk can grow (tart set --disk-size + guest agent resize); workers are re-imaged, never updated in place
  headless           = true
  disable_vnc        = true
  # The Xcode share (read-only, build time only) is the one sanctioned use of --dir: never for build dirs or CAS.
  run_extra_args = concat(
    ["--root-disk-opts=caching=cached,sync=none"],
    var.xcode_app_source != "" ? ["--dir=${local.xcode_share}:${var.xcode_app_source}:ro"] : [],
  )
  ssh_username = var.ssh_username
  ssh_password = var.ssh_password
  ssh_timeout  = "300s"
}

build {
  name    = "cucina-worker-macos"
  sources = ["source.tart-cli.worker"]

  provisioner "shell" {
    inline = ["mkdir -p ${local.stage}/bin ${local.stage}/files ${local.stage}/facts"]
  }

  provisioner "file" {
    source      = "${local.root}/files/"
    destination = "${local.stage}/files"
  }

  provisioner "file" {
    source      = "${local.root}/scripts/smoke.sh"
    destination = "${local.stage}/files/cucina-smoke"
  }

  provisioner "file" {
    sources = [
      "${local.root}/../../LICENSE.md",
      "${local.root}/../../THIRD_PARTY_NOTICES.md",
      "${local.root}/../../macos/pkg/payload/cucina-kcpassword",
    ]
    destination = "${local.stage}/files/"
  }

  provisioner "file" {
    sources = [
      "${var.buildbarn_dir}/${local.bb.assets.bb_worker.name}",
      "${var.buildbarn_dir}/${local.bb.assets.bb_runner.name}",
    ]
    destination = "${local.stage}/bin/"
  }

  dynamic "provisioner" {
    for_each = var.worker_agent_path != "" ? [var.worker_agent_path] : []
    labels   = ["file"]
    content {
      source      = provisioner.value
      destination = "${local.stage}/bin/cucina-worker-agent"
    }
  }

  provisioner "shell" {
    execute_command  = local.sudo_exec
    environment_vars = local.env
    scripts = [
      "${local.root}/provision/00-inspect.sh",
      "${local.root}/provision/10-xcode.sh",
      "${local.root}/provision/20-system.sh",
      "${local.root}/provision/30-build-user.sh",
      "${local.root}/provision/40-data-volume.sh",
      "${local.root}/provision/50-cucina.sh",
    ]
  }

  # One reboot inside the build: the build user's first automatic login (home, keychain, session setup) and the
  # guest agent's new daemon definition happen here, so a VM's first start from the image is an ordinary boot.
  provisioner "shell" {
    execute_command   = local.sudo_exec
    inline            = ["shutdown -r now"]
    expect_disconnect = true
    skip_clean        = true
  }

  provisioner "shell" {
    pause_before     = "15s"
    execute_command  = local.sudo_exec
    environment_vars = local.env
    scripts = [
      "${local.root}/provision/60-finalize.sh",
      "${local.root}/provision/70-verify.sh",
    ]
  }
}
