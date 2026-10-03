<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0203 — Tag client ENIs at launch without replacing campaign clients

* Status: accepted (2026-10-02)

## Context

The env layer's Linux and Windows clients set instance and volume tags, but their implicit
primary ENIs did not inherit `cucina:env`, `cucina:run` or `cucina:expires` (the §12 ownership
invariant). The k3s node already uses an explicitly tagged ENI.

Pinned AWS provider 6.67.0 has no `aws_instance` ENI-tag argument: its create path sends
instance/volume tag specifications and a separate launch-template reference. The
[`launch_template` block is ForceNew][provider], so simply adding it would replace existing
campaign clients. That replacement is not acceptable for this fix.

EC2's [launch-template precedence documentation][precedence] says nested parameters combine
with the template's complex structure; its tag example retains other template tags and only
overrides matching keys. The clients' explicit instance/volume tag specifications therefore
do not erase a template's [`network-interface` tag specification][tag-spec]. This is documented
launch-time behavior, not something the offline mock provider proves.

## Decision

* Add one **env-only**, tag-only `aws_launch_template.client_network_tags`. Its sole launch-time
  setting is a `network-interface` tag specification with `merge(local.tags, local.infra_tags)`.
  Tag the template itself with the same ownership and `cucina:protected=true` tags; provider
  default tags on a template alone do not tag its children.
* Both clients reference that template's ID and explicit numeric `latest_version`, converted
  to a string. Keep AMIs, networking, metadata, user data, instance tags and volume tags on the
  existing instance resources.
* Add **only** `launch_template` to each client's existing `ignore_changes = [ami]`.
  [Ignored arguments still apply on creation][ignore], but adding or changing this reference
  must not replace current clients. Other update/replacement behavior is unchanged. A later,
  separately intended creation or replacement uses the current declared template version.
* Leave k3s and the base Fast Launch prep template unchanged. Fast Launch's service-linked role
  cannot tag ENIs (ADR 0202); this client template must not be reused for prep instances.
* Do not introduce `aws_ec2_tag`: destroying such a dependent tag resource can remove ownership
  markers before its parent instance terminates.

## Consequences

This is **future-launch-only**, not ENI reconciliation. It neither backfills current ENIs nor
propagates subsequent expiry or ownership-tag edits to existing ENIs. Treat campaign tags as
immutable for a client's lifetime: changing instance/volume tags or the template alone can
leave its ENI with old values. Any extension or repair requires separately authorized,
verified reconciliation; never hide that gap by ignoring instance or volume tags.

The current campaign's two client ENIs were repaired separately and additively after checking
the tagged parent, primary attachment, account and tag conflicts. This source fix does not
repeat that repair or imply the template has been deployed. No current-client replacement
is allowed as part of deploying this fix.

The existing `tags_on_every_resource` mock-plan test now covers the template, its sole ENI tag
specification, protection tags and both clients' ID/numeric-version references. The env suite
passes all 12 runs with the pinned provider schema, and native validation succeeds. No live
plan or apply was performed; an authorized deployment still needs a reviewed live plan that
shows no client replacement.

[provider]: https://github.com/hashicorp/terraform-provider-aws/blob/v6.67.0/internal/service/ec2/ec2_instance.go
[precedence]: https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/launch-instances-from-launch-template.html
[tag-spec]: https://docs.aws.amazon.com/AWSEC2/latest/APIReference/API_LaunchTemplateTagSpecificationRequest.html
[ignore]: https://opentofu.org/docs/language/resources/behavior/#lifecycle-customizations
