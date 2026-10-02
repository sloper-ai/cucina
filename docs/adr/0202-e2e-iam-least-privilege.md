<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0202 — Least-privilege IAM for the e2e environment: tag-gated controller, protected nodes, confined SSM

* Status: accepted (2026-10-02)

## Context
§10.1 asks for a controller policy that launches only with the required tags and destroys only
tagged resources, workers without EC2 permissions (R-POOL-3), and a k3s node that runs the
controller with its own instance profile (IMDSv2 hop limit 2, acceptable only for this temporary
cluster, R-CP-8). Three details make the obvious policy weaker than it looks:

1. The AWS-managed `AmazonSSMManagedInstanceCore` grants `ssm:GetParameter` and `ssm:GetParameters`
   on `*`. Attached as written, a worker could read every parameter in the account, not "its own
   pool's".
2. Every resource in the environment carries `cucina:env=e2e`, including the k3s node and the
   clients. A destroy permission conditioned only on that tag would let a controller bug terminate
   the control plane.
3. IAM cannot scope an instance profile to "its own pool": instance-profile credentials carry no
   per-instance principal tags.

## Decision
* `cucina-e2e-controller` (managed policy, 5.2 KB of the 6.1 KB limit; IAM Access Analyzer reports no
  findings): launches (`RunInstances`, `CreateFleet`) need `aws:RequestTag/cucina:env = e2e` and the keys
  `cucina:run` and `cucina:expires` on instances, volumes, ENIs and spot requests, so the controller's
  `extraTags` must carry all three tags (chart value `controller.ec2.extraTags`); everything launched from
  or into (AMIs, snapshots, the subnet, security groups, launch templates) must itself be tagged
  `cucina:env=e2e`; `TerminateInstances`, `DeleteVolume` and `DeleteNetworkInterface` need
  `aws:ResourceTag/cucina:env = e2e`; `CreateTags` may not touch the protective tag keys; `iam:PassRole`
  only for the worker role and only to `ec2.amazonaws.com`; `Describe*`, the price list and the ECR token
  are the only `Resource: *` allows; an explicit deny covers everything outside us-west-1.
* `cucina-e2e-controller-images` (second managed policy on the k3s node role): `DeregisterImage`,
  `DeleteSnapshot`, `EnableFastLaunch` / `DisableFastLaunch` on tagged images **and on the prep launch
  template**, plus the `RunInstances` dry run that `EnableFastLaunch` performs as the caller (without
  request tags), allowed only when it launches from the prep template (`ec2:LaunchTemplate`). All of this
  was found by calling `EnableFastLaunch` under the real policy: it is authorized against the launch
  template as well as the image, and it fails with "Run instances dry run failed" otherwise. Spot
  (`RunInstances` with market options) and `CreateFleet` were verified with dry runs under the real roles.
* The prep launch template tags instances and volumes only: Fast Launch launches the prep instances with
  `AWSServiceRoleForEC2FastLaunch`, which may not tag network interfaces, and a template that does makes
  the enable end in `enabled-failed` (a plan-time test guards it).
* Infrastructure nodes (k3s, clients) and their volumes carry `cucina:protected=true`; the controller
  policy has an explicit **deny** on terminate/stop/reboot/modify/delete/tagging for them.
* Every instance role attaches `AmazonSSMManagedInstanceCore` and an inline policy that **denies**
  `ssm:GetParameter*` outside its prefix: workers `/cucina/e2e/workers/*`, clients
  `/cucina/e2e/clients/*`, the k3s node `/cucina/e2e/*`. "Its own pool" is therefore a path
  convention below `/cucina/e2e/workers/<pool>/`, enforced by the controller's choice of path, not
  by IAM.
* The k3s node role carries the two controller policies and an ECR pull policy; nothing else in the
  environment has any EC2 permission, and the worker role has none (a plan-time test fails on any
  `ec2:` action there).

## Consequences
* A launch without the three tags is refused by IAM, which enforces §12 even if a controller bug
  forgets them. The e2e Helm values must set `controller.ec2.extraTags` accordingly.
* The controller cannot launch from an AMI that was not built with the tags (Packer `tags` and
  `snapshot_tags` are mandatory), and cannot use a subnet or security group outside this
  environment.
* The policies were reviewed against `internal/providers/ec2` (RunInstances, Terminate, volume and ENI
  cleanup, Fast Launch, price list) and exercised with real calls and dry runs under a throwaway role that
  carried exactly these policies: tagged launch, spot and CreateFleet allowed; untagged launch, wrong env,
  other roles, other regions, stop, protected instances denied; Fast Launch enable and disable work. If
  the provider gains calls, extend the policies and the plan-time assertions together.
* Spot and Fast Launch need the account's service-linked roles `AWSServiceRoleForEC2Spot` and
  `AWSServiceRoleForEC2FastLaunch`, which exist today; the policies deliberately have no
  `iam:CreateServiceLinkedRole` (account-level changes stay with the user: the first Fast Launch enable in
  a fresh account is done by the operator, who creates the role).
