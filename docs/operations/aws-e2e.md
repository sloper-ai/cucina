<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# AWS acceptance environment (`deploy/aws-e2e`)

The temporary, fully tagged, single-AZ environment in `us-west-1` that hosts the acceptance campaign
(PROMPT.md §10, T0–T22). OpenTofu v1.13.1 with `hashicorp/aws` 6.67.0 in two root modules, POSIX
scripts around them, and a tag sweep that proves the teardown. Nothing here is production
infrastructure: for EKS see the chart's R-CP-8 notes.

| Layer | Creates | Costs while idle |
| --- | --- | --- |
| `base` | dual-stack VPC, public + private subnet in one AZ, IGW, egress-only IGW, S3 gateway endpoint, security groups, IAM (two controller policies, worker / k3s-node / client roles and instance profiles), ECR repositories, a Fast Launch prep launch template | about $0 (see Cost) |
| `env` | k3s node (the only Elastic IP), `linux-client`, `windows-client` | about $1.29 per hour while up |

Worker pools are **not** created by OpenTofu. `cucina-controller` launches them from chart values; the
layers only create their prerequisites.

## Bring up

Prerequisites: OpenTofu 1.13.1, `aws` (profile `default`, a valid SSO session), `jq`, `curl`; optional
`tflint`, `shellcheck`, `session-manager-plugin`. Export the tag values once per shell:

```sh
export CUCINA_RUN_ID=e2e-<date><letter>          # value of the cucina:run tag
export CUCINA_EXPIRES=<UTC ISO-8601, e.g. 2026-10-09T00:00:00Z>
export AWS_PROFILE=default AWS_REGION=us-west-1
export CUCINA_SECRETS_DIR=$HOME/.config/cucina   # default; state, outputs and kubeconfig live here (0700)
```

```sh
deploy/aws-e2e/scripts/env-check.sh          # profile, region, SSO session, quotas, tools (read-only)
deploy/aws-e2e/scripts/up.sh base            # VPC, security groups, IAM, ECR: ready in about a minute
# ... build the AMIs (workers/*), then:
deploy/aws-e2e/scripts/up.sh env             # k3s node + clients (Windows client only when a windows-base AMI exists)
deploy/aws-e2e/scripts/kubeconfig.sh         # waits for SSM and the k3s bootstrap, writes ~/.config/cucina/aws-e2e/kubeconfig
export KUBECONFIG=$HOME/.config/cucina/aws-e2e/kubeconfig
```

`up.sh` prints a one-line plan summary, refuses to apply a plan that destroys or replaces anything
without `--yes`, and writes full logs to `~/.config/cucina/aws-e2e/<layer>.{plan,apply}.txt`. Use
`--plan-only` to review. The admin CIDR is this machine's public IPv4 `/32` (checkip.amazonaws.com), or
`CUCINA_ADMIN_CIDRS` (comma separated, each at least a `/24`); when your IP changes, re-run
`up.sh base` to move the security-group rules.

Inputs for the `env` layer (all optional): `CUCINA_WINDOWS_CLIENT_AMI` (default: `windows-base.ami_id` from
`~/.config/cucina/aws-e2e/amis.json`, which the image builds write) and
`~/.config/cucina/aws-e2e/env.tfvars.json` for any other variable (sizes, pins, `k3s_ami`, `guard_minutes`).

### Outputs for the other tools

`~/.config/cucina/aws-e2e/base-outputs.json` and `env-outputs.json` (0600, flat `{name: value}`),
refreshed by `up.sh` and `scripts/outputs.sh [--json] base|env [key]`:

| Key | Meaning |
| --- | --- |
| `az`, `vpc_id`, `public_subnet_id`, `private_subnet_id` | the one AZ and its subnets |
| `sg_control_plane`, `sg_workers`, `sg_clients`, `sg_builders`, `sg_isolation` | security groups (`sg_builders`: Packer builders, SSH/WinRM/RDP from the admin `/32` only; `sg_isolation`: fault injection, see below) |
| `public_subnet_cidr`, `private_subnet_cidr` | the subnets' ranges |
| `worker_role_arn`, `worker_instance_profile_name` / `_arn`, `k3s_node_*`, `client_*`, `controller_policy_arn` | IAM |
| `ecr_registry`, `ecr_repository_urls` | `cucina/controller`, `cucina/sts` |
| `fast_launch_template_id` | prep template for EC2 Fast Launch (private subnet, `m7i.large`) |
| `admin_cidr(s)`, `tags` | the admin `/32` and the three mandatory tags |
| env: `k3s_instance_id`, `k3s_private_ip` (fixed), `k3s_public_ip` (the EIP), `linux_client_*`, `windows_client_*` | compute |
| env: `chart_endpoints`, `client_endpoint_private` | the chart's `endpoints` values (also written to `values-endpoints.json`) and the private remote-execution endpoint for in-VPC clients |

