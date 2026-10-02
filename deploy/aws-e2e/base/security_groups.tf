# SPDX-License-Identifier: FSL-1.1-ALv2

# Security groups (PROMPT.md §12 "Network exposure"): inbound only from inside the VPC
# (security-group references, private IPv4) and from the dev Mac's /32. There is no
# 0.0.0.0/0 or ::/0 ingress anywhere; egress is open so workers can reach SSM/ECR.

locals {
  mac_site_cidrs = var.mac_site_cidrs == null ? var.admin_cidrs : var.mac_site_cidrs

  # (port, cidr) products flattened into stable for_each keys.
  client_from_admin = { for p in setproduct(var.client_endpoint_ports, var.admin_cidrs) : "${p[0]}|${p[1]}" => { port = p[0], cidr = p[1] } }
  worker_from_mac   = { for p in setproduct(var.worker_endpoint_ports, local.mac_site_cidrs) : "${p[0]}|${p[1]}" => { port = p[0], cidr = p[1] } }
  builders_admin    = { for p in setproduct(var.builder_remote_ports, var.admin_cidrs) : "${p[0]}|${p[1]}" => { port = p[0], cidr = p[1] } }
  clients_admin     = { for p in setproduct(var.client_remote_ports, var.admin_cidrs) : "${p[0]}|${p[1]}" => { port = p[0], cidr = p[1] } }
  scrape_ranges     = { for r in var.worker_scrape_port_ranges : "${r.from}-${r.to}" => r }
}

# The VPC's default group is adopted and emptied: nothing may use it.
resource "aws_default_security_group" "main" {
  vpc_id = aws_vpc.main.id

  tags = merge(local.tags, { Name = "${local.name}-default-deny" })
}

resource "aws_security_group" "control_plane" {
  name        = "${local.name}-control-plane"
  description = "k3s control-plane node: client and worker endpoints, k3s API"
  vpc_id      = aws_vpc.main.id

  tags = merge(local.tags, { Name = "${local.name}-control-plane" })
}

resource "aws_security_group" "workers" {
  name        = "${local.name}-workers"
  description = "Cucina worker VMs: no inbound except Prometheus scrapes"
  vpc_id      = aws_vpc.main.id

  tags = merge(local.tags, { Name = "${local.name}-workers" })
}

resource "aws_security_group" "clients" {
  name        = "${local.name}-clients"
  description = "Bazel client VMs: no inbound by default (use SSM Session Manager)"
  vpc_id      = aws_vpc.main.id

  tags = merge(local.tags, { Name = "${local.name}-clients" })
}

resource "aws_security_group" "builders" {
  name        = "${local.name}-builders"
  description = "Packer builder instances: SSH/WinRM/RDP from the admin /32 only"
  vpc_id      = aws_vpc.main.id

  tags = merge(local.tags, { Name = "${local.name}-builders" })
}

# --- control-plane ingress -------------------------------------------------------------

resource "aws_vpc_security_group_ingress_rule" "cp_client_from_admin" {
  for_each = local.client_from_admin

  security_group_id = aws_security_group.control_plane.id
  description       = "Client endpoint from admin"
  ip_protocol       = "tcp"
  from_port         = each.value.port
  to_port           = each.value.port
  cidr_ipv4         = each.value.cidr

  tags = merge(local.tags, { Name = "${local.name}-cp-client-admin-${each.value.port}" })
}

resource "aws_vpc_security_group_ingress_rule" "cp_client_from_clients" {
  for_each = toset([for p in var.client_endpoint_ports : tostring(p)])

  security_group_id            = aws_security_group.control_plane.id
  description                  = "Client endpoint from the client VMs (private IPv4)"
  ip_protocol                  = "tcp"
  from_port                    = tonumber(each.value)
  to_port                      = tonumber(each.value)
  referenced_security_group_id = aws_security_group.clients.id

  tags = merge(local.tags, { Name = "${local.name}-cp-client-clients-${each.value}" })
}

resource "aws_vpc_security_group_ingress_rule" "cp_worker_from_workers" {
  for_each = toset([for p in var.worker_endpoint_ports : tostring(p)])

  security_group_id            = aws_security_group.control_plane.id
  description                  = "Worker endpoint from workers (private IPv4)"
  ip_protocol                  = "tcp"
  from_port                    = tonumber(each.value)
  to_port                      = tonumber(each.value)
  referenced_security_group_id = aws_security_group.workers.id

  tags = merge(local.tags, { Name = "${local.name}-cp-worker-workers-${each.value}" })
}

