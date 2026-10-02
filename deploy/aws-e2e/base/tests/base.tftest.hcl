# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Offline plan-time tests of the base layer (R-TEST-6 "Infrastructure"): `tofu test` with a
# mocked AWS provider and command = plan. Nothing here talks to AWS. The real apply/destroy of
# the campaign is the integration test; tests/run.sh adds the structural checks that HCL
# assertions cannot express (no aws_nat_gateway resource, every resource carries tags).

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
  mock_data "aws_ec2_instance_type_offerings" {
    defaults = { locations = ["us-west-1b", "us-west-1c"] }
  }
  mock_resource "aws_vpc" {
    defaults = {
      ipv6_cidr_block           = "2600:1f1c:abc:de00::/56"
      default_route_table_id    = "rtb-0123456789abcdef0"
      default_security_group_id = "sg-0123456789abcdef0"
    }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::123456789012:role/cucina-e2e-worker" }
  }
  mock_resource "aws_iam_policy" {
    defaults = { arn = "arn:aws:iam::123456789012:policy/cucina-e2e-mock" }
  }
  mock_resource "aws_iam_instance_profile" {
    defaults = { arn = "arn:aws:iam::123456789012:instance-profile/cucina-e2e-worker" }
  }
}

# Distinct ids for the two route tables (a mock gives every resource of a type the same id).
override_resource {
  target = aws_route_table.private
  values = { id = "rtb-0000000000000private" }
}

override_resource {
  target = aws_route_table.public
  values = { id = "rtb-00000000000000public" }
}

variables {
  state_dir   = "/nonexistent/cucina-test-state"
  run_id      = "e2e-test"
  expires     = "2030-01-01T00:00:00Z"
  admin_cidrs = ["192.0.2.10/32"]
}

