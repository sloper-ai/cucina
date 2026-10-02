// SPDX-License-Identifier: FSL-1.1-ALv2

//! The quick filter (`/` or `:`): whitespace-separated terms that must all match.
//! `field:value` (or `field=value`) matches one column, a bare word matches any
//! column; both are case-insensitive substring matches, applied as you type.
//! Field names may be abbreviated to any unambiguous prefix (`plat:linux`,
//! `inv:…`); a term whose field is unknown is matched as plain text, so URLs and
//! digests containing `:` still work. Quote values with spaces: `target:"//a b"`.

/// A field a table can be filtered on: canonical name and aliases.
pub type FieldSpec = (&'static str, &'static [&'static str]);

#[derive(Debug, Clone, PartialEq, Eq)]
enum Term {
    Field { field: &'static str, value: String },
    Text(String),
}

/// A parsed filter.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Filter {
    terms: Vec<Term>,
}

fn tokens(input: &str) -> Vec<String> {
    let mut out = Vec::new();
    let mut cur = String::new();
    let mut quoted = false;
    for c in input.chars() {
        match c {
            '"' => quoted = !quoted,
            c if c.is_whitespace() && !quoted => {
                if !cur.is_empty() {
                    out.push(std::mem::take(&mut cur));
                }
            }
            c => cur.push(c),
        }
    }
    if !cur.is_empty() {
        out.push(cur);
    }
    out
}

fn resolve(name: &str, fields: &[FieldSpec]) -> Option<&'static str> {
    let name = name.to_ascii_lowercase();
    if let Some((canon, _)) = fields
        .iter()
        .find(|(canon, aliases)| *canon == name || aliases.contains(&name.as_str()))
    {
        return Some(canon);
    }
    let mut prefixed = fields.iter().filter(|(canon, _)| canon.starts_with(&name));
    match (prefixed.next(), prefixed.next()) {
        (Some((canon, _)), None) => Some(canon),
        _ => None,
    }
}

impl Filter {
    /// Parses `input` for a table with `fields`.
    pub fn parse(input: &str, fields: &[FieldSpec]) -> Filter {
        let mut terms = Vec::new();
        for tok in tokens(input) {
            let split = tok
                .find([':', '='])
                .filter(|&i| i > 0)
                .map(|i| (&tok[..i], &tok[i + 1..]));
            let term = match split.and_then(|(name, value)| Some((resolve(name, fields)?, value))) {
                Some((_, "")) => continue,
                Some((field, value)) => Term::Field {
                    field,
                    value: value.to_lowercase(),
                },
                None => Term::Text(tok.to_lowercase()),
            };
            terms.push(term);
        }
        Filter { terms }
    }

    pub fn is_empty(&self) -> bool {
        self.terms.is_empty()
    }

    /// Whether a row with these `(field, value)` cells passes.
    pub fn matches(&self, row: &[(&'static str, String)]) -> bool {
        self.terms.iter().all(|t| match t {
            Term::Field { field, value } => row
                .iter()
                .any(|(f, v)| f == field && v.to_lowercase().contains(value.as_str())),
            Term::Text(text) => row
                .iter()
                .any(|(_, v)| v.to_lowercase().contains(text.as_str())),
        })
    }
}
