# SPDX-License-Identifier: FSL-1.1-ALv2
#
# macOS Tart worker image (R-MAC-7, R-VER-3). See README.md and docs/operations/macos-images.md.
#
#   make -C workers/macos image-macos XCODE=27.0   build -> smoke test -> report (local image in TART_HOME)
#   make -C workers/macos validate                  packer init/fmt/validate + shellcheck (no VM needed)
#   make -C workers/macos push XCODE=27.0           publish to the private GHCR package (guarded; never in the campaign)
#
# A new Xcode is one versions.json entry plus `make image-macos XCODE=<ver>`; the pool generation bump (new image
# reference in the WorkerPool) then rolls it out (R-OPS-2). Variables: CUCINA_VERSION, XCODE_APP (Xcode.app to copy
# when the Cirrus image ships another build; auto-detected from /Applications/Xcode.app), WORKER_AGENT (darwin/arm64
# cucina-worker-agent; built from cmd/cucina-worker-agent when present), FORCE=1 (replace an existing local image),
# BOOTS (boot-to-ready samples, default 3).

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

HERE := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
ROOT := $(abspath $(HERE)/../..)
VERSIONS := $(HERE)/versions.json

XCODE ?= 27.0
CUCINA_VERSION ?= 0.1.0-dev
DEV_STORAGE ?= $(or $(CUCINA_DEV_STORAGE),/Volumes/Code/Sources/.dev-storage/cucina)
WORK ?= $(DEV_STORAGE)/macimage
TART_HOME ?= $(DEV_STORAGE)/tart
PACKER_CACHE_DIR ?= $(DEV_STORAGE)/packer-cache
export TART_HOME PACKER_CACHE_DIR
PACKER ?= packer
SHELLCHECK ?= $(shell shellcheck --version >/dev/null 2>&1 && echo shellcheck || echo 'mise exec aqua:koalaman/shellcheck@0.11.0 -- shellcheck')

IMAGE := cucina-worker-macos:$(XCODE)-$(CUCINA_VERSION)
XCODE_BUILD := $(shell jq -r --arg x '$(XCODE)' '.xcode[$$x].build // empty' $(VERSIONS))
BASE_REF := $(shell jq -r --arg x '$(XCODE)' '.xcode[$$x] | if . == null then empty elif .baseImageDigest != "" then (.baseImage | sub(":[^:/]+$$"; "")) + "@" + .baseImageDigest else .baseImage end' $(VERSIONS))
BB_RELEASE := $(shell jq -r .buildbarn.bbRemoteExecution $(VERSIONS))
BB_DIR := $(WORK)/buildbarn/$(BB_RELEASE)
WORKER_AGENT ?= $(WORK)/bin/cucina-worker-agent
# The local Xcode.app (symlinks resolved) when its build is the one versions.json wants; shared read-only into the
# build VM and copied only if the base image's Xcode differs (docs/adr/0352-*).
XCODE_APP ?= $(shell [ "$$(jq -r --arg x '$(XCODE)' '.xcode[$$x].source' $(VERSIONS))" != cirrus ] && \
  app="$$(cd /Applications/Xcode.app 2>/dev/null && pwd -P)" && [ -n "$$app" ] && \
  [ "$$(/usr/libexec/PlistBuddy -c 'Print :ProductBuildVersion' "$$app/Contents/version.plist" 2>/dev/null)" = "$(XCODE_BUILD)" ] && echo "$$app")
STAMP := $(shell date -u +%Y%m%dT%H%M%SZ)
LOG ?= $(DEV_STORAGE)/logs/macimage-packer-$(XCODE)-$(STAMP).log
REPORT ?= $(WORK)/reports/$(XCODE)-$(CUCINA_VERSION)-$(STAMP).json

PACKER_VARS = -var-file=$(HERE)/packer/xcode-$(XCODE).pkrvars.hcl \
  -var 'cucina_version=$(CUCINA_VERSION)' \
  -var 'buildbarn_dir=$(BB_DIR)' \
  -var 'xcode_app_source=$(XCODE_APP)'

.PHONY: help image-macos check-xcode fetch agent init lint validate require-base build smoke report push clean-test-vms

help:
	@sed -n '3,15p' $(HERE)/Makefile | sed 's/^# \{0,1\}//'

image-macos: build
	@$(MAKE) --no-print-directory -C $(HERE) smoke REPORT='$(REPORT)'
	@$(MAKE) --no-print-directory -C $(HERE) remote-smoke
	@$(MAKE) --no-print-directory -C $(HERE) report

.PHONY: remote-smoke
SMOKE_BUILD_DIRECTORY ?= nfsv4
remote-smoke:
	@CUCINA_DEV_STORAGE='$(DEV_STORAGE)' $(HERE)/scripts/fetch-buildbarn.sh '$(DEV_STORAGE)/bb-release' --smoke-servers
	@CUCINA_DEV_STORAGE='$(DEV_STORAGE)' RUNS=1 UNITS=2 HEADERS=16 \
	  $(HERE)/bench/bench-build-dir.sh '$(IMAGE)' '$(SMOKE_BUILD_DIRECTORY)'

