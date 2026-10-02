<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# Runbook: game days with AWS FIS

**Use when** you want evidence that the controller behaves under **real** AWS capacity errors and API throttling, not only under the simulated ones. AWS Fault Injection Service (FIS) can make the EC2 API return `InsufficientInstanceCapacity` and throttling errors to a specific IAM role, which is exactly the
controller's role. Run a game day before releases that change the EC2 provider or the scaling policy, and on a regular schedule in staging. It is not part of every campaign, and it never runs in production. **Time:** an hour, including preparation.

## What it proves

The simulation tier already proves the decisions deterministically (the `ice` and `throttling` scenarios in `sim/scenarios/`). A game day proves the **wiring**: that the real EC2 errors are classified the way the controller expects, that backoff and the ordered instance-type and subnet walk work against the real API, that the fail-fast
path produces a clear message, and that alerts fire. If a game day disagrees with the simulation, fix the fake (`internal/fakes`) so that it behaves like the real thing, and add the case to the conformance suite.

## Safety

* Use a **staging** deployment in a **non-production AWS account or region**, with a small `max` on the pools under test and a `dailyInstanceHourCap` as a spend ceiling.
* The faults target only the **controller's IAM role** (the identity that calls `RunInstances`, `CreateFleet`, `DescribeInstances` and `TerminateInstances`: the Pod Identity or IRSA role on EKS, or the node's role on a k3s test cluster). Nothing else in the account is affected.
* Every experiment has a **duration** (at most 15 minutes) and a stop condition. Know how to stop it (below) before you start it.
* Announce it, and have someone watching the dashboards. Agree beforehand who aborts.
* FIS bills per action-minute; it is cheap, but check the current pricing.

## Preparation

1. **An FIS experiment role.** Create the IAM role that FIS assumes to run the experiment, as described in the FIS documentation for the two actions below (`aws:ec2:api-insufficient-instance-capacity-error` and `aws:fis:inject-api-throttle-error`). Scope it to this account and the controller's role.
2. **Find the controller role** and keep its ARN. With IRSA it is the role annotation on the service account (`kubectl -n cucina get sa cucina-controller -o jsonpath='{.metadata.annotations}'`). With EKS Pod Identity there is no annotation: look up the association instead
   (`aws eks list-pod-identity-associations --cluster-name <cluster> --namespace cucina --service-account cucina-controller --region $REGION --query 'associations[].associationId' --output text`, then
   `aws eks describe-pod-identity-association --cluster-name <cluster> --association-id <id> --region $REGION --query association.roleArn --output text`). On a test k3s cluster it is the node's instance-profile role.
3. **A stop condition (recommended).** A CloudWatch alarm on something that means "too much": the number of instances in the cluster tag, or `cucina_invariant_violations_total` above zero. Use `{"source": "none"}` only for short experiments you are actively watching.
4. **Prepare the observation.** Open the Cucina overview dashboard, and in a terminal: `watch -n 10 'cucinactl pools describe <pool>'`.

## Experiment 1: insufficient capacity

The action makes a percentage of provisioning attempts fail with an insufficient-capacity error for the target role, in the Availability Zones you name (`all` or a comma-separated list of AZ IDs such as `usw1-az1`). Replace the placeholders:

```json
{
  "description": "Cucina game day: insufficient instance capacity for the controller role",
  "targets": {
    "ControllerRole": {
      "resourceType": "aws:iam:role",
      "resourceArns": ["arn:aws:iam::123456789012:role/<controller role>"],
      "selectionMode": "ALL"
    }
  },
  "actions": {
    "ice": {
      "actionId": "aws:ec2:api-insufficient-instance-capacity-error",
      "parameters": {"availabilityZoneIdentifiers": "all", "duration": "PT15M", "percentage": "100"},
      "targets": {"Roles": "ControllerRole"}
    }
  },
  "stopConditions": [{"source": "none"}],
  "roleArn": "arn:aws:iam::123456789012:role/<fis experiment role>",
  "tags": {"cucina:env": "gameday"}
}
```

```sh
aws fis create-experiment-template --cli-input-json file://ice.json --region $REGION --query experimentTemplate.id --output text
aws fis start-experiment --experiment-template-id <template id> --region $REGION --query experiment.id --output text
aws fis get-experiment --id <experiment id> --region $REGION --query experiment.state.status --output text
```

While it runs, **queue work for a pool that is at zero** (a small remote build on that platform) so that the controller tries to scale out from zero. Then watch for the behaviour in the checklist below. Use `percentage: 50` and a single AZ to test the walk to the next subnet.

## Experiment 2: API throttling

Same shape, different action: the target role receives `RequestLimitExceeded`-style throttling for the EC2 operations you list.

```json
"actions": {
  "throttle": {
    "actionId": "aws:fis:inject-api-throttle-error",
    "parameters": {"service": "ec2", "operations": "RunInstances,DescribeInstances,TerminateInstances", "percentage": "50", "duration": "PT10M"},
    "targets": {"Roles": "ControllerRole"}
  }
}
```

Run it with a burst of queued work so that many launches are wanted at once.

## Expected behaviour

| Observation | Where | Expected |
| --- | --- | --- |
| Errors are classified | `cucina_ec2_capacity_errors_total{pool,type,kind}`, `cucina_ec2_api_errors_total{op,code}` | Capacity errors count as `ice`; throttling as `RequestLimitExceeded`, not as hard failures |
| No hot loop | Controller log, API call rate | Jittered backoff; the call rate stays bounded by the token buckets |
| The walk to alternatives | `cucinactl pools describe <pool>` events; controller log | The pool tries its next instance type and subnet; ICE'd types move to the end of the list |
| Fail-fast | `cucinactl ops list --stage queued`; client output | After `controller.scheduler.queueFailAfter` (10 minutes by default) with queued work, no registered worker and capacity errors, the queue is failed with a message that names the cause, not left to hang; the capacity-errors alert fires after 15 minutes of errors, so run experiment 1 for the full 15 minutes to see both |
| No leaks, no duplicates | `aws ec2 describe-instances` filtered by the cluster tag; `cucina_invariant_violations_total` | No instance without tags, no duplicate launch for one token, no orphans; the violations counter stays at zero |
| Recovery | After the experiment ends | Launches resume by themselves; queued work completes; the pool returns to zero when idle |

## Stop it

```sh
aws fis stop-experiment --id <experiment id> --region $REGION
```

An experiment also ends by itself at its duration. If anything unexpected happens (instances multiplying, errors you do not understand), stop it first and investigate afterwards.

## Afterwards

1. Confirm the experiment state is `completed` or `stopped`, then delete the template (`aws fis delete-experiment-template --id <template id>`). Delete the temporary role if you made one for this.
2. Run the checks of [cost leak](cost-leak.md): the pool is at zero, no orphaned volumes or interfaces.
3. Record the result next to the date and the commit under test: what you expected, what happened, how long recovery took, which alerts fired. Anything that differs from the simulation becomes a bug and a regression scenario in `sim/scenarios/`, or a fix to the fake.

Network faults between hostd and the controller are exercised by the integration tier with an in-memory fault network (`internal/hostlink/hostlinktest`: down, reset peer, blackhole); use Chaos Mesh for further network game days only, and only in staging.
