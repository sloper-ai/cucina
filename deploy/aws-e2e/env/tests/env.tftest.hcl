# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Offline plan-time tests of the env layer (R-TEST-6 "Infrastructure"): mocked AWS provider and a
# stubbed base remote state, command = plan. The Windows client AMI is a placeholder (the real one
# comes from the images agent); the layer must plan cleanly with it.

mock_provider "aws" {
  mock_data "aws_ssm_parameter" {
    defaults = { insecure_value = "ami-0123456789abcdef0", value = "ami-0123456789abcdef0" }
  }
  mock_resource "aws_eip" {
    defaults = { public_ip = "198.51.100.10", id = "eipalloc-0123456789abcdef0" }
  }
  mock_resource "aws_launch_template" {
    defaults = { id = "lt-0123456789abcdef0", latest_version = 7 }
  }
}

# Stub of the base layer's outputs (what up.sh base produces); no real ids.
override_data {
  target = data.terraform_remote_state.base
  values = {
    outputs = {
      az                             = "us-west-1b"
      public_subnet_id               = "subnet-0123456789abcdef0"
      sg_control_plane               = "sg-0123456789abcdef0"
      sg_clients                     = "sg-0123456789abcdef1"
      vpc_cidr                       = "10.42.0.0/16"
      public_subnet_cidr             = "10.42.0.0/20"
      k3s_node_instance_profile_name = "cucina-e2e-k3s-node"
      client_instance_profile_name   = "cucina-e2e-client"
    }
  }
}

variables {
  state_dir          = "/nonexistent/cucina-test-state"
  run_id             = "e2e-test"
  expires            = "2030-01-01T00:00:00Z"
  windows_client_ami = "ami-0123456789abcdef0"
}

run "k3s_node" {
  command = plan

  # Guards: ADR 0004 — default to a two-vCPU m7i.large for functional tests; keep §10.1 storage unchanged.
  assert {
    condition     = aws_instance.k3s.instance_type == "m7i.large" && aws_instance.k3s.root_block_device[0].volume_size == 30
    error_message = "k3s node: m7i.large with a 30 GiB root volume (small functional-test default, ADR 0004)"
  }
  assert {
    condition = alltrue([
      for d in aws_instance.k3s.ebs_block_device :
      d.volume_type == "gp3" && d.volume_size >= 300 && d.iops >= 6000 && d.throughput >= 500 && d.delete_on_termination && d.device_name == "/dev/sdf"
    ]) && length(aws_instance.k3s.ebs_block_device) == 1
    error_message = "k3s data volume: gp3, >= 300 GiB, >= 6000 IOPS, >= 500 MiB/s, DeleteOnTermination"
  }
  assert {
    condition     = aws_instance.k3s.root_block_device[0].delete_on_termination
    error_message = "the root volume must be deleted with the instance"
  }

  # R-CP-8: IMDSv2 required, hop limit 2 (the node's instance profile serves the pods; temporary cluster only).
  assert {
    condition     = aws_instance.k3s.metadata_options[0].http_tokens == "required" && aws_instance.k3s.metadata_options[0].http_put_response_hop_limit == 2
    error_message = "k3s node: IMDSv2 required with hop limit 2"
  }

  # §12: runaway guard.
  assert {
    condition     = aws_instance.k3s.instance_initiated_shutdown_behavior == "stop"
    error_message = "the k3s node must stop (not terminate, not ignore) when the OS shuts down"
  }
  assert {
    condition     = strcontains(aws_instance.k3s.user_data, "shutdown -h +480")
    error_message = "the k3s node must arm `shutdown -h +480` (8 h runaway guard)"
  }

  # k3s: pinned release, SHA-256 verified, Traefik disabled, TLS SAN / external IP = the Elastic IP.
  assert {
    condition     = strcontains(aws_instance.k3s.user_data, "v1.36.5%2Bk3s1") && strcontains(aws_instance.k3s.user_data, "sha256sum -c -") && strcontains(aws_instance.k3s.user_data, var.k3s_sha256_amd64) && strcontains(aws_instance.k3s.user_data, var.k3s_install_sha256)
    error_message = "k3s binary and installer must be pinned by version and verified by SHA-256"
  }
  assert {
    condition     = strcontains(aws_instance.k3s.user_data, "node-external-ip: 198.51.100.10") && strcontains(aws_instance.k3s.user_data, "- 198.51.100.10") && strcontains(aws_instance.k3s.user_data, "- traefik")
    error_message = "k3s must be installed with --tls-san/--node-external-ip = the EIP and Traefik disabled (ServiceLB stays)"
  }
  assert {
    condition     = !strcontains(aws_instance.k3s.user_data, "get.k3s.io") && strcontains(aws_instance.k3s.user_data, "INSTALL_K3S_SKIP_DOWNLOAD=true")
    error_message = "no unpinned installer download; the pinned installer runs with the verified binary"
  }
  assert {
    condition     = strcontains(aws_instance.k3s.user_data, "/var/lib/rancher/k3s/storage")
    error_message = "the data volume must be mounted at /var/lib/rancher/k3s/storage"
  }

  # The API server must be advertised on the private address: with --node-external-ip alone k3s advertises the
  # EIP and every in-cluster client (CoreDNS, metrics-server, the controller) fails (pre-flight finding).
  assert {
    condition     = strcontains(aws_instance.k3s.user_data, "advertise-address: $priv") && strcontains(aws_instance.k3s.user_data, "node-external-ip: 198.51.100.10")
    error_message = "k3s must advertise the private address while publishing the EIP as the node's external IP"
  }

  # Pods and Services live outside the VPC range: k3s' default pod CIDR 10.42.0.0/16 equals the VPC CIDR.
  assert {
    condition     = strcontains(aws_instance.k3s.user_data, "cluster-cidr: 10.244.0.0/16") && strcontains(aws_instance.k3s.user_data, "service-cidr: 10.245.0.0/16") && strcontains(aws_instance.k3s.user_data, "cluster-dns: 10.245.0.10")
    error_message = "k3s must be configured with pod and service CIDRs that do not overlap the VPC"
  }

  # §10.1 "Images": the node pulls Cucina images from ECR with its instance profile (no expiring token).
  assert {
    condition     = strcontains(aws_instance.k3s.user_data, "image-credential-provider-config=") && strcontains(aws_instance.k3s.user_data, "image-credential-provider-bin-dir=") && strcontains(aws_instance.k3s.user_data, "*.dkr.ecr.*.amazonaws.com")
    error_message = "the kubelet must be configured with the ECR credential provider"
  }
}

