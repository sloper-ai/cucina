<!-- SPDX-License-Identifier: FSL-1.1-ALv2 -->
# 0804 — `--output json` is a hand-written, schema-checked contract

* Status: accepted (2026-10-02)

## Context
R-CLI-3 asks for `--output json` for scripts with stable exit codes; R-TEST-6 asks for contract tests of the JSON schemas. The
generated buffa messages can serialize to proto3 JSON, but that would make every proto change a breaking CLI change and
renders `Money`, `Timestamp` and `Duration` in proto3's shapes.

## Decision
Each command renders a dedicated view struct (`src/views.rs`, `inspect.rs`, `commands/*`) with a `schema` field naming a JSON
Schema in `cli/cucinactl/schemas/<name>.v1.schema.json` (snake_case, RFC 3339 strings, seconds as numbers, integer
micro-dollars). `tests/json_contracts.rs` runs every command against fake servers and validates the output; a new major
shape gets a new `.v2` schema. Errors go to stderr (`error.v1` in JSON mode). No YAML.

## Consequences
Proto evolution does not break scripts; adding an output field means updating the schema in the same change (the contract
test fails otherwise).
