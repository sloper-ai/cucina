<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Running Cucina on Amazon EKS

The chart is EKS-ready (R-CP-8); this guide is documentation only — the acceptance campaign runs on k3s (§10), and
MT-007 ("fresh production install on EKS using only the README") is the manual check. The example values are
[`charts/cucina/ci/values-large.yaml`](../../charts/cucina/ci/values-large.yaml).

## Topology (R-DATA-4)

* One AZ for nodes, the storage volumes, the internal NLB and the worker subnets: in-AZ traffic is free and EBS volumes
  are zonal. Workers in private subnets of a dual-stack VPC with an egress-only internet gateway and an S3 gateway
  endpoint; no NAT gateway below the documented break-even.
* Node group sized for the control plane only (workers are EC2 instances the controller launches, not pods); the
  `large` profile fits on 3 × m7i.2xlarge, `medium` on 2 × m7i.xlarge (see [sizing](../sizing.md)).

## AWS credentials for the controller

The controller (and only the controller) calls AWS. Give its ServiceAccount (`<release>-controller`) a role with
**IRSA**:

```yaml
controller:
  serviceAccount:
    annotations:
      eks.amazonaws.com/role-arn: arn:aws:iam::<account-id>:role/cucina-controller
  aws:
    enabled: true
    region: us-west-2
    accountId: "<account-id>"     # set at install time; never commit it
```

or with **EKS Pod Identity**: create an association for namespace `cucina`, ServiceAccount `cucina-controller` and the
same role (no annotation needed). The k3s test cluster instead uses the node's instance profile with IMDSv2 and a hop
limit of 2 so pods can reach IMDS — acceptable only for that temporary single-tenant cluster: every pod on the node
gets the role.

Role policy (least privilege; tag conditions keep the controller to its own resources):

| Statement | Actions | Condition / resource |
| --- | --- | --- |
| Launch | `ec2:RunInstances`, `ec2:CreateFleet`, `ec2:CreateTags` | `aws:RequestTag/cucina:managed-by = cucina-controller`, `aws:RequestTag/cucina:cluster = <clusterId>`; `ec2:CreateAction` ∈ {RunInstances, CreateFleet} for CreateTags; subnets, security groups, AMIs, launch templates of the pools |
| Pass the worker role | `iam:PassRole` | the worker instance-profile role only |
| Terminate / clean up | `ec2:TerminateInstances`, `ec2:DeleteVolume`, `ec2:DeleteNetworkInterface` | `aws:ResourceTag/cucina:cluster = <clusterId>` |
| Observe | `ec2:Describe*` (instances, images, volumes, network interfaces, instance types, subnets, fast-launch images) | `*` (no resource-level permissions) |
| Windows Fast Launch | `ec2:EnableFastLaunch`, `ec2:DisableFastLaunch`, `ec2:DescribeFastLaunchImages`, launch-template use | Windows AMIs of the pools |
| Worker logs (management API, docs/dev/mgmt.md) | `ssm:SendCommand` | documents `AWS-RunShellScript`, `AWS-RunPowerShellScript`; instances with `ssm:resourceTag/cucina:managed-by = cucina-controller` and `ssm:resourceTag/cucina:cluster = <clusterId>` |
| | `ssm:GetCommandInvocation` | `*` |
| Cost | `pricing:GetProducts` | `*` |

Workers get no EC2 permissions: `AmazonSSMManagedInstanceCore` and, at most, read access to their pool's parameters.

## Storage: EBS CSI with raw block volumes

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: ebs-gp3-block}
provisioner: ebs.csi.aws.com
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
parameters: {type: gp3, iops: "6000", throughput: "500", encrypted: "true"}
```

```yaml
storage:
  mode: block                    # raw Block PVC for the CAS blocks + a small filesystem PVC (ADR 0402)
  storageClassName: ebs-gp3-block
  supplementalGroups: [6]        # group of the block device node (disk) so the non-root container can open it
```

`WaitForFirstConsumer` places each shard's volumes in its pod's AZ. The CAS device size is
`storage.stores.cas.size`; bb_storage sizes its blocks from the device. Verify that the device node in the container is
group-readable by the supplemental group (it depends on the node OS); otherwise run the storage container as root.

## Load balancers

Install the AWS Load Balancer Controller and use NLBs (`loadBalancerClass: service.k8s.aws/nlb`): internet-facing (or
internal, for VPN-only access) for `exposure.client`/`exposure.api`, **internal** with cross-zone load balancing off and
`externalTrafficPolicy: Local` for `exposure.worker`. Each Service is its own NLB; put their DNS names in
`endpoints.worker.storageHost`, `schedulerHost`, `enrollmentHost` and `endpoints.client.host`. Details and the idle-timeout
rule (> 2 min): [exposure.md](exposure.md).

## TLS

Production default is Cucina's private CA (§13). With a public client name, `tls.public.source: certManager` and an ACME
`ClusterIssuer` give Bazel clients a publicly trusted certificate; keep `tls.internal` on Cucina's CA (workers trust it).
Or bring Secrets (`existingSecret`). See ADR 0403.

## Monitoring

With kube-prometheus-stack: `monitoring.serviceMonitors.enabled`, `workerScrapeConfig.enabled` (EC2 workers through the
controller's HTTP service discovery; allow the workers' metrics port from the cluster security group),
`prometheusRules.enabled`, `grafanaDashboards.enabled`, and `monitoring.labels.release: <prometheus release>`.