run "exactly_one_elastic_ip" {
  command = plan

  # The static check in tests/run.sh asserts there is no other aws_eip anywhere; here: it is
  # allocated once, for the k3s node, and the clients rely on auto-assigned public IPv4 only.
  assert {
    condition     = aws_eip.k3s.domain == "vpc" && aws_eip_association.k3s.allocation_id == aws_eip.k3s.id && aws_eip_association.k3s.network_interface_id == aws_network_interface.k3s.id
    error_message = "the single Elastic IP is associated with the k3s node's primary interface before the instance exists"
  }
  assert {
    condition     = one(aws_instance.k3s.primary_network_interface).network_interface_id == aws_network_interface.k3s.id
    error_message = "the k3s node boots on the interface that already carries the Elastic IP"
  }
  assert {
    condition     = output.k3s_public_ip == aws_eip.k3s.public_ip
    error_message = "the EIP is exported as k3s_public_ip"
  }
}

run "stable_private_address_and_chart_values" {
  command = plan

  # NFR-T5/T9: workers and in-VPC clients reach the control plane over its private address, which must be stable
  # and must appear, with the public EIP, in every certificate the chart issues.
  assert {
    condition     = aws_network_interface.k3s.private_ips == toset(["10.42.0.10"])
    error_message = "the k3s node has a fixed private IP in the public subnet"
  }
  assert {
    condition     = output.chart_endpoints.client.host == "198.51.100.10" && contains(output.chart_endpoints.client.extraNames, "10.42.0.10")
    error_message = "public endpoint = Elastic IP; the private IP is an extra SAN of the client-facing certificates"
  }
  assert {
    condition     = output.chart_endpoints.worker.host == "10.42.0.10" && contains(output.chart_endpoints.worker.extraNames, "198.51.100.10") && output.chart_endpoints.hosts.host == "198.51.100.10"
    error_message = "workers use the private IP; the EIP is an extra SAN for Mac hosts, which connect over the internet"
  }
  assert {
    condition     = output.client_endpoint_private == "10.42.0.10:443"
    error_message = "in-VPC clients use the private remote-execution endpoint"
  }
}

run "private_ip_host_number_is_validated" {
  command = plan

  variables {
    k3s_private_ip_host = 2
  }

  expect_failures = [var.k3s_private_ip_host]
}

