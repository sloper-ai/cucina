# SPDX-License-Identifier: FSL-1.1-ALv2

# Dual-stack VPC, one AZ, no NAT gateway (R-DATA-4, §10.1).
#   public subnet  : k3s node, client VMs, Packer builders (IPv4 + IPv6 via the IGW)
#   private subnet : workers. IPv6 egress via an egress-only IGW; S3 via a gateway
#                    endpoint; NO IPv4 default route (the IPv4 fallback is an
#                    auto-assigned public IPv4 on workers placed in the public subnet).
# Raw resources instead of terraform-aws-modules/vpc: see docs/adr/0201-*.md.

resource "aws_vpc" "main" {
  cidr_block                       = var.vpc_cidr
  assign_generated_ipv6_cidr_block = true
  enable_dns_support               = true
  enable_dns_hostnames             = true

  tags = merge(local.tags, { Name = "${local.name}-vpc" })
}

resource "aws_subnet" "public" {
  vpc_id                          = aws_vpc.main.id
  availability_zone               = local.az
  cidr_block                      = cidrsubnet(var.vpc_cidr, 4, 0)
  ipv6_cidr_block                 = cidrsubnet(aws_vpc.main.ipv6_cidr_block, 8, 0)
  map_public_ip_on_launch         = true
  assign_ipv6_address_on_creation = true

  tags = merge(local.tags, { Name = "${local.name}-public" })

  lifecycle {
    precondition {
      condition     = local.az != ""
      error_message = "No allowed AZ offers every required instance type (see var.required_instance_types / var.allowed_azs)."
    }
  }
}

resource "aws_subnet" "private" {
  vpc_id                          = aws_vpc.main.id
  availability_zone               = local.az
  cidr_block                      = cidrsubnet(var.vpc_cidr, 4, 1)
  ipv6_cidr_block                 = cidrsubnet(aws_vpc.main.ipv6_cidr_block, 8, 1)
  map_public_ip_on_launch         = false
  assign_ipv6_address_on_creation = true

  tags = merge(local.tags, { Name = "${local.name}-private" })
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id

  tags = merge(local.tags, { Name = "${local.name}-igw" })
}

resource "aws_egress_only_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id

  tags = merge(local.tags, { Name = "${local.name}-eigw" })
}

# The VPC's main route table is adopted so that it carries the tags and no routes.
resource "aws_default_route_table" "main" {
  default_route_table_id = aws_vpc.main.default_route_table_id

  tags = merge(local.tags, { Name = "${local.name}-main-unused" })
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  tags = merge(local.tags, { Name = "${local.name}-public" })
}

resource "aws_route" "public_ipv4_default" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.main.id
}

resource "aws_route" "public_ipv6_default" {
  route_table_id              = aws_route_table.public.id
  destination_ipv6_cidr_block = "::/0"
  gateway_id                  = aws_internet_gateway.main.id
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id

  tags = merge(local.tags, { Name = "${local.name}-private" })
}

# IPv6 egress only. There is deliberately no IPv4 default route in the private table.
resource "aws_route" "private_ipv6_default" {
  route_table_id              = aws_route_table.private.id
  destination_ipv6_cidr_block = "::/0"
  egress_only_gateway_id      = aws_egress_only_internet_gateway.main.id
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table_association" "private" {
  subnet_id      = aws_subnet.private.id
  route_table_id = aws_route_table.private.id
}

# S3 gateway endpoint: free; carries ECR layer pulls and any S3 traffic without NAT.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.main.id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.private.id, aws_route_table.public.id]

  tags = merge(local.tags, { Name = "${local.name}-s3" })
}
