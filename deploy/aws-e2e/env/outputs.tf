# SPDX-License-Identifier: FSL-1.1-ALv2

# Flattened by scripts/up.sh into ~/.config/cucina/aws-e2e/env-outputs.json. Names are a contract.

output "region" {
  value = var.region
}

output "az" {
  value = local.base.az
}

output "k3s_instance_id" {
  value = aws_instance.k3s.id
}

output "k3s_private_ip" {
  description = "The control plane's fixed private IP: workers and in-VPC clients address it by this, never by the Elastic IP (NFR-T5/T9)."
  value       = local.k3s_private_ip
}

output "chart_endpoints" {
  description = "Values for the chart's `endpoints` block (written to values-endpoints.json by up.sh): the public endpoint is the Elastic IP, in-VPC clients and workers use the private IP, and every certificate carries both names."
  value = {
    client = {
      host       = aws_eip.k3s.public_ip
      extraNames = [local.k3s_private_ip]
    }
    worker = {
      host       = local.k3s_private_ip
      extraNames = [aws_eip.k3s.public_ip]
    }
    hosts = {
      host = aws_eip.k3s.public_ip
    }
  }
}

output "client_endpoint_private" {
  description = "Remote-execution endpoint for in-VPC clients (linux-client, windows-client): host:port on the private IP."
  value       = "${local.k3s_private_ip}:443"
}

output "k3s_public_ip" {
  description = "The Elastic IP (client and Mac endpoint, kubeconfig address)."
  value       = aws_eip.k3s.public_ip
}

output "k3s_eip_allocation_id" {
  value = aws_eip.k3s.id
}

output "k3s_version" {
  value = var.k3s_version
}

output "linux_client_instance_id" {
  value = aws_instance.linux_client.id
}

output "linux_client_private_ip" {
  value = aws_instance.linux_client.private_ip
}

output "windows_client_instance_id" {
  description = "Empty string while windows_client_ami is unset."
  value       = try(aws_instance.windows_client[0].id, "")
}

output "windows_client_private_ip" {
  value = try(aws_instance.windows_client[0].private_ip, "")
}

output "guard_minutes" {
  value = var.guard_minutes
}

output "tags" {
  value = local.tags
}