run "clients" {
  command = plan

  # Guards: ADR 0004 — both clients default to two-vCPU m7i.large; retain §10.1 AMIs, SSM and the 8 h guard.
  assert {
    condition     = aws_instance.linux_client.instance_type == "m7i.large" && aws_instance.windows_client[0].instance_type == "m7i.large"
    error_message = "both clients: m7i.large (small functional-test default, ADR 0004)"
  }
  assert {
    condition     = aws_instance.windows_client[0].ami == "ami-0123456789abcdef0"
    error_message = "the Windows client launches from var.windows_client_ami"
  }
  assert {
    condition     = aws_instance.linux_client.instance_initiated_shutdown_behavior == "stop" && aws_instance.windows_client[0].instance_initiated_shutdown_behavior == "stop"
    error_message = "clients must stop when the guard fires"
  }
  assert {
    condition     = strcontains(aws_instance.linux_client.user_data, "shutdown -h +480") && strcontains(aws_instance.windows_client[0].user_data, "shutdown.exe /s /t") && strcontains(aws_instance.windows_client[0].user_data, "[int]$Minutes = 480")
    error_message = "both clients arm the 8 h power-off guard"
  }
  assert {
    condition     = strcontains(aws_instance.linux_client.user_data, "bazelisk-linux-amd64") && strcontains(aws_instance.linux_client.user_data, "sha256sum -c -") && strcontains(aws_instance.linux_client.user_data, "git jq python3 unzip")
    error_message = "linux-client: pinned, SHA-256-verified Bazelisk plus git, python3 and unzip"
  }
  assert {
    condition     = strcontains(aws_instance.windows_client[0].user_data, "SetEnvironmentVariable('TMP', 'C:\\bb\\tmp', 'Machine')") && strcontains(aws_instance.windows_client[0].user_data, "SetEnvironmentVariable('TEMP', 'C:\\bb\\tmp', 'Machine')") && strcontains(aws_instance.windows_client[0].user_data, "New-Item -ItemType Directory -Force -Path C:\\bb\\tmp")
    error_message = "windows-client: TMP/TEMP machine variables point at C:\\bb\\tmp, which exists"
  }
  assert {
    condition     = strcontains(aws_instance.windows_client[0].user_data, "Get-FileHash -Algorithm SHA256") && strcontains(aws_instance.windows_client[0].user_data, var.bazelisk_windows_amd64_sha256) && strcontains(aws_instance.windows_client[0].user_data, var.git_for_windows_sha256)
    error_message = "windows-client: Git and Bazelisk are pinned and SHA-256 verified"
  }
}

run "imdsv2_on_every_instance" {
  command = plan

  assert {
    condition = alltrue([
      for m in [
        aws_instance.k3s.metadata_options[0],
        aws_instance.linux_client.metadata_options[0],
        aws_instance.windows_client[0].metadata_options[0],
      ] : m.http_tokens == "required" && m.http_endpoint == "enabled" && m.instance_metadata_tags == "enabled"
    ])
    error_message = "every instance requires IMDSv2 and exposes instance tags through IMDS"
  }
}

run "tags_on_every_resource" {
  command = plan

  assert {
    condition = alltrue([
      for t in [
        aws_eip.k3s.tags, aws_network_interface.k3s.tags,
        aws_instance.k3s.tags, aws_instance.k3s.volume_tags,
        aws_instance.linux_client.tags, aws_instance.linux_client.volume_tags,
        aws_instance.windows_client[0].tags, aws_instance.windows_client[0].volume_tags,
        aws_launch_template.client_network_tags.tags,
        one(aws_launch_template.client_network_tags.tag_specifications).tags,
      ] : try(t["cucina:env"] == "e2e" && t["cucina:run"] == "e2e-test" && t["cucina:expires"] == "2030-01-01T00:00:00Z", false)
    ])
    error_message = "every resource (including the volumes created with the instances) carries the three mandatory tags"
  }
  assert {
    condition = alltrue([
      for t in [
        aws_instance.k3s.tags, aws_instance.linux_client.tags, aws_instance.windows_client[0].tags,
        aws_launch_template.client_network_tags.tags,
        one(aws_launch_template.client_network_tags.tag_specifications).tags,
      ] : t["cucina:protected"] == "true"
    ])
    error_message = "infrastructure nodes, the client ENI template and its ENIs carry cucina:protected=true so the controller policy can never touch them"
  }

  # Guards: PROMPT.md §12 — tag the clients' implicit primary ENIs at launch, not just instances and volumes.
  assert {
    condition     = one(aws_launch_template.client_network_tags.tag_specifications).resource_type == "network-interface"
    error_message = "the client template must add only network-interface tags, leaving instance and volume tags on the instances"
  }
  assert {
    condition = alltrue([
      for client in [aws_instance.linux_client, aws_instance.windows_client[0]] :
      try(
        length(client.launch_template) == 1 &&
        one(client.launch_template).id == aws_launch_template.client_network_tags.id &&
        one(client.launch_template).version == tostring(aws_launch_template.client_network_tags.latest_version) &&
        can(regex("^[1-9][0-9]*$", one(client.launch_template).version)),
        false,
      )
    ])
    error_message = "both clients must use the ENI-tagging launch template with its explicit numeric latest version"
  }
}

run "windows_client_is_optional_until_the_ami_exists" {
  command = plan

  variables {
    windows_client_ami = ""
  }

  assert {
    condition     = length(aws_instance.windows_client) == 0 && output.windows_client_instance_id == ""
    error_message = "no Windows client without an AMI"
  }
}

run "ami_inputs_are_validated" {
  command = plan

  variables {
    windows_client_ami = "not-an-ami"
  }

  expect_failures = [var.windows_client_ami]
}

run "data_volume_floor" {
  command = plan

  variables {
    k3s_data_volume_iops = 3000
  }

  expect_failures = [var.k3s_data_volume_iops]
}

run "region_is_pinned" {
  command = plan

  variables {
    region = "eu-west-1"
  }

  expect_failures = [var.region]
}

run "k3s_cidrs_must_not_overlap_the_vpc" {
  command = plan

  variables {
    k3s_cluster_cidr = "10.42.0.0/16"
  }

  expect_failures = [aws_instance.k3s]
}
