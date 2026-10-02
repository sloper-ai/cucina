// SPDX-License-Identifier: FSL-1.1-ALv2

//! Compact, deterministic text for table cells. Times are shown relative to the
//! state's clock (`State::now_ms`), never the wall clock, so rendering is a pure
//! function of the state.

use cucina_api::proto::cucina::v1 as pb;

use crate::util::{PbDuration, PbTimestamp};

/// Milliseconds since the Unix epoch of a set protobuf timestamp.
pub fn ts_ms(ts: Option<&PbTimestamp>) -> Option<i64> {
    ts.filter(|t| t.seconds != 0 || t.nanos != 0)
        .map(|t| t.seconds.saturating_mul(1000) + i64::from(t.nanos) / 1_000_000)
}

/// Seconds of a set protobuf duration.
pub fn dur_secs(d: Option<&PbDuration>) -> Option<f64> {
    d.map(|d| d.seconds as f64 + f64::from(d.nanos) / 1e9)
}

/// `42s`, `12m`, `2h05m`, `3d04h`, `41d` (at most 6 characters below 1000 days).
pub fn compact_secs(secs: i64) -> String {
    let s = secs.max(0);
    match s {
        0..60 => format!("{s}s"),
        60..3_600 => format!("{}m", s / 60),
        3_600..86_400 => format!("{}h{:02}m", s / 3_600, (s % 3_600) / 60),
        86_400..864_000 => format!("{}d{:02}h", s / 86_400, (s % 86_400) / 3_600),
        _ => format!("{}d", s / 86_400),
    }
}

/// Age of `ts` at `now_ms` (`-` when unset).
pub fn age(now_ms: i64, ts: Option<&PbTimestamp>) -> String {
    match ts_ms(ts) {
        Some(t) => compact_secs((now_ms - t) / 1000),
        None => "-".into(),
    }
}

/// Time until `ts` (`expired` when past, `-` when unset).
pub fn until(now_ms: i64, ts: Option<&PbTimestamp>) -> String {
    match ts_ms(ts) {
        Some(t) if t > now_ms => format!("in {}", compact_secs((t - now_ms) / 1000)),
        Some(_) => "expired".into(),
        None => "-".into(),
    }
}

/// A duration with sub-second precision below 10 s (`2.1s`, `41s`, `1m05s`).
pub fn secs(s: Option<f64>) -> String {
    match s {
        Some(s) if s < 0.0 => "-".into(),
        Some(s) if s < 10.0 => format!("{s:.1}s"),
        Some(s) if s < 60.0 => format!("{}s", s.round() as i64),
        Some(s) if s < 3_600.0 => {
            let s = s.round() as i64;
            format!("{}m{:02}s", s / 60, s % 60)
        }
        Some(s) => compact_secs(s.round() as i64),
        None => "-".into(),
    }
}

/// A protobuf duration; zero or unset is `-` (the API leaves unknown latencies at 0).
pub fn dur(d: Option<&PbDuration>) -> String {
    secs(dur_secs(d).filter(|s| *s > 0.0))
}

/// Binary-prefixed bytes (`1.2 GiB`).
pub fn bytes(n: u64) -> String {
    const UNITS: [&str; 6] = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
    let mut v = n as f64;
    let mut unit = 0;
    while v >= 1024.0 && unit + 1 < UNITS.len() {
        v /= 1024.0;
        unit += 1;
    }
    if unit == 0 {
        format!("{n} B")
    } else {
        format!("{v:.1} {}", UNITS[unit])
    }
}

/// A transfer rate (`1.2 MiB/s`).
pub fn rate(bytes_per_sec: f64) -> String {
    if !bytes_per_sec.is_finite() || bytes_per_sec <= 0.0 {
        return "0 B/s".into();
    }
    format!("{}/s", bytes(bytes_per_sec.round() as u64))
}

/// `$1,234.56` from a money message (`-` when unset).
pub fn money(m: Option<&pb::Money>) -> String {
    match m {
        Some(m) => crate::output::usd(m.micros),
        None => "-".into(),
    }
}

/// A ratio such as `"0.93"` as `93%` (other text is shown as is).
pub fn ratio(text: &str) -> String {
    match text.trim().parse::<f64>() {
        Ok(r) if (0.0..=1.0).contains(&r) => format!("{:.0}%", r * 100.0),
        _ if text.trim().is_empty() => "-".into(),
        _ => text.trim().to_string(),
    }
}

/// `linux/x86-64` for a queue's platform; Cucina's own properties are abbreviated
/// (`linux/rv64g qemu`, `macos/arm-a64 xcode27.0`), others shown as `name=value`.
pub fn platform(q: Option<&pb::QueueRef>) -> String {
    let Some(q) = q else {
        return "-".into();
    };
    let get = |name: &str| {
        q.platform
            .iter()
            .find(|p| p.name.eq_ignore_ascii_case(name))
            .map(|p| p.value.as_str())
    };
    let mut parts = Vec::new();
    match (get("OSFamily"), get("ISA")) {
        (Some(os), Some(isa)) => parts.push(format!("{os}/{isa}")),
        (Some(os), None) => parts.push(os.to_string()),
        (None, Some(isa)) => parts.push(isa.to_string()),
        (None, None) => {}
    }
    let mut extra: Vec<String> = q
        .platform
        .iter()
        .filter(|p| !p.name.eq_ignore_ascii_case("OSFamily") && !p.name.eq_ignore_ascii_case("ISA"))
        .map(|p| match p.name.as_str() {
            "cucina-emulation" => p.value.clone(),
            "xcode-version" => format!("xcode{}", p.value),
            _ => format!("{}={}", p.name, p.value),
        })
        .collect();
    extra.sort();
    parts.extend(extra);
    if parts.is_empty() {
        "-".into()
    } else {
        parts.join(" ")
    }
}

/// `-` for empty strings.
pub fn dash(s: &str) -> String {
    if s.is_empty() {
        "-".into()
    } else {
        s.to_string()
    }
}

/// Truncates to `max` characters with a trailing `…`.
pub fn ellipsize(s: &str, max: usize) -> String {
    if s.chars().count() <= max {
        return s.to_string();
    }
    let keep: String = s.chars().take(max.saturating_sub(1)).collect();
    format!("{keep}…")
}