### Wiring the chart and the controller

The control plane has a **fixed private IP** (`k3s_private_ip`, host 10 of the public subnet by default,
`k3s_private_ip_host`), so certificates, values and clients can name it before the instance exists and across
replacements. In-VPC clients and workers use it (no public-IP hairpin, NFR-T5/T9); the Mac host and the
dev Mac use the Elastic IP. Every certificate the chart issues carries both. `up.sh env` writes
`~/.config/cucina/aws-e2e/values-endpoints.json` (a JSON Helm values file; `jq .chart_endpoints` on the env
outputs):

```json
{"endpoints": {
  "client": {"host": "<k3s_public_ip>", "extraNames": ["<k3s_private_ip>"]},
  "worker": {"host": "<k3s_private_ip>", "extraNames": ["<k3s_public_ip>"]},
  "hosts":  {"host": "<k3s_public_ip>"}}}
```

| Chart value | Source |
| --- | --- |
| `endpoints.client.host` (STS and management default to it) | `k3s_public_ip` |
| `endpoints.client.extraNames` | `[k3s_private_ip]`: certificate SAN for in-VPC clients, which dial `client_endpoint_private` (`<private>:443`; STS `:8443`, management `:8444`) |
| `endpoints.worker.host` | `k3s_private_ip` (workers never use the public address) |
| `endpoints.worker.extraNames`, `endpoints.hosts.host` | `[k3s_public_ip]`, `k3s_public_ip`: Mac hosts connect over the internet |
| `controller.ec2.region` | `us-west-1` |
| `controller.ec2.extraTags` | **all three** `cucina:env=e2e`, `cucina:run`, `cucina:expires`: the controller's IAM policy refuses launches without them |
| pool `subnets` | `[private_subnet_id]` (IPv6 egress); add `public_subnet_id` plus a public IP only as the R-DATA-4 fallback |
| pool `securityGroupIds`, `instanceProfile` | `[sg_workers]`, `worker_instance_profile_name` |
| Windows pool Fast Launch template | `fast_launch_template_id` |
| image repositories | `<ecr_registry>/cucina/controller`, `.../cucina/sts`; the node pulls with its instance profile (a kubelet credential provider is installed), `docker login` for pushing: `aws ecr get-login-password | docker login --username AWS --password-stdin <ecr_registry>` |
| storage class | k3s `local-path`, backed by the 300 GiB gp3 volume mounted at `/var/lib/rancher/k3s/storage` |

## Bring down

```sh
helm uninstall ...                                     # finalizers terminate pool instances (R-OPS-3)
deploy/aws-e2e/scripts/down.sh --delete-amis           # destroy env, then base, then sweep (T15)
```

`down.sh` refuses to continue while tagged **worker** instances exist (tags `cucina:env=e2e` +
`cucina:run`, not `cucina:protected`) unless `--force`, which terminates exactly those and waits.
`--keep-base` stops after the env layer, `--delete-amis` makes the sweep disable Fast Launch and delete
the run's AMIs and snapshots (the campaign deletes AMIs at teardown; Packer rebuilds them). Its exit
status is the sweep's: zero only when nothing tagged is left. On success the generated local files
(outputs, kubeconfig, plans) are removed.

## Network and exposure

```
                    internet
                       |  (dev Mac /32 only)
        +--------------+-----------------------------+   one AZ (us-west-1b or -1c, chosen by
        | public subnet 10.42.0.0/20 (IPv4 + IPv6)   |   offerings: every pool instance type must
        |  k3s node [EIP]  linux-client  windows-client  exist there)
        |  Packer builders                           |
        +--------------------+-----------------------+
                             | private IPv4 (SG references)
        +--------------------+-----------------------+
        | private subnet 10.42.16.0/20 (IPv4 + IPv6) |   no IPv4 default route, no NAT gateway
        |  workers  ----IPv6---> egress-only IGW     |   S3 via the (free) gateway endpoint
        +--------------------------------------------+
```

