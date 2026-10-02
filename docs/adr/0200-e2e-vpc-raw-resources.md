<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0200 — The e2e VPC is built from raw resources, not `terraform-aws-modules/vpc`

* Status: accepted (2026-10-02)

## Context
R-LIB-4 lists `terraform-aws-modules/vpc` v6.7.3 for the acceptance environment, and the task for
`deploy/aws-e2e` allows it "if it fits dual-stack / single-AZ / egress-only IGW / no NAT cleanly".
We evaluated it (v6.7.3, hashicorp/aws 6.67.0). It can express the topology (`enable_ipv6`,
`azs = [one]`, `create_egress_only_igw`, `enable_nat_gateway = false`), but it does not fit the
test strategy this layer is held to (R-TEST-6: `tofu test` with `mock_provider` and
`command = plan`, no Terratest):

* A test can only reference a module's **outputs**. `module.vpc.aws_vpc.this[0].tags` fails with
  "This object does not have an attribute named aws_vpc". The invariants the layer must prove at
  plan time (three tags on every resource, no NAT gateway or NAT route, egress-only IPv6 as the
  only default route of the private table, no world-open ingress) are properties of resources the
  module hides.
* The module declares 74 resource blocks (NAT gateways, EIPs, database/intra/ElastiCache/Redshift
  subnet groups, flow logs, ...). This topology uses about ten of them, and every unused one that
  costs money (NAT, EIP) would be something to prove stays off.
* `cidrsubnet()` over the generated IPv6 /56 and a private route table with only `::/0` →
  egress-only IGW are a few lines of raw HCL.

## Decision
`deploy/aws-e2e/base` declares the VPC, the two subnets, the IGW, the egress-only IGW, the route
tables (the main table is adopted and left empty), the S3 gateway endpoint and the security
groups as plain resources, each with explicit `tags` in addition to the provider's
`default_tags`. `tests/run.sh` adds structural checks the HCL assertions cannot express: no
`aws_nat_gateway`, exactly one `aws_eip` (in `env`), no world-open ingress, tags on every
resource.

## Consequences
* Every resource of the base layer is directly assertable; about 90 lines of network HCL are ours
  to maintain.
* `terraform-aws-modules/vpc` stays the right choice for a production VPC (EKS, multi-AZ); R-CP-8's
  EKS notes do not depend on this layer. Revisit if OpenTofu gains attribute access into module
  resources from `tofu test`.
