# SPDX-License-Identifier: FSL-1.1-ALv2

# Base layer of the temporary acceptance environment (PROMPT.md §10.1): the dual-stack,
# single-AZ network, security groups, IAM and ECR. Nothing here costs money while idle
# (no NAT gateway, no Elastic IP, no interface endpoints).

data "aws_partition" "current" {}
data "aws_caller_identity" "current" {}

locals {
  name = "cucina-e2e"

  # The three mandatory tags (§12). They are applied twice on purpose: through the
  # provider's default_tags (safety net for anything created implicitly) and explicitly
  # on every taggable resource, which is what the offline `tofu test` suite asserts.
  tags = {
    "cucina:env"     = "e2e"
    "cucina:run"     = var.run_id
    "cucina:expires" = var.expires
  }

  account_id = data.aws_caller_identity.current.account_id
  partition  = data.aws_partition.current.partition
}

# --- Availability-zone selection ---------------------------------------------------
# Everything lives in ONE AZ (no cross-AZ charges, R-DATA-4). Pick the first allowed AZ
# in which every required instance type is offered.
data "aws_ec2_instance_type_offerings" "required" {
  for_each      = toset(var.required_instance_types)
  location_type = "availability-zone"

  filter {
    name   = "instance-type"
    values = [each.value]
  }
}

locals {
  az_offering_sets = [for t in sort(var.required_instance_types) : toset(data.aws_ec2_instance_type_offerings.required[t].locations)]
  az_candidates    = [for az in var.allowed_azs : az if alltrue([for s in local.az_offering_sets : contains(s, az)])]
  az               = var.az != "" ? var.az : try(local.az_candidates[0], "")
}
