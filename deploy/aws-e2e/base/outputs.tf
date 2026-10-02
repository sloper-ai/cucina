# SPDX-License-Identifier: FSL-1.1-ALv2

# Consumed by deploy/aws-e2e/env (terraform_remote_state) and, flattened by
# scripts/up.sh, written to ~/.config/cucina/aws-e2e/base-outputs.json for the other agents
# (Packer, controller, test harness). Output names are a contract: add, never rename.

output "region" {
  value = var.region
}

output "az" {
  description = "The single AZ that hosts everything."
  value       = local.az
}

output "vpc_id" {
  value = aws_vpc.main.id
}

output "vpc_cidr" {
  value = aws_vpc.main.cidr_block
}

output "vpc_ipv6_cidr" {
  value = aws_vpc.main.ipv6_cidr_block
}

output "public_subnet_id" {
  value = aws_subnet.public.id
}

output "private_subnet_id" {
  value = aws_subnet.private.id
}

output "route_table_public_id" {
  value = aws_route_table.public.id
}

output "route_table_private_id" {
  value = aws_route_table.private.id
}

output "egress_only_igw_id" {
  value = aws_egress_only_internet_gateway.main.id
}

output "s3_gateway_endpoint_id" {
  value = aws_vpc_endpoint.s3.id
}

output "sg_control_plane" {
  value = aws_security_group.control_plane.id
}

output "sg_workers" {
  value = aws_security_group.workers.id
}

output "sg_clients" {
  value = aws_security_group.clients.id
}

output "sg_builders" {
  description = "Packer builders: SSH/WinRM/RDP from the admin /32 only."
  value       = aws_security_group.builders.id
}

output "sg_isolation" {
  description = "Fault injection (T9e/T9f): swap a worker's security groups to this one to cut it off the control plane; HTTPS egress only, no inbound."
  value       = aws_security_group.isolation.id
}

output "public_subnet_cidr" {
  value = aws_subnet.public.cidr_block
}

output "private_subnet_cidr" {
  value = aws_subnet.private.cidr_block
}

output "admin_cidrs" {
  value = var.admin_cidrs
}

output "admin_cidr" {
  description = "First admin CIDR (the dev Mac /32), or null when none was given."
  value       = try(var.admin_cidrs[0], null)
}

output "tags" {
  description = "The three mandatory resource tags (cucina:env, cucina:run, cucina:expires)."
  value       = local.tags
}

# --- IAM -------------------------------------------------------------------------------

output "controller_policy_arn" {
  value = aws_iam_policy.controller.arn
}

output "controller_images_policy_arn" {
  value = aws_iam_policy.controller_images.arn
}

output "worker_role_name" {
  value = aws_iam_role.worker.name
}

output "worker_role_arn" {
  value = aws_iam_role.worker.arn
}

output "worker_instance_profile_name" {
  value = aws_iam_instance_profile.worker.name
}

output "worker_instance_profile_arn" {
  value = aws_iam_instance_profile.worker.arn
}

output "k3s_node_role_name" {
  value = aws_iam_role.k3s_node.name
}

output "k3s_node_role_arn" {
  value = aws_iam_role.k3s_node.arn
}

output "k3s_node_instance_profile_name" {
  value = aws_iam_instance_profile.k3s_node.name
}

output "k3s_node_instance_profile_arn" {
  value = aws_iam_instance_profile.k3s_node.arn
}

output "client_role_name" {
  value = aws_iam_role.client.name
}

output "client_role_arn" {
  value = aws_iam_role.client.arn
}

output "client_instance_profile_name" {
  description = "Also suitable for Packer builders (SSM Session Manager)."
  value       = aws_iam_instance_profile.client.name
}

output "client_instance_profile_arn" {
  value = aws_iam_instance_profile.client.arn
}

output "ssm_parameter_prefix" {
  description = "SSM parameters readable by the controller; workers read only .../workers/*, clients only .../clients/*."
  value       = "/cucina/e2e"
}

# --- ECR / Fast Launch -------------------------------------------------------------------

output "ecr_registry" {
  description = "Registry host for docker login / k3s registries."
  value       = "${local.account_id}.dkr.ecr.${var.region}.amazonaws.com"
}

output "ecr_repository_urls" {
  description = "Map of short name (controller, sts) to repository URL."
  value       = { for k, r in aws_ecr_repository.cucina : k => r.repository_url }
}

output "fast_launch_template_id" {
  description = "Launch template for EC2 Fast Launch prep instances (private subnet, small type)."
  value       = aws_launch_template.fast_launch_prep.id
}

output "fast_launch_template_version" {
  value = "$Default"
}
