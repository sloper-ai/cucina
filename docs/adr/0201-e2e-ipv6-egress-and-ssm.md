<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0201 — Private-subnet workers reach SSM over IPv6 only with dual-stack endpoints enabled

* Status: accepted (2026-10-02)

## Context
R-DATA-4 puts EC2 workers in a private subnet of a dual-stack VPC with an egress-only internet
gateway (IPv6) and an S3 gateway endpoint, no NAT gateway and no public IPv4, and asks for the
outcome of the IPv6 path to be recorded: "if an IPv6 path is missing (e.g. SSM messaging …) fall
back to auto-assigned public IPv4". We measured it on 2026-10-02 from a `t4g.nano` (Amazon Linux 2023,
SSM agent as shipped) in the base layer's private subnet: security group `workers`, instance
profile `cucina-e2e-client` (SSM core), no public IPv4, `::/0` through the egress-only IGW, no IPv4
default route.

| Destination | Result from the private subnet |
| --- | --- |
| `ssm.us-west-1.api.aws`, `ssmmessages.us-west-1.api.aws` (dual-stack) | reachable over IPv6 |
| `ssm.us-west-1.amazonaws.com`, `ssmmessages…`, `ec2messages…`, `sts…`, `api.ecr…` (legacy names) | A records only: unreachable (no IPv4 route) |
| `ec2messages.us-west-1.api.aws` | does not exist (no dual-stack name for the message gateway) |
| `ecr.us-west-1.api.aws`, `sts.us-west-1.api.aws`, `ec2.us-west-1.api.aws` | reachable over IPv6 |
| `s3.us-west-1.amazonaws.com` | reachable over IPv4 through the S3 gateway endpoint |
| `github.com`, `ghcr.io` | no AAAA records: unreachable |

With the agent's default configuration it registers against the legacy, IPv4-only
`ssm.us-west-1.amazonaws.com`, times out and never becomes Online. With
`{"Agent": {"UseDualStackEndpoint": true}}` in `amazon-ssm-agent.json` (upstream option; the same
result with explicit `Ssm.Endpoint` / `Mgs.Endpoint` overrides to the `.api.aws` names) the agent
was Online about 20 s after launch, held its control channel over IPv6, ran Run Command
(through the `ssmmessages` gateway: the agent logs harmless errors for the unreachable
`ec2messages` poller) and started a Session Manager session.

## Decision
Keep the R-DATA-4 topology: workers in the private subnet, no NAT gateway, no interface
endpoints, no public IPv4. The worker images must ship `Agent.UseDualStackEndpoint = true`
(Linux: `/etc/amazon/ssm/amazon-ssm-agent.json`; Windows: `%ProgramFiles%\Amazon\SSM\amazon-ssm-agent.json`,
the path the upstream agent uses; the Windows case was not exercised). Nothing a worker needs at
boot may come from `github.com` or `ghcr.io`: it is baked into the AMI or fetched from the
controller (private IPv4) or S3.

The fallback stays available and is a deployment choice, not a code change: put the public subnet
(`public_subnet_id`) in the pool's subnet list with `AssociatePublicIpAddress`, at about
$0.005 per running worker-hour. A NAT gateway only beats that above roughly ten workers running
on average (R-DATA-1, P8) and is not used.

## Consequences
* NFR-T9 holds in the test topology: no NAT bytes, no public-IPv4 bytes, worker internet egress is
  SSM control traffic only.
* An image built without the flag looks healthy in EC2 but is invisible to SSM; the symptom is an
  agent log full of `ssm.us-west-1.amazonaws.com … i/o timeout` (R-OPS runbook "worker won't register").
* Any other AWS API a worker calls must use a dual-stack hostname (`*.api.aws`) or be avoided; the
  EC2 SDK needs `AWS_USE_DUALSTACK_ENDPOINT=true`. The worker role has no EC2 permissions anyway.
* Windows over IPv6 is unverified until the Windows worker image is booted in the private subnet
  (the images build reports it).
