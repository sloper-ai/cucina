# SPDX-License-Identifier: FSL-1.1-ALv2

# Compute layer of the temporary acceptance environment (PROMPT.md §10.1): the k3s node (the
# ONLY Elastic IP in the system), linux-client and windows-client. Network, security groups,
# IAM and ECR come from the base layer (terraform_remote_state, local backend). Worker pools
# are NOT created here: cucina-controller launches them from chart values.

data "terraform_remote_state" "base" {
  backend = "local"

  config = {
    path = "${var.state_dir}/base/terraform.tfstate"
  }
}

# Ubuntu 26.04 LTS (amd64), resolved from Canonical's public SSM parameter unless pinned.
data "aws_ssm_parameter" "ubuntu" {
  name = var.ubuntu_ami_ssm_parameter
}

locals {
  name = "cucina-e2e"

  # The three mandatory tags (§12): applied through the provider's default_tags and, explicitly,
  # on every resource (what the offline `tofu test` suite asserts).
  tags = {
    "cucina:env"     = "e2e"
    "cucina:run"     = var.run_id
    "cucina:expires" = var.expires
  }

  base = data.terraform_remote_state.base.outputs

  # The control plane's fixed private address: workers and in-VPC clients use it (never the Elastic IP).
  k3s_private_ip = cidrhost(local.base.public_subnet_cidr, var.k3s_private_ip_host)

  ubuntu_ami = data.aws_ssm_parameter.ubuntu.insecure_value
  k3s_ami    = var.k3s_ami != "" ? var.k3s_ami : local.ubuntu_ami
  linux_ami  = var.linux_client_ami != "" ? var.linux_client_ami : local.ubuntu_ami

  # Infrastructure nodes are protected from the controller's IAM policy (cucina:protected=true).
  infra_tags = { "cucina:protected" = "true" }

  k3s_user_data = templatefile("${path.module}/templates/k3s-node.sh.tftpl", {
    guard_minutes      = var.guard_minutes
    data_volume_gib    = var.k3s_data_volume_gib
    k3s_version        = var.k3s_version
    k3s_version_url    = urlencode(var.k3s_version)
    k3s_sha256         = var.k3s_sha256_amd64
    k3s_install_sha256 = var.k3s_install_sha256
    public_ip          = aws_eip.k3s.public_ip

    cluster_cidr = var.k3s_cluster_cidr
    service_cidr = var.k3s_service_cidr
    cluster_dns  = cidrhost(var.k3s_service_cidr, 10)

    ecr_credential_provider = var.k3s_ecr_credential_provider
    ecr_provider_b64        = base64encode(file("${path.module}/files/ecr-credential-provider.sh"))
  })

  linux_client_user_data = templatefile("${path.module}/templates/linux-client.sh.tftpl", {
    guard_minutes    = var.guard_minutes
    bazelisk_version = var.bazelisk_version
    bazelisk_sha256  = var.bazelisk_linux_amd64_sha256
  })

  windows_client_user_data = templatefile("${path.module}/templates/windows-client.ps1.tftpl", {
    guard_minutes    = var.guard_minutes
    bazelisk_version = var.bazelisk_version
    bazelisk_sha256  = var.bazelisk_windows_amd64_sha256
    git_version      = var.git_for_windows_version
    git_installer    = var.git_for_windows_installer
    git_sha256       = var.git_for_windows_sha256
  })
}

# --- k3s node ---------------------------------------------------------------------------

# The one and only Elastic IP: the client / Mac endpoint and the kubeconfig address. Allocated
# before the instance so it can be baked into --tls-san / --node-external-ip.
resource "aws_eip" "k3s" {
  domain = "vpc"

  tags = merge(local.tags, { Name = "${local.name}-k3s", "cucina:e2e-role" = "k3s" })
}

# The node's primary network interface exists, and carries the Elastic IP, before the instance
# does. The node therefore boots with its final public address: an EIP associated after launch
# replaces the auto-assigned public IPv4 and breaks the connections opened meanwhile (the SSM
# agent's, cloud-init's); in the pre-flight, Run Command stayed delayed for about six minutes
# that way and never did with this layout.
resource "aws_network_interface" "k3s" {
  subnet_id          = local.base.public_subnet_id
  security_groups    = [local.base.sg_control_plane]
  private_ips        = [local.k3s_private_ip]
  ipv6_address_count = 1
  description        = "${local.name} k3s node primary interface"

  tags = merge(local.tags, local.infra_tags, { Name = "${local.name}-k3s", "cucina:e2e-role" = "k3s" })
}

resource "aws_eip_association" "k3s" {
  allocation_id        = aws_eip.k3s.id
  network_interface_id = aws_network_interface.k3s.id
}