| Security group | Inbound |
| --- | --- |
| `control-plane` | 443 (remote execution), 8443 (STS), 8444 (management) from the admin `/32` and the `clients` SG; 8981, 8983, 8445, 8446 (worker endpoint, enrollment, host API) from the `workers` SG and the Mac-site `/32` (default: the admin `/32`); 6443 (k3s API) from the admin `/32` only |
| `workers` | Prometheus scrape ranges (9100–9199, 9980–9989) from the `control-plane` SG only |
| `clients` | nothing by default (SSM Session Manager); `client_remote_ports` opens SSH/RDP/WinRM from the admin `/32` |
| `builders` | 22, 5985, 5986, 3389 from the admin `/32` |
| `isolation` | nothing (fault injection; outbound HTTPS only) |

No `0.0.0.0/0` or `::/0` ingress exists anywhere (tests enforce it); egress is open. The endpoint ports
follow `charts/cucina/values.yaml` (`endpoints.*`); change `client_endpoint_ports` / `worker_endpoint_ports`
if the chart changes them.

The k3s node boots on a network interface that was created first and already carries the Elastic IP:
an EIP associated after launch replaces the auto-assigned public IPv4 and breaks the connections opened
in the meantime (the SSM agent's, cloud-init's).

k3s is configured for this network, both found in the pre-flight on real instances:

* **Advertised address.** With `--node-external-ip` alone k3s advertises the Elastic IP as the API server
  address, so the `default/kubernetes` Service DNATs pod traffic to an address pods cannot reach: CoreDNS,
  metrics-server and local-path-provisioner crash-loop and nothing works. The config sets `advertise-address`
  to the private IP and keeps the EIP as `node-external-ip` and `tls-san`.
* **Pod and service CIDRs** are `10.244.0.0/16` and `10.245.0.0/16` (`k3s_cluster_cidr`, `k3s_service_cidr`; the
  layer refuses overlaps with the VPC). k3s' defaults (10.42.0.0/16, 10.43.0.0/16) collide with this VPC's
  `10.42.0.0/16`: flannel's `cni0` would take the subnet gateway's address.

### Cutting a worker off the control plane (T9e, T9f)

`sg_isolation` has no inbound rule and allows outbound HTTPS only (SSM keeps working). Swapping a worker's
security groups to it is an operator action (`ec2:ModifyInstanceAttribute`); the controller role cannot do it
(a plan-time test checks that). **A security-group change only affects new connections**: tracked, established
connections (the worker's long-lived gRPC streams) keep flowing, which was measured here (an established TCP
connection kept acknowledging for the 40 s it was watched after the swap while every new connection timed out). To make the cut
real, also drop the established connections on the worker; `ss -K` does it on Linux and still works because the
SSM path stays open:

```sh
ID=<worker instance id>
B=~/.config/cucina/aws-e2e/base-outputs.json; CP=$(jq -r .k3s_private_ip ~/.config/cucina/aws-e2e/env-outputs.json)
PREV=$(aws ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].SecurityGroups[].GroupId' --output text)

aws ec2 modify-instance-attribute --instance-id "$ID" --groups "$(jq -r .sg_isolation $B)"     # cut: new connections fail
aws ssm send-command --instance-ids "$ID" --document-name AWS-RunShellScript \
  --parameters "commands=[\"ss -K dst $CP\"]"                                                  # drop the tracked ones
sleep 120
aws ec2 modify-instance-attribute --instance-id "$ID" --groups $PREV                           # restore
```

Windows workers have no `ss -K`: block the control-plane address with a host firewall rule for the duration
(`New-NetFirewallRule -Direction Outbound -RemoteAddress <ip> -Action Block`) instead of, or in addition to, the swap.

## IAM