resource "aws_vpc_security_group_ingress_rule" "cp_worker_from_mac_sites" {
  for_each = local.worker_from_mac

  security_group_id = aws_security_group.control_plane.id
  description       = "Worker endpoint from Mac sites"
  ip_protocol       = "tcp"
  from_port         = each.value.port
  to_port           = each.value.port
  cidr_ipv4         = each.value.cidr

  tags = merge(local.tags, { Name = "${local.name}-cp-worker-mac-${each.value.port}" })
}

resource "aws_vpc_security_group_ingress_rule" "cp_k3s_api_from_admin" {
  for_each = toset(var.admin_cidrs)

  security_group_id = aws_security_group.control_plane.id
  description       = "k3s API from admin"
  ip_protocol       = "tcp"
  from_port         = var.k3s_api_port
  to_port           = var.k3s_api_port
  cidr_ipv4         = each.value

  tags = merge(local.tags, { Name = "${local.name}-cp-k3s-api" })
}

# --- workers ingress: Prometheus scrapes from the control plane only ---------------------

resource "aws_vpc_security_group_ingress_rule" "workers_scrape_from_control_plane" {
  for_each = local.scrape_ranges

  security_group_id            = aws_security_group.workers.id
  description                  = "Prometheus scrape from the control plane"
  ip_protocol                  = "tcp"
  from_port                    = each.value.from
  to_port                      = each.value.to
  referenced_security_group_id = aws_security_group.control_plane.id

  tags = merge(local.tags, { Name = "${local.name}-workers-scrape-${each.key}" })
}

# --- clients / builders: remote access from the admin /32 only (default: clients none) ----

resource "aws_vpc_security_group_ingress_rule" "clients_remote_from_admin" {
  for_each = local.clients_admin

  security_group_id = aws_security_group.clients.id
  description       = "Remote access from admin"
  ip_protocol       = "tcp"
  from_port         = each.value.port
  to_port           = each.value.port
  cidr_ipv4         = each.value.cidr

  tags = merge(local.tags, { Name = "${local.name}-clients-admin-${each.value.port}" })
}

resource "aws_vpc_security_group_ingress_rule" "builders_remote_from_admin" {
  for_each = local.builders_admin

  security_group_id = aws_security_group.builders.id
  description       = "Packer SSH/WinRM/RDP from admin"
  ip_protocol       = "tcp"
  from_port         = each.value.port
  to_port           = each.value.port
  cidr_ipv4         = each.value.cidr

  tags = merge(local.tags, { Name = "${local.name}-builders-admin-${each.value.port}" })
}

# --- isolation: fault injection for the campaign (T9e/T9f) -----------------------------------
# An instance whose security groups are swapped to this one loses the control plane (no inbound at
# all; outbound only HTTPS, which SSM needs) while staying manageable over SSM. The swap is an
# operator action (ec2:ModifyInstanceAttribute); the controller role cannot do it.

resource "aws_security_group" "isolation" {
  name        = "${local.name}-isolation"
  description = "Fault injection: no inbound, outbound HTTPS only (SSM)"
  vpc_id      = aws_vpc.main.id

  tags = merge(local.tags, { Name = "${local.name}-isolation" })
}

resource "aws_vpc_security_group_egress_rule" "isolation_https_ipv4" {
  security_group_id = aws_security_group.isolation.id
  description       = "HTTPS (SSM) over IPv4"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"

  tags = merge(local.tags, { Name = "${local.name}-isolation-https-v4" })
}

resource "aws_vpc_security_group_egress_rule" "isolation_https_ipv6" {
  security_group_id = aws_security_group.isolation.id
  description       = "HTTPS (SSM) over IPv6"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv6         = "::/0"

  tags = merge(local.tags, { Name = "${local.name}-isolation-https-v6" })
}

# --- egress: open (workers need SSM/ECR over IPv6, builders need the internet) ------------

locals {
  sg_ids = {
    control_plane = aws_security_group.control_plane.id
    workers       = aws_security_group.workers.id
    clients       = aws_security_group.clients.id
    builders      = aws_security_group.builders.id
  }
}

resource "aws_vpc_security_group_egress_rule" "all_ipv4" {
  for_each = local.sg_ids

  security_group_id = each.value
  description       = "All outbound IPv4"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"

  tags = merge(local.tags, { Name = "${local.name}-${each.key}-egress-v4" })
}

resource "aws_vpc_security_group_egress_rule" "all_ipv6" {
  for_each = local.sg_ids

  security_group_id = each.value
  description       = "All outbound IPv6"
  ip_protocol       = "-1"
  cidr_ipv6         = "::/0"

  tags = merge(local.tags, { Name = "${local.name}-${each.key}-egress-v6" })
}