resource "aws_instance" "k3s" {
  ami                                  = local.k3s_ami
  instance_type                        = var.k3s_instance_type
  iam_instance_profile                 = local.base.k3s_node_instance_profile_name
  instance_initiated_shutdown_behavior = "stop"
  monitoring                           = false
  user_data                            = local.k3s_user_data
  user_data_replace_on_change          = true

  primary_network_interface {
    network_interface_id = aws_network_interface.k3s.id
  }

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required" # IMDSv2 only
    http_put_response_hop_limit = 2          # pods reach the node's instance profile (R-CP-8; temporary cluster only)
    instance_metadata_tags      = "enabled"
  }

  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.k3s_root_volume_gib
    encrypted             = true
    delete_on_termination = true
  }

  # Buildbarn storage (PVCs on the local-path provisioner): mounted at /var/lib/rancher/k3s/storage.
  ebs_block_device {
    device_name           = "/dev/sdf"
    volume_type           = "gp3"
    volume_size           = var.k3s_data_volume_gib
    iops                  = var.k3s_data_volume_iops
    throughput            = var.k3s_data_volume_throughput_mibps
    encrypted             = true
    delete_on_termination = true
  }

  tags        = merge(local.tags, local.infra_tags, { Name = "${local.name}-k3s", "cucina:e2e-role" = "k3s" })
  volume_tags = merge(local.tags, local.infra_tags, { Name = "${local.name}-k3s", "cucina:e2e-role" = "k3s" })

  # The address association must exist before the first boot.
  depends_on = [aws_eip_association.k3s]

  lifecycle {
    # Pods and Services must live outside the VPC range (k3s' default pod CIDR is this VPC's CIDR).
    precondition {
      condition = !anytrue([
        for pair in setproduct([local.base.vpc_cidr], [var.k3s_cluster_cidr, var.k3s_service_cidr]) :
        cidrcontains(pair[0], pair[1]) || cidrcontains(pair[1], pair[0])
      ]) && !(cidrcontains(var.k3s_cluster_cidr, var.k3s_service_cidr) || cidrcontains(var.k3s_service_cidr, var.k3s_cluster_cidr))
      error_message = "The k3s pod and service CIDRs must not overlap the VPC CIDR or each other."
    }

    # A newer Ubuntu build must not replace the node in the middle of a campaign
    # (force a refresh with `tofu apply -replace=aws_instance.k3s`).
    ignore_changes = [ami]
  }
}

# --- client ENI tags ---------------------------------------------------------------------

# aws_instance tags/volume_tags do not tag implicit ENIs. Keep this env-only template tag-only;
# the base Fast Launch prep template must not tag ENIs (its service role cannot). ADR 0203.
resource "aws_launch_template" "client_network_tags" {
  name        = "${local.name}-client-network-tags"
  description = "Ownership tags for e2e client network interfaces"

  tag_specifications {
    resource_type = "network-interface"
    tags          = merge(local.tags, local.infra_tags)
  }

  tags = merge(local.tags, local.infra_tags)
}

# --- linux-client ------------------------------------------------------------------------

resource "aws_instance" "linux_client" {
  ami                                  = local.linux_ami
  instance_type                        = var.client_instance_type
  subnet_id                            = local.base.public_subnet_id
  vpc_security_group_ids               = [local.base.sg_clients]
  iam_instance_profile                 = local.base.client_instance_profile_name
  associate_public_ip_address          = true # no EIP: the only EIP is the k3s node's
  instance_initiated_shutdown_behavior = "stop"
  monitoring                           = false
  user_data                            = local.linux_client_user_data
  user_data_replace_on_change          = true

  launch_template {
    id      = aws_launch_template.client_network_tags.id
    version = tostring(aws_launch_template.client_network_tags.latest_version)
  }

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required" # IMDSv2 only
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.linux_client_root_volume_gib
    encrypted             = true
    delete_on_termination = true
  }

  tags        = merge(local.tags, local.infra_tags, { Name = "${local.name}-linux-client", "cucina:e2e-role" = "linux-client" })
  volume_tags = merge(local.tags, local.infra_tags, { Name = "${local.name}-linux-client", "cucina:e2e-role" = "linux-client" })

  lifecycle {
    # ENI tags are create-only: attaching/updating the template must not replace a campaign client.
    ignore_changes = [ami, launch_template]
  }
}

# --- windows-client ----------------------------------------------------------------------

# Launched from the windows-base AMI (Windows Server 2025 + VS Build Tools) built by Packer.
# Skipped while windows_client_ami is empty.
resource "aws_instance" "windows_client" {
  count = var.windows_client_ami == "" ? 0 : 1

  ami                                  = var.windows_client_ami
  instance_type                        = var.client_instance_type
  subnet_id                            = local.base.public_subnet_id
  vpc_security_group_ids               = [local.base.sg_clients]
  iam_instance_profile                 = local.base.client_instance_profile_name
  associate_public_ip_address          = true
  instance_initiated_shutdown_behavior = "stop"
  monitoring                           = false
  get_password_data                    = false
  user_data                            = local.windows_client_user_data
  user_data_replace_on_change          = true

  launch_template {
    id      = aws_launch_template.client_network_tags.id
    version = tostring(aws_launch_template.client_network_tags.latest_version)
  }

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required" # IMDSv2 only
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  root_block_device {
    volume_type           = "gp3"
    volume_size           = var.windows_client_root_volume_gib
    encrypted             = true
    delete_on_termination = true
  }

  tags        = merge(local.tags, local.infra_tags, { Name = "${local.name}-windows-client", "cucina:e2e-role" = "windows-client" })
  volume_tags = merge(local.tags, local.infra_tags, { Name = "${local.name}-windows-client", "cucina:e2e-role" = "windows-client" })

  lifecycle {
    # ENI tags are create-only: attaching/updating the template must not replace a campaign client.
    ignore_changes = [ami, launch_template]
  }
}
