// SPDX-License-Identifier: FSL-1.1-ALv2

//! A minimal JSON Schema (2020-12 subset) validator for the `--output json`
//! contract tests: `type` (string or list), `properties`, `required`,
//! `additionalProperties` (bool or schema), `items`, `enum`, `const`, `anyOf` and
//! local `$ref` (`#/$defs/<name>`). Exactly the keywords used by
//! `cli/cucinactl/schemas/*.schema.json`; an unknown keyword is an error so the
//! subset cannot silently drift.

use std::path::PathBuf;

use serde_json::Value;

const KNOWN: &[&str] = &[
    "$schema",
    "$id",
    "$defs",
    // Annotations (no validation effect).
    "title",
    "description",
    "type",
    "properties",
    "required",
    "additionalProperties",
    "items",
    "enum",
    "const",
    "anyOf",
    "$ref",
];

/// Loads `schemas/<name>.schema.json`.
pub fn load(name: &str) -> Value {
    let path: PathBuf = super::repo_root()
        .join("cli/cucinactl/schemas")
        .join(format!("{name}.schema.json"));
    let text = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    serde_json::from_str(&text).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

/// Validates `doc` against `schema`; returns the list of violations.
pub fn validate(schema: &Value, doc: &Value) -> Vec<String> {
    let mut errors = Vec::new();
    check(schema, schema, doc, "$", &mut errors);
    errors
}

fn type_matches(t: &str, v: &Value) -> bool {
    match t {
        "object" => v.is_object(),
        "array" => v.is_array(),
        "string" => v.is_string(),
        "boolean" => v.is_boolean(),
        "null" => v.is_null(),
        "integer" => v.is_i64() || v.is_u64(),
        "number" => v.is_number(),
        _ => false,
    }
}

fn check(root: &Value, schema: &Value, v: &Value, path: &str, errors: &mut Vec<String>) {
    let Some(obj) = schema.as_object() else {
        errors.push(format!("{path}: schema is not an object"));
        return;
    };
    for k in obj.keys() {
        if !KNOWN.contains(&k.as_str()) {
            errors.push(format!("{path}: unsupported schema keyword {k:?}"));
        }
    }
    if let Some(r) = obj.get("$ref").and_then(Value::as_str) {
        let name = r.strip_prefix("#/$defs/").unwrap_or(r);
        match root.get("$defs").and_then(|d| d.get(name)) {
            Some(target) => check(root, target, v, path, errors),
            None => errors.push(format!("{path}: unresolved $ref {r}")),
        }
        return;
    }
    if let Some(any) = obj.get("anyOf").and_then(Value::as_array) {
        let ok = any.iter().any(|alt| {
            let mut e = Vec::new();
            check(root, alt, v, path, &mut e);
            e.is_empty()
        });
        if !ok {
            errors.push(format!("{path}: matches no anyOf alternative: {v}"));
        }
    }
    if let Some(c) = obj.get("const")
        && c != v
    {
        errors.push(format!("{path}: expected const {c}, got {v}"));
    }
    if let Some(e) = obj.get("enum").and_then(Value::as_array)
        && !e.contains(v)
    {
        errors.push(format!("{path}: {v} not in enum {e:?}"));
    }
    if let Some(t) = obj.get("type") {
        let types: Vec<&str> = match t {
            Value::String(s) => vec![s.as_str()],
            Value::Array(a) => a.iter().filter_map(Value::as_str).collect(),
            _ => vec![],
        };
        if !types.iter().any(|t| type_matches(t, v)) {
            errors.push(format!("{path}: expected type {types:?}, got {v}"));
            return;
        }
    }
    if let Value::Object(map) = v {
        let props = obj.get("properties").and_then(Value::as_object);
        if let Some(req) = obj.get("required").and_then(Value::as_array) {
            for r in req.iter().filter_map(Value::as_str) {
                if !map.contains_key(r) {
                    errors.push(format!("{path}: missing required field {r:?}"));
                }
            }
        }
        for (k, child) in map {
            let sub = format!("{path}.{k}");
            match props.and_then(|p| p.get(k)) {
                Some(s) => check(root, s, child, &sub, errors),
                None => match obj.get("additionalProperties") {
                    Some(Value::Bool(false)) => {
                        errors.push(format!("{path}: unexpected field {k:?}"))
                    }
                    Some(s @ Value::Object(_)) => check(root, s, child, &sub, errors),
                    _ => {}
                },
            }
        }
    }
    if let (Value::Array(items), Some(item_schema)) = (v, obj.get("items")) {
        for (i, item) in items.iter().enumerate() {
            check(root, item_schema, item, &format!("{path}[{i}]"), errors);
        }
    }
}

/// Panics with every violation if `doc` does not satisfy schema `name`.
pub fn assert_valid(name: &str, doc: &Value) {
    let schema = load(name);
    let errors = validate(&schema, doc);
    assert!(
        errors.is_empty(),
        "output does not match schemas/{name}.schema.json:\n  {}\ndocument: {doc}",
        errors.join("\n  ")
    );
    assert_eq!(
        doc.get("schema").and_then(Value::as_str),
        Some(name),
        "schema field"
    );
}