run "plan_is_valid" {
  command = plan

  # --- the AZ is chosen automatically and everything sits in it --------------------------
  assert {
    condition     = output.az == "us-west-1b"
    error_message = "the first allowed AZ that offers every required type must be chosen"
  }
  assert {
    condition     = aws_subnet.public.availability_zone == output.az && aws_subnet.private.availability_zone == output.az
    error_message = "both subnets must live in the single chosen AZ (R-DATA-4: one AZ, no cross-AZ charges)"
  }

  # --- topology: dual-stack, egress-only IPv6 for the private subnet, no NAT ------------------
  assert {
    condition     = aws_vpc.main.assign_generated_ipv6_cidr_block && aws_subnet.private.ipv6_cidr_block != null && aws_subnet.public.ipv6_cidr_block != null
    error_message = "the VPC and both subnets must be dual-stack"
  }
  assert {
    condition     = !aws_subnet.private.map_public_ip_on_launch && aws_subnet.public.map_public_ip_on_launch
    error_message = "workers' subnet must not hand out public IPv4; the public subnet must"
  }
  assert {
    condition     = aws_route.private_ipv6_default.egress_only_gateway_id != null && aws_route.private_ipv6_default.destination_ipv6_cidr_block == "::/0"
    error_message = "the private subnet's only default route is IPv6 through the egress-only IGW"
  }
  assert {
    condition = alltrue([
      for r in [aws_route.public_ipv4_default, aws_route.public_ipv6_default, aws_route.private_ipv6_default] :
      try(r.nat_gateway_id, null) == null
    ])
    error_message = "no route may target a NAT gateway (R-DATA-4, NFR-T9)"
  }
  assert {
    condition     = aws_route.private_ipv6_default.route_table_id == aws_route_table.private.id && aws_route.public_ipv4_default.route_table_id == aws_route_table.public.id && aws_route.public_ipv6_default.route_table_id == aws_route_table.public.id
    error_message = "default routes to the internet gateway belong to the public table only; the private table has the egress-only IPv6 route and no IPv4 default"
  }
  assert {
    condition     = aws_vpc_endpoint.s3.vpc_endpoint_type == "Gateway" && endswith(aws_vpc_endpoint.s3.service_name, ".s3")
    error_message = "S3 must be reached through a (free) gateway endpoint"
  }

  # --- no 0.0.0.0/0 or ::/0 ingress anywhere (§12) ------------------------------------------
  assert {
    condition = alltrue([
      for r in concat(
        values(aws_vpc_security_group_ingress_rule.cp_client_from_admin),
        values(aws_vpc_security_group_ingress_rule.cp_client_from_clients),
        values(aws_vpc_security_group_ingress_rule.cp_worker_from_workers),
        values(aws_vpc_security_group_ingress_rule.cp_worker_from_mac_sites),
        values(aws_vpc_security_group_ingress_rule.cp_k3s_api_from_admin),
        values(aws_vpc_security_group_ingress_rule.workers_scrape_from_control_plane),
        values(aws_vpc_security_group_ingress_rule.clients_remote_from_admin),
        values(aws_vpc_security_group_ingress_rule.builders_remote_from_admin),
      ) : try(r.cidr_ipv4, null) != "0.0.0.0/0" && try(r.cidr_ipv6, null) != "::/0"
    ])
    error_message = "no security-group rule may allow ingress from 0.0.0.0/0 or ::/0"
  }
  assert {
    condition = alltrue([
      for r in values(aws_vpc_security_group_ingress_rule.cp_client_from_admin) : r.cidr_ipv4 == "192.0.2.10/32"
    ])
    error_message = "the client endpoint must only be open to the admin /32"
  }
  assert {
    condition = alltrue([
      for r in values(aws_vpc_security_group_ingress_rule.workers_scrape_from_control_plane) : r.referenced_security_group_id != null && r.cidr_ipv4 == null
    ])
    error_message = "workers accept inbound only from the control-plane security group (Prometheus scrapes)"
  }
  assert {
    condition     = length(aws_vpc_security_group_ingress_rule.clients_remote_from_admin) == 0
    error_message = "clients must have no inbound rules by default (SSM Session Manager instead of SSH/RDP/WinRM)"
  }

  # The isolation group (T9e/T9f fault injection): no inbound anywhere, outbound HTTPS only.
  assert {
    condition = alltrue([
      for r in [aws_vpc_security_group_egress_rule.isolation_https_ipv4, aws_vpc_security_group_egress_rule.isolation_https_ipv6] :
      r.security_group_id == aws_security_group.isolation.id && r.ip_protocol == "tcp" && r.from_port == 443 && r.to_port == 443
    ])
    error_message = "the isolation security group may only send HTTPS (SSM)"
  }
  assert {
    condition     = alltrue([for k, v in aws_vpc_security_group_egress_rule.all_ipv4 : k != "isolation"]) && alltrue([for k, v in aws_vpc_security_group_egress_rule.all_ipv6 : k != "isolation"])
    error_message = "the isolation security group must not get the open egress rules"
  }

  # --- the three mandatory tags on every resource (§12) --------------------------------------
  assert {
    condition = alltrue([
      for t in concat(
        [
          aws_vpc.main.tags, aws_subnet.public.tags, aws_subnet.private.tags,
          aws_internet_gateway.main.tags, aws_egress_only_internet_gateway.main.tags,
          aws_default_route_table.main.tags, aws_route_table.public.tags, aws_route_table.private.tags,
          aws_vpc_endpoint.s3.tags, aws_default_security_group.main.tags,
          aws_security_group.control_plane.tags, aws_security_group.workers.tags,
          aws_security_group.clients.tags, aws_security_group.builders.tags, aws_security_group.isolation.tags,
          aws_vpc_security_group_egress_rule.isolation_https_ipv4.tags, aws_vpc_security_group_egress_rule.isolation_https_ipv6.tags,
          aws_iam_policy.controller.tags, aws_iam_policy.ecr_pull.tags,
          aws_iam_role.worker.tags, aws_iam_role.k3s_node.tags, aws_iam_role.client.tags,
          aws_iam_instance_profile.worker.tags, aws_iam_instance_profile.k3s_node.tags, aws_iam_instance_profile.client.tags,
          aws_launch_template.fast_launch_prep.tags,
        ],
        [for r in values(aws_vpc_security_group_ingress_rule.cp_client_from_admin) : r.tags],
        [for r in values(aws_vpc_security_group_ingress_rule.cp_client_from_clients) : r.tags],
        [for r in values(aws_vpc_security_group_ingress_rule.cp_worker_from_workers) : r.tags],
        [for r in values(aws_vpc_security_group_ingress_rule.cp_worker_from_mac_sites) : r.tags],
        [for r in values(aws_vpc_security_group_ingress_rule.cp_k3s_api_from_admin) : r.tags],
        [for r in values(aws_vpc_security_group_ingress_rule.workers_scrape_from_control_plane) : r.tags],
        [for r in values(aws_vpc_security_group_ingress_rule.builders_remote_from_admin) : r.tags],
        [for r in values(aws_vpc_security_group_egress_rule.all_ipv4) : r.tags],
        [for r in values(aws_vpc_security_group_egress_rule.all_ipv6) : r.tags],
        [for r in values(aws_ecr_repository.cucina) : r.tags],
      ) :
      try(t["cucina:env"] == "e2e" && t["cucina:run"] == "e2e-test" && t["cucina:expires"] == "2030-01-01T00:00:00Z", false)
    ])
    error_message = "every taggable resource must carry cucina:env=e2e, cucina:run and cucina:expires"
  }
  assert {
    condition = alltrue([
      for t in [
        aws_launch_template.fast_launch_prep.tag_specifications[0].tags,
        aws_launch_template.fast_launch_prep.tag_specifications[1].tags,
      ] : try(t["cucina:env"] == "e2e" && t["cucina:run"] == "e2e-test" && t["cucina:expires"] == "2030-01-01T00:00:00Z", false)
    ])
    error_message = "resources launched from the Fast Launch prep template must carry the tags too"
  }

  # --- IMDSv2 on everything that can launch an instance ---------------------------------------
  assert {
    condition     = aws_launch_template.fast_launch_prep.metadata_options[0].http_tokens == "required"
    error_message = "the Fast Launch prep template must require IMDSv2"
  }
}