check-xcode:
	@[ -n "$(XCODE_BUILD)" ] || { echo "Xcode $(XCODE) has no entry in versions.json" >&2; exit 1; }
	@[ -f "$(HERE)/packer/xcode-$(XCODE).pkrvars.hcl" ] || { echo "missing packer/xcode-$(XCODE).pkrvars.hcl" >&2; exit 1; }

fetch:
	@$(HERE)/scripts/fetch-buildbarn.sh $(BB_DIR)

agent:
	@if [ "$(WORKER_AGENT)" != "$(WORK)/bin/cucina-worker-agent" ]; then \
	  [ -f "$(WORKER_AGENT)" ] || { echo "WORKER_AGENT=$(WORKER_AGENT) does not exist" >&2; exit 1; }; \
	elif compgen -G "$(ROOT)/cmd/cucina-worker-agent/*.go" >/dev/null; then \
	  mkdir -p $(WORK)/bin; \
	  cd $(ROOT) && GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags '-X main.version=$(CUCINA_VERSION)' \
	    -o $(WORKER_AGENT) ./cmd/cucina-worker-agent || exit $$?; \
	  echo "built $(WORKER_AGENT)"; \
	else \
	  echo "cmd/cucina-worker-agent is required; set WORKER_AGENT to a built darwin/arm64 binary" >&2; exit 1; \
	fi

init:
	@$(PACKER) init $(HERE)/packer >/dev/null

lint:
	$(PACKER) fmt -check -diff -recursive $(HERE)/packer
	$(SHELLCHECK) -x $(HERE)/scripts/*.sh $(HERE)/provision/*.sh $(HERE)/bench/*.sh $(HERE)/files/cucina-render

validate: check-xcode fetch init lint
	$(PACKER) validate $(PACKER_VARS) $(HERE)/packer

# Never let Packer pull implicitly: one `tart pull` holds Tart's lock, and the base is ~70 GB.
require-base:
	@[ -n "$(BASE_REF)" ] || { echo "no base image for Xcode $(XCODE) in versions.json" >&2; exit 1; }
	@tart list --source oci --format json | jq -e --arg r '$(BASE_REF)' 'any(.[]; .Name == $$r)' >/dev/null || \
	  { echo "base image $(BASE_REF) is not in TART_HOME=$(TART_HOME); pull it first (tart pull $(BASE_REF))" >&2; exit 1; }

build: check-xcode fetch agent init require-base
	@if tart get '$(IMAGE)' >/dev/null 2>&1; then \
	  if [ "$(FORCE)" = 1 ]; then tart delete '$(IMAGE)'; else echo "$(IMAGE) exists; FORCE=1 rebuilds it" >&2; exit 1; fi; \
	fi
	@mkdir -p $(dir $(LOG))
	@agent=""; agent_sha=""; \
	if [ -f "$(WORKER_AGENT)" ]; then agent="$(WORKER_AGENT)"; agent_sha="$$(shasum -a 256 "$$agent" | cut -d' ' -f1)"; fi; \
	echo "building $(IMAGE) from $(BASE_REF) (Xcode $(XCODE) $(XCODE_BUILD); Xcode share: $(or $(XCODE_APP),none)); log: $(LOG)"; \
	$(PACKER) build -color=false $(PACKER_VARS) -var "worker_agent_path=$$agent" -var "worker_agent_sha256=$$agent_sha" \
	  $(HERE)/packer >$(LOG) 2>&1 || { tail -n 40 $(LOG) >&2; exit 1; }; \
	grep -E '^\s*tart-cli\.worker: \[[0-9]{2}-|Build .* finished' $(LOG) | tail -n 25

smoke:
	@$(HERE)/scripts/test-image.sh '$(IMAGE)' '$(REPORT)'

report:
	@echo "image: $(IMAGE)"; tart list --format json | jq -r --arg i '$(IMAGE)' '.[] | select(.Name == $$i) | "  disk \(.Disk) GB logical, size on disk \(.Size) GB"'
	@echo "TART_HOME usage:"; command du -sh $(TART_HOME)/vms $(TART_HOME)/cache 2>/dev/null | sed 's/^/  /'

push:
	@$(HERE)/scripts/push.sh '$(IMAGE)'

# Cleanup is explicit: never enumerate/delete another run's throwaway VMs merely by prefix.
export VMS
clean-test-vms:
	@set -eu; [ -n "$${VMS:-}" ] || { echo 'Set VMS to the exact cucina-imgtest-* names you created' >&2; exit 2; }; \
	for vm in $$VMS; do \
	  [[ "$$vm" =~ ^cucina-imgtest-[A-Za-z0-9_-]+$$ ]] || { echo "refusing non-test VM $$vm" >&2; exit 2; }; \
	done; \
	for vm in $$VMS; do \
	  if tart list --source local --format json | jq -e --arg n "$$vm" 'any(.[]; .Name == $$n and .Running)' >/dev/null; then \
	    tart stop "$$vm" --timeout 30; \
	  fi; \
	  tart delete "$$vm"; echo "deleted $$vm"; \
	done