| Principal | Permissions | Notes |
| --- | --- | --- |
| `cucina-e2e-controller` (managed policy) | launch (`RunInstances`, `CreateFleet`, spot) only with the three request tags and only from / into tagged AMIs, snapshots, subnet, security groups and templates; terminate, delete volume / ENI only on `cucina:env=e2e`; `iam:PassRole` for the worker role only; `Describe*`, price list; `ssm:GetParameter*` on `/cucina/e2e/*`; ECR pull; explicit deny for `cucina:protected=true` resources and for any region but us-west-1 | attached to the k3s node role (IMDSv2 hop limit 2, R-CP-8: acceptable only for this temporary cluster); rationale in `docs/adr/0202-e2e-iam-least-privilege.md` |
| `cucina-e2e-controller-images` (managed policy) | deregister tagged AMIs, delete tagged snapshots, `Enable/DisableFastLaunch` on tagged AMIs and on the prep launch template, and the launch dry run EnableFastLaunch runs from that template | also on the k3s node role; verified against real EC2 |
| `cucina-e2e-worker` (role + profile) | `AmazonSSMManagedInstanceCore`; read of `/cucina/e2e/workers/*`; **no EC2** (R-POOL-3) | the managed policy's `ssm:GetParameter` on `*` is denied outside the prefix |
| `cucina-e2e-k3s-node` | SSM core, controller policy, ECR pull | |
| `cucina-e2e-client` | SSM core; read of `/cucina/e2e/clients/*` | also usable by Packer builders |

The account's `AWSServiceRoleForEC2FastLaunch` service-linked role is created by AWS the first time
Fast Launch is enabled and is left in place at teardown (§12). It already exists here.

## State, secrets, hygiene

* OpenTofu state: `~/.config/cucina/aws-e2e/<layer>/terraform.tfstate` (`backend "local"`, path from
  `var.state_dir`, evaluated at `tofu init`, so a repo-local state file cannot be created by
  accident). `TF_DATA_DIR` is `~/.config/cucina/tf-data/<layer>`.
* Never in the repo: state, plans, outputs, kubeconfig, account IDs, IPs, resource IDs, admin CIDR.
  The tests scan for them; `*.tfstate`, `*.tfvars` and `.terraform/` are gitignored.
* The kubeconfig carries cluster-admin credentials and dies with the cluster. SSM keeps the output
  of Run Command invocations (visible to principals that may call `ssm:GetCommandInvocation`);
  acceptable for a temporary, single-user account.

## Runaway guards

* **k3s node and clients**: `shutdown -h +480` armed at boot (and re-armed at every boot by
  `cucina-guard.service`; Windows: a startup task running `shutdown.exe /s /t 28800`) with
  `InstanceInitiatedShutdownBehavior=stop`: after 8 hours the instances stop, EBS keeps costing
  about $0.13/h for everything. Extend a session with `sudo cucina-guard 240` (Linux) or
  `C:\ProgramData\Cucina\guard.ps1 240` (Windows), or shorten with `guard_minutes`.
* **Workers**: dead-man switch (R-POOL-7, `InstanceInitiatedShutdownBehavior=terminate`).
* Every resource carries `cucina:expires`; `sweep.sh` finds anything past it by tag, and the
  campaign ends with `down.sh`.

## Cost (us-west-1 list prices, 2026-10-02)

Standing cost with nothing running:

| Item | Monthly |
| --- | --- |
| VPC, subnets, route tables, IGW, egress-only IGW, S3 gateway endpoint, security groups, IAM, launch template | $0 |
| ECR (`cucina/*`, lifecycle keeps the last 10 images) | $0.10 per GB-month (cents) |
| Public IPv4 / EIP, NAT gateway, interface endpoints | none exist in `base` |

While `env` is up (hourly): k3s `m8i.2xlarge` $0.494 + its volumes (30 GiB root, 300 GiB gp3 at 6000 IOPS /
500 MiB/s = $0.093) + EIP $0.005; `linux-client` `m7i.xlarge` $0.235 + 100 GiB $0.013 + public IPv4 $0.005;
`windows-client` `m7i.xlarge` Windows $0.419 + 150 GiB $0.020 + public IPv4 $0.005. About **$1.29 per
hour, $10.3 per 8-hour guard window**. Workers are priced by the controller (R-OBS-5); for scale, 4
`c8i.8xlarge` Linux workers are $7.5 per hour.

## The tag sweep

`scripts/sweep.sh` inventories everything tagged `cucina:env=e2e` **and** `cucina:run=$CUCINA_RUN_ID`:
instances, volumes, snapshots, AMIs, ENIs, EIPs, security groups, subnets, route tables, internet and
egress-only gateways, VPC endpoints, NAT gateways (must never exist), VPCs, launch templates, key pairs,
IAM roles / policies / instance profiles, ECR repositories, SSM parameters, Secrets Manager secrets,
and, through the Resource Groups Tagging API (us-west-1, and us-east-1 for IAM), anything else that
carries the tags (log groups, S3 buckets, ...). It also lists resources tagged `CreatedBy=EC2 Fast Launch`
(snapshots, prep instances, volumes, launch templates), attributed to the run when they carry its tags,
mention one of its AMIs ("This is Fast Launch snapshot for image ami-...") or were created from one of its
launch templates (tag `CreatedByLaunchTemplateId`), and flagged `unattributed` otherwise; both count as leftovers.

