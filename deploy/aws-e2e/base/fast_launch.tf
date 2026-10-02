# SPDX-License-Identifier: FSL-1.1-ALv2

# Launch template that EC2 Fast Launch uses for its pre-provisioning ("prep") instances
# (R-POOL-2). The account has no default VPC, so Fast Launch needs an explicit subnet; a
# small instance type keeps the per-snapshot prep cost negligible. The template itself is
# free. It must NOT tag network interfaces: Fast Launch launches the prep instances with the
# service-linked role AWSServiceRoleForEC2FastLaunch, which may not tag them, and the whole
# enable fails ("enabled-failed ... not authorized to perform ec2:CreateTags on network-interface"). Windows AMIs enable Fast Launch via the Packer `fast_launch` block or the
# controller (ec2:EnableFastLaunch) and reference this template.

resource "aws_launch_template" "fast_launch_prep" {
  name                   = "${local.name}-fastlaunch-prep"
  description            = "EC2 Fast Launch prep instances for Windows worker AMIs"
  instance_type          = var.fast_launch_prep_instance_type
  update_default_version = true

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  network_interfaces {
    subnet_id                   = aws_subnet.private.id
    associate_public_ip_address = false
    delete_on_termination       = true
    security_groups             = [aws_security_group.workers.id]
  }

  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.tags, { Name = "${local.name}-fastlaunch-prep" })
  }

  tag_specifications {
    resource_type = "volume"
    tags          = merge(local.tags, { Name = "${local.name}-fastlaunch-prep" })
  }

  tags = merge(local.tags, { Name = "${local.name}-fastlaunch-prep" })
}