run "worker_role_has_no_ec2_permissions" {
  command = plan

  # R-POOL-3: the instance profile has no EC2 permissions: SSM core plus, at most, its own pool's parameters.
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.worker.policy).Statement :
      alltrue([
        for a in(try(tolist(s.Action), [s.Action])) :
        !startswith(lower(a), "ec2:") && a != "*" && !endswith(a, ":*")
      ])
    ])
    error_message = "the worker role policy must not contain ec2:* (or wildcard) actions"
  }
  assert {
    condition     = endswith(aws_iam_role_policy_attachment.worker_ssm_core.policy_arn, "policy/AmazonSSMManagedInstanceCore")
    error_message = "the worker role's managed policy is AmazonSSMManagedInstanceCore"
  }
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_role_policy.worker.policy).Statement :
      s.Effect == "Deny" || alltrue([for r in tolist(s.Resource) : endswith(r, ":parameter/cucina/e2e/workers/*")])
    ])
    error_message = "workers may only read their own parameter prefix"
  }
  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.worker.policy).Statement :
      s.Effect == "Deny" && contains(s.Action, "ssm:GetParameter") && length(try(s.NotResource, [])) == 1
    ])
    error_message = "the managed SSM policy grants ssm:GetParameter on *; an explicit deny must confine it to the worker prefix"
  }
  assert {
    condition     = aws_iam_role.worker.assume_role_policy != null && jsondecode(aws_iam_role.worker.assume_role_policy).Statement[0].Principal.Service == "ec2.amazonaws.com"
    error_message = "only EC2 may assume the worker role"
  }
}

run "controller_policy_is_tag_conditioned" {
  command = plan

  # Allow statements that create instances/volumes/ENIs/fleets must require the request tags.
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_policy.controller.policy).Statement :
      s.Effect != "Allow" || !(contains(try(tolist(s.Action), [s.Action]), "ec2:RunInstances") || contains(try(tolist(s.Action), [s.Action]), "ec2:CreateFleet")) ||
      try(s.Condition.StringEquals["aws:RequestTag/cucina:env"] == "e2e" || s.Condition.StringEquals["aws:ResourceTag/cucina:env"] == "e2e", false)
    ])
    error_message = "RunInstances/CreateFleet must be conditioned on cucina:env (request tag for new resources, resource tag for the things launched from)"
  }
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_policy.controller.policy).Statement :
      s.Effect != "Allow" || !contains(try(tolist(s.Action), [s.Action]), "ec2:RunInstances") || !can(s.Condition.Null) ||
      (s.Condition.Null["aws:RequestTag/cucina:run"] == "false" && s.Condition.Null["aws:RequestTag/cucina:expires"] == "false")
    ])
    error_message = "launches must carry all three tags (run and expires are required keys, not just env)"
  }

  # Every destructive Allow is limited to resources tagged cucina:env=e2e.
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_policy.controller.policy).Statement :
      s.Effect != "Allow" ||
      !anytrue([
        for a in try(tolist(s.Action), [s.Action]) :
        anytrue([for p in ["ec2:Terminate", "ec2:Delete", "ec2:Deregister", "ec2:DisableFastLaunch", "ec2:EnableFastLaunch", "ec2:ModifyLaunchTemplate"] : startswith(a, p)])
      ]) ||
      try(s.Condition.StringEquals["aws:ResourceTag/cucina:env"] == "e2e", false)
    ])
    error_message = "destructive actions must require aws:ResourceTag/cucina:env = e2e"
  }

  # iam:PassRole only for the worker role, only to EC2.
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_policy.controller.policy).Statement :
      s.Effect != "Allow" || !contains(try(tolist(s.Action), [s.Action]), "iam:PassRole") ||
      (length(s.Resource) == 1 && s.Resource[0] == aws_iam_role.worker.arn && s.Condition.StringEquals["iam:PassedToService"] == "ec2.amazonaws.com")
    ])
    error_message = "iam:PassRole must be limited to the worker role and to ec2.amazonaws.com"
  }

  # No unconditioned mutation: an Allow on Resource "*" may only contain read-only actions.
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_policy.controller.policy).Statement :
      s.Effect != "Allow" || !contains(try(tolist(s.Resource), [s.Resource]), "*") ||
      alltrue([
        for a in try(tolist(s.Action), [s.Action]) :
        contains(["ec2:Describe*", "ecr:GetAuthorizationToken", "pricing:GetProducts", "pricing:DescribeServices", "pricing:GetAttributeValues"], a)
      ])
    ])
    error_message = "only Describe*, the ECR token and the price list may use Resource *"
  }

  # The infrastructure nodes (k3s, clients) are protected from the controller, and the policy fits IAM's size limit.
  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_policy.controller.policy).Statement :
      s.Effect == "Deny" && contains(s.Action, "ec2:TerminateInstances") && s.Condition.StringEquals["aws:ResourceTag/cucina:protected"] == "true"
    ])
    error_message = "an explicit deny must protect resources tagged cucina:protected=true"
  }
  assert {
    condition     = length(aws_iam_policy.controller.policy) < 6144
    error_message = "a managed policy may not exceed 6144 characters"
  }

  # Swapping a worker's security groups (T9e/T9f) is an operator action: no controller policy allows it.
  assert {
    condition = alltrue([
      for pol in [aws_iam_policy.controller.policy, aws_iam_policy.controller_images.policy] : alltrue([
        for s in jsondecode(pol).Statement :
        s.Effect != "Allow" || !anytrue([for a in try(tolist(s.Action), [s.Action]) : contains(["ec2:ModifyInstanceAttribute", "ec2:ModifyNetworkInterfaceAttribute", "ec2:*"], a)])
      ])
    ])
    error_message = "the controller must not be able to change an instance's security groups"
  }
}

