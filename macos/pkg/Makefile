# SPDX-License-Identifier: FSL-1.1-ALv2
# Local entry points for the Cucina host package (R-MAC-8/-9). Bazel builds the unsigned package from the same
# scripts (see BUILD.bazel and docs/mdm/pkg.md); signing is local-only because it needs keychain access.
#
#   make -C macos/pkg pkg                       unsigned package           build/cucina-host-X-Y-Z-unsigned.pkg
#   make -C macos/pkg sign IDENTITY="..."       signed with the private certificate   build/cucina-host-X-Y-Z.pkg
#   make -C macos/pkg sign SIGNING=developer-id DEVID_APP=... DEVID_INSTALLER=... NOTARY_KEY=... NOTARY_KEY_ID=... NOTARY_ISSUER=...
#   make -C macos/pkg uninstall-pkg [IDENTITY=...]   payload-free uninstaller (UNINSTALL_ARGS=--purge for the purge variant)
#   make -C macos/pkg manifest [URL=... MANIFEST_URL=...]   MDM manifest + Apple Business values for the signed package
#   make -C macos/pkg pkg-publish [TARGET=github|s3] [DRY_RUN=1]  versioned, immutable upload + verification
#   make -C macos/pkg cert CERT_NAME="..."      create the private signing identity in the login keychain
#   make -C macos/pkg check | lint | test | clean
#
# Inputs: VERSION (default: <repo>/VERSION or 0.1.0), HOSTD_BIN (default: go build ./cmd/cucina-hostd),
# NOTICES (default: <repo>/THIRD_PARTY_NOTICES.md), IDENTITY (default: $CUCINA_SIGN_IDENTITY; name or SHA-1),
# SIGN_KEYCHAIN (keychain holding the identity, e.g. from scripts/ci-keychain.sh).

SHELL := /bin/sh
REPO := $(abspath $(CURDIR)/../..)
VERSION ?= $(shell cat $(REPO)/VERSION 2>/dev/null || echo 0.1.0)
DASHED := $(subst .,-,$(VERSION))
BASE := cucina-host-$(DASHED)
BUILD ?= $(CURDIR)/build
HOSTD_BIN ?= $(BUILD)/cucina-hostd
NOTICES ?= $(REPO)/THIRD_PARTY_NOTICES.md
LICENSE ?= $(REPO)/LICENSE.md
IDENTITY ?= $(CUCINA_SIGN_IDENTITY)
SIGNING ?= private
UNINSTALL_ARGS ?=
TARGET ?= github
REPO_SLUG ?= sloper-ai/cucina
URL ?= https://github.com/$(REPO_SLUG)/releases/download/v$(VERSION)/$(BASE).pkg
MANIFEST_URL ?= https://github.com/$(REPO_SLUG)/releases/download/v$(VERSION)/$(BASE).plist
CERT_NAME ?= Cucina Host Package Signing
DRY_RUN ?=

UNSIGNED := $(BUILD)/$(BASE)-unsigned.pkg
SIGNED := $(BUILD)/$(BASE).pkg
UNINSTALLER := $(BUILD)/cucina-host-uninstall-$(DASHED).pkg
INPUTS = --hostd "$(HOSTD_BIN)" --license "$(LICENSE)" --notices "$(NOTICES)" --version "$(VERSION)"

.PHONY: help fetch hostd pkg sign uninstall-pkg check manifest pkg-publish cert lint test clean

help:
	@sed -n '2,/^$$/p' Makefile | sed 's/^# \{0,1\}//'

fetch:
	scripts/fetch-deps.sh

# Builds cucina-hostd (cgo, darwin/arm64) unless HOSTD_BIN points at an existing binary.
hostd:
	@if [ "$(HOSTD_BIN)" = "$(BUILD)/cucina-hostd" ]; then \
		[ -d "$(REPO)/cmd/cucina-hostd" ] || { echo "error: $(REPO)/cmd/cucina-hostd not found; pass HOSTD_BIN=/path/to/cucina-hostd" >&2; exit 1; }; \
		mkdir -p "$(BUILD)"; \
		cd "$(REPO)" && CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o "$(HOSTD_BIN)" ./cmd/cucina-hostd; \
	else \
		[ -f "$(HOSTD_BIN)" ] || { echo "error: HOSTD_BIN=$(HOSTD_BIN) does not exist" >&2; exit 1; }; \
	fi

pkg: hostd
	scripts/build-pkg.sh $(INPUTS) --out "$(UNSIGNED)"

sign: hostd
	CUCINA_SIGNING="$(SIGNING)" scripts/sign.sh \
		$(if $(IDENTITY),--identity "$(IDENTITY)") \
		$(if $(SIGN_KEYCHAIN),--keychain "$(SIGN_KEYCHAIN)") \
		$(if $(DEVID_APP),--app-identity "$(DEVID_APP)") \
		$(if $(DEVID_INSTALLER),--installer-identity "$(DEVID_INSTALLER)") \
		$(if $(NOTARY_KEY),--notary-key "$(NOTARY_KEY)" --notary-key-id "$(NOTARY_KEY_ID)" --notary-issuer "$(NOTARY_ISSUER)") \
		-- $(INPUTS) --out "$(SIGNED)"

uninstall-pkg:
	scripts/build-pkg.sh --uninstall --version "$(VERSION)" --out "$(UNINSTALLER)" \
		$(if $(UNINSTALL_ARGS),--uninstall-args "$(UNINSTALL_ARGS)") \
		$(if $(IDENTITY),--installer-identity "$(IDENTITY)") \
		$(if $(SIGN_KEYCHAIN),--keychain "$(SIGN_KEYCHAIN)")
	scripts/check-pkg.sh "$(UNINSTALLER)" --uninstaller

check:
	@for p in "$(UNSIGNED)" "$(SIGNED)"; do \
		[ ! -f "$$p" ] || scripts/check-pkg.sh "$$p" --version "$(VERSION)" $$([ "$$p" = "$(SIGNED)" ] && echo --signed); \
	done

manifest:
	scripts/make-manifest.sh --pkg "$(SIGNED)" --version "$(VERSION)" --url "$(URL)" --manifest-url "$(MANIFEST_URL)" --out-dir "$(BUILD)"

pkg-publish:
	scripts/publish.sh --target "$(TARGET)" --version "$(VERSION)" --pkg "$(SIGNED)" \
		$(if $(wildcard $(UNINSTALLER)),--uninstall-pkg "$(UNINSTALLER)") \
		$(if $(SIGNER_CERT),--signer-cert "$(SIGNER_CERT)") \
		$(if $(BUCKET),--bucket "$(BUCKET)") $(if $(BASE_URL),--base-url "$(BASE_URL)") \
		$(if $(DRY_RUN),--dry-run)

cert:
	scripts/make-signing-cert.sh --name "$(CERT_NAME)"

lint:
	test/lint.sh

test:
	test/lint.sh
	test/manifest_test.sh
	test/setup_dryrun_test.sh
	../profiles/test/render_test.sh

clean:
	rm -rf "$(BUILD)"