```sh
deploy/aws-e2e/scripts/sweep.sh                  # report; exit 0 clean, 1 leftovers, 2 incomplete (API error)
deploy/aws-e2e/scripts/sweep.sh --delete-amis    # Fast Launch off (waits until reported disabled), deregister, delete snapshots, report
deploy/aws-e2e/scripts/sweep.sh --selftest       # query-only demonstration that the tag filters filter
```

Only AMIs (and their snapshots, and only when those carry both tags) are ever deleted by the sweep; the
rest is removed by `tofu destroy` and the controller's finalizers. Order at teardown: disable Fast
Launch, then deregister the AMI, then delete its snapshots; `--delete-amis` does exactly that per AMI.

## Tests

`deploy/aws-e2e/tests/run.sh` (about 15 s, offline, no credentials): `tofu fmt -check`, `tofu validate`,
`tflint`, `tofu test` with `mock_provider` and `command = plan` for both layers, a stubbed-curl test of the
kubelet ECR credential provider, `shellcheck`, and structural checks (no NAT gateway, exactly one EIP, no
world-open ingress, tags on every resource, hygiene, SPDX headers, identical provider locks). What
`tofu test` asserts: the three tags on every resource, no `0.0.0.0/0` / `::/0` ingress, no NAT route, IMDSv2
on every instance, k3s hop limit 2 and the data volume sizing, the 8-hour guard, pinned SHA-256s, the worker
role has no `ec2:` action, the controller policies' tag conditions, `PassRole` scope and size limits, the Fast Launch prep
template (instances and volumes only: tagging ENIs makes Fast Launch fail), k3s advertising its private
address, non-overlapping pod / service CIDRs. The real apply / destroy is the integration test; there is no
Terratest.

## Troubleshooting

* **Nothing registers in SSM**: for a worker in the private subnet see ADR 0201; for the k3s node and clients
  check the instance profile and `aws ssm describe-instance-information`.
* **k3s bootstrap**: `/var/log/cucina-bootstrap.log` on the node (Windows client:
  `C:\ProgramData\Cucina\bootstrap.log`), readable over SSM Session Manager or Run Command. `kubeconfig.sh`
  waits for `/var/lib/cucina/bootstrap.done`.
* **AMI inputs are plain variables** (`windows_client_ami`, `linux_client_ami`, `k3s_ami`; `up.sh` fills the
  first from `amis.json`). `ami` is ignored once an instance exists, so a rebuilt image or a new Ubuntu
  build never changes a plan mid-campaign; replace on purpose with `up.sh --yes env -- -replace=aws_instance.k3s`
  (or `aws_instance.linux_client`, `aws_instance.windows_client[0]`). Editing user data replaces the instance
  too (`user_data_replace_on_change`).
* **SSM Run Command has no `$HOME`**: `export HOME=/root` before Bazelisk or any tool that wants a cache
  directory (a session as `ssm-user` has one).
* **Spot**: not used in the campaign; it would need `AWSServiceRoleForEC2Spot`, which exists here.

## Measured on real instances (pre-flight, 2026-10-02)

Ubuntu 26.04.1 (kernel 7.0 AWS flavour) and the `windows-base` AMI, launched with the user data of the `env`
layer, then terminated. The k3s node: SSM Online about 30 s after launch, bootstrap done about 45 s after boot
(k3s v1.36.5+k3s1 `Ready`; data volume mounted as `ext4` at `/var/lib/rancher/k3s/storage`; all three
kube-system pods Running within a minute after the two k3s fixes above); `kubeconfig.sh` end to end in about
a minute and `kubectl` from the dev Mac works over the Elastic IP; a pod pulled a private ECR image through the
kubelet credential provider. `linux-client`: Bazelisk 1.29.0 fetched Bazel 9.2.0. `windows-client`: SSM Online
about 40 s after boot, bootstrap done within 90 s; `TMP` / `TEMP` are `C:\bb\tmp` at machine scope and in the
environment SSM commands see (the agent is restarted at the end of the bootstrap), `BAZEL_SH` points at Git's
bash. The 8-hour guard was exercised with shortened timers: all three instance types stopped themselves.