run "controller_images_policy_is_scoped" {
  command = plan

  # Image lifecycle and Fast Launch: every Allow is either limited to cucina:env=e2e resources or is the
  # RunInstances dry run that EnableFastLaunch performs, allowed only from the prep launch template.
  assert {
    condition = alltrue([
      for s in jsondecode(aws_iam_policy.controller_images.policy).Statement :
      s.Effect != "Allow" || try(s.Condition.StringEquals["aws:ResourceTag/cucina:env"] == "e2e", false) ||
      try(s.Condition.ArnEquals["ec2:LaunchTemplate"] == aws_launch_template.fast_launch_prep.arn, false)
    ])
    error_message = "image / Fast Launch permissions must require the env tag, or the prep launch template for the launch dry run"
  }
  # EnableFastLaunch is authorized against the launch template as well as the image (found on real EC2).
  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_policy.controller_images.policy).Statement :
      contains(try(tolist(s.Action), [s.Action]), "ec2:EnableFastLaunch") && anytrue([for r in tolist(s.Resource) : strcontains(r, ":launch-template/")])
      ]) && anytrue([
      for s in jsondecode(aws_iam_policy.controller_images.policy).Statement :
      contains(try(tolist(s.Action), [s.Action]), "ec2:EnableFastLaunch") && anytrue([for r in tolist(s.Resource) : strcontains(r, "::image/")])
    ])
    error_message = "Enable/DisableFastLaunch must be allowed on the (tagged) images and on the prep launch template"
  }
  assert {
    condition     = aws_iam_role_policy_attachment.k3s_node_controller_images.policy_arn == aws_iam_policy.controller_images.arn && length(aws_iam_policy.controller_images.policy) < 6144
    error_message = "the k3s node carries the images policy, which fits the managed-policy size limit"
  }
  # Fast Launch launches the prep instances with its service-linked role, which cannot tag network interfaces:
  # a template that tags them makes the enable fail ("enabled-failed ... ec2:CreateTags on network-interface").
  assert {
    condition     = alltrue([for t in aws_launch_template.fast_launch_prep.tag_specifications : contains(["instance", "volume"], t.resource_type)])
    error_message = "the Fast Launch prep template may only tag instances and volumes"
  }
}

run "k3s_node_carries_the_controller_policy" {
  command = plan

  assert {
    condition     = aws_iam_role_policy_attachment.k3s_node_controller.policy_arn == aws_iam_policy.controller.arn
    error_message = "the k3s node (IMDSv2 hop-limit 2, R-CP-8) runs the controller with the node's instance profile"
  }
}

run "admin_cidr_must_be_narrow" {
  command = plan

  variables {
    admin_cidrs = ["0.0.0.0/0"]
  }

  expect_failures = [var.admin_cidrs]
}

run "region_is_pinned" {
  command = plan

  variables {
    region = "us-east-1"
  }

  expect_failures = [var.region]
}
