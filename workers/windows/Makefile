# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Windows worker images (R-POOL-5). Usage (from the repo root, after `source .work/env.sh` or equivalent):
#   make -C workers/windows image-windows STAGE=base      # WS2025 + VS Build Tools + SDK + WinFSP + shawl + Git
#   make -C workers/windows image-windows STAGE=worker    # + Buildbarn services, sysprep (+ Fast Launch if FAST_LAUNCH=1)
#   make -C workers/windows verify-bazel                  # rules_cc/Bazel MSVC autodetection on the newest base AMI
#   make -C workers/windows validate                      # packer fmt/validate, shellcheck, PSScriptAnalyzer
# Network/SG/admin CIDR come from the e2e environment outputs (never committed).

SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

HERE          := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
REGION        ?= us-west-1
STAGE         ?= base
BUILDER_TYPE  ?= m7i.large
OUTPUTS       ?= $(HOME)/.config/cucina/aws-e2e/base-outputs.json
AMIS_FILE     ?= $(HOME)/.config/cucina/aws-e2e/amis.json
ARTIFACTS     ?= $(CUCINA_DEV_STORAGE)/images/windows
# Simply expanded once per make run (a recursive $(shell date) would change between recipe lines).
ifeq ($(origin IMAGE_VERSION),undefined)
IMAGE_VERSION := $(shell date -u +%Y%m%d.%H%M)
endif
FAST_LAUNCH   ?= 0
FAST_LAUNCH_COUNT ?= 4
DEFENDER_MODE ?= exclusions
# cucina-worker-agent (windows/amd64; `make agent` builds it); a missing file means "image without agent".
WORKER_AGENT  ?= $(CUCINA_DEV_STORAGE)/images/agent/cucina-worker-agent-windows-amd64.exe
SYSPREP_GATE  ?=
PACKER        ?= packer
SHELLCHECK    ?= mise exec shellcheck@0.11.0 -- shellcheck
PWSH          ?= mise exec aqua:PowerShell/PowerShell@7.6.6 -- pwsh
LINUX_SCRIPTS := $(HERE)/../linux/scripts

.PHONY: help image-windows vars agent verify-bazel validate fmt fast-launch-enable fast-launch-disable

help:
	@sed -n '3,8p' $(HERE)/Makefile

$(ARTIFACTS):
	@mkdir -p $@

# Generated per build from the environment outputs: network, builder SG, admin /32, Fast Launch template.
vars: | $(ARTIFACTS)
	@test -s "$(OUTPUTS)" || { echo "missing $(OUTPUTS) (aws-e2e base not applied yet)" >&2; exit 1; }
	@jq '{vpc_id, subnet_id: .public_subnet_id, security_group_ids: [.sg_builders], admin_cidrs: [.admin_cidr], fast_launch_template_id: (.fast_launch_template_id // "")}' \
	  "$(OUTPUTS)" > "$(ARTIFACTS)/env.pkrvars.json"
	@chmod 600 "$(ARTIFACTS)/env.pkrvars.json"

image-windows: vars
	@case "$(STAGE)" in base|worker) ;; *) echo "STAGE must be base or worker" >&2; exit 2;; esac
	@# Packer's WinRM upload fails on large files: ship the agent zipped and split into ~1 MB parts.
	@rm -rf "$(ARTIFACTS)/agent-parts"; mkdir -p "$(ARTIFACTS)/agent-parts"; if [ -f "$(WORKER_AGENT)" ]; then \
	  cp "$(WORKER_AGENT)" "$(ARTIFACTS)/agent-parts/cucina-worker-agent.exe" && cd "$(ARTIFACTS)/agent-parts" && \
	  zip -q -9 cucina-worker-agent.zip cucina-worker-agent.exe && split -b 1000k cucina-worker-agent.zip cucina-worker-agent.zip.part- && \
	  rm cucina-worker-agent.exe cucina-worker-agent.zip; fi
	@gen=$${GENERATION:-$$($(LINUX_SCRIPTS)/next-generation.sh windows-$(STAGE) $(REGION))}; \
	base_ami=""; if [ "$(STAGE)" = worker ]; then base_ami=$$(jq -er '."windows-base".ami_id' "$(AMIS_FILE)"); fi; \
	echo "building windows-$(STAGE) image $(IMAGE_VERSION) (generation $$gen)"; \
	cd $(HERE)/packer && $(PACKER) init . >/dev/null && \
	CUCINA_IMAGES_ARTIFACTS="$(ARTIFACTS)" $(PACKER) build -timestamp-ui -only='$(STAGE).*' \
	  -var-file="$(ARTIFACTS)/env.pkrvars.json" -var base_instance_type="$(BUILDER_TYPE)" -var worker_instance_type="$(BUILDER_TYPE)" \
	  -var image_version=$(IMAGE_VERSION) -var generation=$$gen -var amis_file="$(AMIS_FILE)" -var base_ami="$$base_ami" \
	  -var defender_mode=$(DEFENDER_MODE) -var worker_agent_path="$(ARTIFACTS)/agent-parts" -var sysprep_gate_file="$(SYSPREP_GATE)" \
	  -var enable_fast_launch=$$([ "$(FAST_LAUNCH)" = 1 ] && echo true || echo false) \
	  -var fast_launch_target_count=$(FAST_LAUNCH_COUNT) . 2>&1 | tee "$(ARTIFACTS)/windows-$(STAGE)-$(IMAGE_VERSION).packer.log"
	@if [ "$(STAGE)" = base ]; then \
	  sed -n 's/.*CUCINA_INVENTORY:[0-9]*:\([A-Za-z0-9+/=]*\).*/\1/p' "$(ARTIFACTS)/windows-base-$(IMAGE_VERSION).packer.log" | tr -d '\n' | \
	    base64 -d > "$(ARTIFACTS)/windows-base-$(IMAGE_VERSION).json" && \
	  $(HERE)/scripts/update-versions.sh "$(ARTIFACTS)/windows-base-$(IMAGE_VERSION).json"; fi

agent:
	@mkdir -p "$(dir $(WORKER_AGENT))"
	cd $(HERE)/../.. && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(IMAGE_VERSION)" \
	  -o "$(WORKER_AGENT)" ./cmd/cucina-worker-agent

verify-bazel:
	$(HERE)/scripts/verify-bazel.sh

fast-launch-enable:
	$(HERE)/scripts/fast-launch.sh enable --ami "$(AMI)" --count $(FAST_LAUNCH_COUNT) --wait

fast-launch-disable:
	$(HERE)/scripts/fast-launch.sh disable --ami "$(AMI)" --wait

fmt:
	$(PACKER) fmt $(HERE)/packer

# Static checks only (no AWS calls).
validate: | $(ARTIFACTS)
	$(PACKER) fmt -check -diff $(HERE)/packer
	@echo '{"vpc_id":"vpc-0","subnet_id":"subnet-0","security_group_ids":["sg-0"],"admin_cidrs":["192.0.2.1/32"]}' > "$(ARTIFACTS)/validate.pkrvars.json"
	cd $(HERE)/packer && $(PACKER) init . >/dev/null && CUCINA_IMAGES_ARTIFACTS="$(ARTIFACTS)" $(PACKER) validate \
	  -var-file="$(ARTIFACTS)/validate.pkrvars.json" -var image_version=0.0.0 -var generation=0 .
	$(SHELLCHECK) $(HERE)/scripts/*.sh
	$(PWSH) -NoProfile -File $(HERE)/scripts/lint.ps1 -Path $(HERE)
