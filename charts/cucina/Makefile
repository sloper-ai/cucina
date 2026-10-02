# SPDX-License-Identifier: FSL-1.1-ALv2
# Chart checks without Bazel (`bazel test //charts/cucina/...` runs the same ones with
# pinned tools). Tools: helm (Helm 4) on PATH; optional helm3, ct, promtool, sloth,
# kubeconform under $(TOOLS) or on PATH; pinned Buildbarn binaries in
# $(CUCINA_DEV_STORAGE)/bb-release/<tag>/ for the boot checks.
SHELL := /bin/bash
REPO := $(abspath ../..)
CUCINA_DEV_STORAGE ?= /Volumes/Code/Sources/.dev-storage/cucina
TOOLS ?= $(CUCINA_DEV_STORAGE)/tools/chart/bin
export PATH := $(TOOLS):$(PATH)
export CUCINA_DEV_STORAGE
export HELM_PLUGINS ?= $(CUCINA_DEV_STORAGE)/helm-plugins
HELM3 ?= $(wildcard $(TOOLS)/helm3)

.PHONY: test lint unittest static boot ct slo-check sync slo update

## test: every chart check (lint, unittest, schema/kubeconform/promtool/sync, config-render boot, ct)
test: lint unittest static boot ct slo-check

## lint: helm lint --strict (Helm 4 and, if available, Helm 3) + helm package
lint:
	HELM3=$(HELM3) tests/lint_test.sh

## unittest: helm-unittest suites (conditionals and defaults that matter)
unittest:
	tests/unittest_test.sh

## static: copies/schema/README freshness, Go-contract authz cross-check, schema fail-fast
## cases, kubeconform -strict, promtool rule tests
static:
	cd $(REPO) && go test ./charts/cucina/tests/static -count=1

## boot: render every profile and boot the pinned Buildbarn binaries (R-CP-2), and the
## rendered control plane's behaviour (auth, AC isolation, queues at zero)
boot:
	cd $(REPO) && go test ./charts/cucina/tests/boot -count=1

## ct: chart-testing lint over the ci/ profiles
ct:
	tests/ct_test.sh

## slo-check: files/rules/slo.yaml is what Sloth generates from files/slo/
slo-check:
	@if command -v sloth >/dev/null; then \
	  tmp=$$(mktemp -d); sloth generate -i files/slo/cucina.yaml -o $$tmp/slo.yaml >/dev/null 2>&1 && \
	  diff -u files/rules/slo.yaml $$tmp/slo.yaml && echo "ok: slo rules up to date"; rc=$$?; rm -rf $$tmp; exit $$rc; \
	else echo "SKIP: sloth not found"; fi

## sync: refresh the copies of platforms/pools.json and api/crds, then the generated
## schema definitions and README values table
sync:
	cp $(REPO)/platforms/pools.json files/platforms.json
	cp $(REPO)/api/crds/*.yaml files/crds/
	cd $(REPO) && go test ./charts/cucina/tests/static -run 'TestSchemaDefinitionsMatchCRDs|TestReadmeValuesTableUpToDate' -count=1 -update

## slo: regenerate files/rules/slo.yaml with Sloth v0.16.0
slo:
	sloth generate -i files/slo/cucina.yaml -o files/rules/slo.yaml
