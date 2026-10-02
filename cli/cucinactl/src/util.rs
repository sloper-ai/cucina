// SPDX-License-Identifier: FSL-1.1-ALv2

//! Small shared helpers: durations, timestamps, terminal detection.

use std::io::IsTerminal;
use std::time::Duration;

use jiff::Timestamp;

/// Parses a duration such as `90s`, `5m`, `1h30m`, `2d` or `250ms`.
pub fn parse_duration(input: &str) -> Result<Duration, String> {
    let s = input.trim();
    if s.is_empty() {
        return Err("empty duration".into());
    }
    let mut total = Duration::ZERO;
    let mut rest = s;
    while !rest.is_empty() {
        let digits = rest
            .find(|c: char| !c.is_ascii_digit())
            .unwrap_or(rest.len());
        if digits == 0 {
            return Err(format!(
                "invalid duration {input:?} (expected e.g. 30s, 5m, 1h30m, 2d)"
            ));
        }
        let value: u64 = rest[..digits]
            .parse()
            .map_err(|_| format!("invalid number in duration {input:?}"))?;
        rest = &rest[digits..];
        let unit_len = rest
            .find(|c: char| c.is_ascii_digit())
            .unwrap_or(rest.len());
        let unit = &rest[..unit_len];
        rest = &rest[unit_len..];
        let piece = match unit {
            "ms" => Duration::from_millis(value),
            "s" => Duration::from_secs(value),
            "m" => Duration::from_secs(value.saturating_mul(60)),
            "h" => Duration::from_secs(value.saturating_mul(3600)),
            "d" => Duration::from_secs(value.saturating_mul(86_400)),
            _ => {
                return Err(format!(
                    "invalid unit {unit:?} in duration {input:?} (use ms, s, m, h or d)"
                ));
            }
        };
        total = total.saturating_add(piece);
    }
    Ok(total)
}

/// Formats a duration compactly (`1h30m`, `45s`, `250ms`).
pub fn format_duration(d: Duration) -> String {
    let secs = d.as_secs();
    if secs == 0 {
        return format!("{}ms", d.subsec_millis());
    }
    let (days, rem) = (secs / 86_400, secs % 86_400);
    let (hours, rem) = (rem / 3600, rem % 3600);
    let (mins, s) = (rem / 60, rem % 60);
    let mut out = String::new();
    for (v, u) in [(days, "d"), (hours, "h"), (mins, "m"), (s, "s")] {
        if v > 0 {
            out.push_str(&format!("{v}{u}"));
        }
    }
    out
}

/// Current Unix time in seconds.
pub fn now_unix() -> i64 {
    Timestamp::now().as_second()
}

/// RFC 3339 UTC timestamp with whole seconds (`2026-10-02T10:15:00Z`), the format
/// Bazel's credential-helper protocol requires (no fractional seconds).
pub fn rfc3339_seconds(unix: i64) -> String {
    match Timestamp::from_second(unix) {
        Ok(ts) => ts.strftime("%Y-%m-%dT%H:%M:%SZ").to_string(),
        Err(_) => "1970-01-01T00:00:00Z".into(),
    }
}

/// Protobuf well-known types (buffa-types).
pub use buffa_types::google::protobuf::{Duration as PbDuration, Timestamp as PbTimestamp};

/// Converts a protobuf timestamp to RFC 3339 (whole seconds); `None` when unset.
pub fn proto_time(ts: Option<&PbTimestamp>) -> Option<String> {
    ts.filter(|t| t.seconds != 0 || t.nanos != 0)
        .map(|t| rfc3339_seconds(t.seconds))
}

/// Converts a protobuf duration to seconds (fractional); `None` when unset.
pub fn proto_secs(d: Option<&PbDuration>) -> Option<f64> {
    d.map(|d| d.seconds as f64 + f64::from(d.nanos) / 1e9)
}

/// Converts a std duration to a protobuf duration.
pub fn to_proto_duration(d: Duration) -> PbDuration {
    PbDuration {
        seconds: i64::try_from(d.as_secs()).unwrap_or(i64::MAX),
        nanos: i32::try_from(d.subsec_nanos()).unwrap_or(0),
        ..Default::default()
    }
}

/// Human rendering of an optional protobuf duration (`-` when unset).
pub fn human_secs(secs: Option<f64>) -> String {
    match secs {
        Some(s) if s >= 0.0 => format_duration(Duration::from_secs_f64(s)),
        _ => "-".into(),
    }
}

/// Whether stdin and stdout are both terminals (interactive session).
pub fn interactive() -> bool {
    std::io::stdin().is_terminal() && std::io::stdout().is_terminal()
}

/// Whether stderr is a terminal (progress bars, prompts).
pub fn stderr_is_tty() -> bool {
    std::io::stderr().is_terminal()
}

/// Lowercase hex encoding.
pub fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut s, b| {
            let _ = write!(s, "{b:02x}");
            s
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    // Guards the duration syntax documented in docs/cli.md (flags such as --for, --ttl).
    #[test]
    fn durations_parse_and_reject() {
        let cases: &[(&str, Option<u64>)] = &[
            ("90s", Some(90)),
            ("5m", Some(300)),
            ("1h30m", Some(5400)),
            ("2d", Some(172_800)),
            ("1d2h3m4s", Some(93_784)),
            ("", None),
            ("10", None),
            ("m", None),
            ("3w", None),
            ("-5m", None),
        ];
        for (input, want) in cases {
            let got = parse_duration(input).ok().map(|d| d.as_secs());
            assert_eq!(got, *want, "parse_duration({input:?})");
        }
        assert_eq!(parse_duration("250ms").unwrap(), Duration::from_millis(250));
    }
}
