<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0406 — CRDs in crds/, upgraded by a `crds apply` hook

* Status: accepted (2026-10-02)

## Context
R-OPS-1 wants `helm upgrade` to upgrade the CRDs. The chart first rendered them as templates for that reason. A fresh
`helm install` on kind then failed with "no matches for kind TrustPolicy… ensure CRDs are installed first". Helm maps
every object of the release, including the break-glass TrustPolicy, before it creates any of them, and the CRD did not
exist yet. Helm's `crds/` directory is installed first but never upgraded or deleted.

## Decision
`charts/cucina/crds/` holds byte-identical copies of `api/crds/*.yaml` (`make sync`; a static test compares them).
`helm install` creates them before anything else.

A pre-install/pre-upgrade hook Job runs `cucina-controller crds apply`:
* It server-side applies the CRDs embedded in the binary (`api/crds.FS`) as field manager `cucina-controller`, forcing
  ownership over Helm's fields.
* It then waits until every CRD is Established.
* Weights: the hook's RBAC is -10, the Job -8, the bootstrap Job -5.
* A hook ClusterRole grants get/create/patch on exactly those CRD names (`controller.CRDApplyRules`; a static test
  compares it with the rendered rules, and an envtest test runs the apply under it).

`crds.install=false` drops the hook and its RBAC for clusters where the CRDs are managed elsewhere (`--skip-crds`; the
CRDs must then exist before `helm install`, since Helm maps every object before any hook runs). `crds.keep` is gone:
neither `crds/` nor the hook ever deletes a CRD.

## Consequences
The image's embedded CRDs, not the chart's copy, decide the result of an upgrade. Both come from `api/crds` at the same
commit; overriding `images.controller.tag` with a different version applies that version's CRDs.

A rollback runs no hook, so CRDs only move forward and their changes must stay additive.

A new CRD kind whose objects ship in the same chart version needs two steps, because Helm maps objects before
pre-upgrade hooks run. Either install the CRD first (`kubectl apply -f crds/`) or release the kind one version before
its objects.
