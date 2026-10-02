# SPDX-License-Identifier: FSL-1.1-ALv2
#
# Linux worker images (R-POOL-4). Usage (from the repo root, after `source .work/env.sh` or equivalent):
#   make -C workers/linux image-linux ARCH=x86_64 VARIANT=ubuntu     # also: ARCH=arm64, VARIANT=al2023
#   make -C workers/linux measure-boot ARCH=x86_64 VARIANT=ubuntu N=3
#   make -C workers/linux validate                                    # packer fmt/validate + shellcheck
# Network/SG/admin CIDR come from the e2e environment outputs (never committed).

SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

HERE          := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
REGION        ?= us-west-1
ARCH          ?= x86_64
VARIANT       ?= ubuntu
OUTPUTS       ?= $(HOME)/.config/cucina/aws-e2e/base-outputs.json
AMIS_FILE     ?= $(HOME)/.config/cucina/aws-e2e/amis.json
ARTIFACTS     ?= $(CUCINA_DEV_STORAGE)/images/linux
# Simply expanded once per make run (a recursive $(shell date) would change between recipe lines).
ifeq ($(origin IMAGE_VERSION),undefined)
IMAGE_VERSION := $(shell date -u +%Y%m%d.%H%M)
endif
GOARCH        := $(if $(filter arm64,$(ARCH)),arm64,amd64)
# cucina-worker-agent for this architecture (`make agent` builds it); a missing file means "image without agent".
WORKER_AGENT  ?= $(CUCINA_DEV_STORAGE)/images/agent/cucina-worker-agent-linux-$(GOARCH)
N             ?= 3
PACKER        ?= packer
SHELLCHECK    ?= mise exec shellcheck@0.11.0 -- shellcheck
FAMILY        := linux-$(VARIANT)-$(ARCH)

.PHONY: help vars smoke-binaries agent image-linux measure-boot validate fmt

help:
	@sed -n '3,7p' $(HERE)/Makefile

$(ARTIFACTS):
	@mkdir -p $@

vars: | $(ARTIFACTS)
	@test -s "$(OUTPUTS)" || { echo "missing $(OUTPUTS) (aws-e2e base not applied yet)" >&2; exit 1; }
	@jq '{vpc_id, subnet_id: .public_subnet_id, security_group_ids: [.sg_builders], admin_cidrs: [.admin_cidr]}' \
	  "$(OUTPUTS)" > "$(ARTIFACTS)/env.pkrvars.json"
	@chmod 600 "$(ARTIFACTS)/env.pkrvars.json"

agent:
	@mkdir -p "$(dir $(WORKER_AGENT))"
	cd $(HERE)/../.. && GOOS=linux GOARCH=$(GOARCH) CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(IMAGE_VERSION)" \
	  -o "$(WORKER_AGENT)" ./cmd/cucina-worker-agent

smoke-binaries: | $(ARTIFACTS)
	@test -s "$(ARTIFACTS)/smoke/hello-s390x-dynamic" || $(HERE)/scripts/build-smoke-binaries.sh "$(ARTIFACTS)/smoke"

image-linux: vars smoke-binaries
	@case "$(ARCH)/$(VARIANT)" in x86_64/ubuntu|arm64/ubuntu|x86_64/al2023|arm64/al2023) ;; *) echo "ARCH=x86_64|arm64 VARIANT=ubuntu|al2023" >&2; exit 2;; esac
	@gen=$${GENERATION:-$$($(HERE)/scripts/next-generation.sh $(FAMILY) $(REGION))}; \
	echo "building $(FAMILY) image $(IMAGE_VERSION) (generation $$gen)"; \
	cd $(HERE)/packer && $(PACKER) init . >/dev/null && \
	CUCINA_IMAGES_ARTIFACTS="$(ARTIFACTS)" $(PACKER) build -timestamp-ui \
	  -var-file="$(ARTIFACTS)/env.pkrvars.json" -var arch=$(ARCH) -var variant=$(VARIANT) \
	  -var image_version=$(IMAGE_VERSION) -var generation=$$gen -var amis_file="$(AMIS_FILE)" \
	  -var smoke_dir="$(ARTIFACTS)/smoke" -var worker_agent_path="$(WORKER_AGENT)" .

measure-boot:
	$(HERE)/scripts/measure-boot.sh --family $(FAMILY) --count $(N)

fmt:
	$(PACKER) fmt $(HERE)/packer

# Static checks only (no AWS calls).
validate: | $(ARTIFACTS)
	$(PACKER) fmt -check -diff $(HERE)/packer
	@echo '{"vpc_id":"vpc-0","subnet_id":"subnet-0","security_group_ids":["sg-0"],"admin_cidrs":["192.0.2.1/32"]}' > "$(ARTIFACTS)/validate.pkrvars.json"
	cd $(HERE)/packer && $(PACKER) init . >/dev/null && for a in x86_64 arm64; do for v in ubuntu al2023; do \
	  CUCINA_IMAGES_ARTIFACTS="$(ARTIFACTS)" $(PACKER) validate -var-file="$(ARTIFACTS)/validate.pkrvars.json" \
	    -var arch=$$a -var variant=$$v -var image_version=0.0.0 -var generation=0 . ; done; done
	$(SHELLCHECK) $(HERE)/scripts/*.sh $(HERE)/files/bin/*
