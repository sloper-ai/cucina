// SPDX-License-Identifier: FSL-1.1-ALv2

//! Key screens of `cucinactl tui` (R-CLI-4; R-TEST-6: a handful of screens with
//! `insta` + ratatui's `TestBackend`, at 100×30 and the classic 80×24). The state is
//! reached through the public reducer, fed by the deterministic fake at its fixed
//! clock, so no wall-clock time or randomness reaches a snapshot. Colours are not
//! part of the text snapshot; the screens are rendered with the plain palette.
//! Review changes with `cargo insta review` (or `INSTA_UPDATE=always` + diff).

use cucina_api::proto::cucina::v1 as pb;
use cucinactl::tui::fake::{FakeManagement, keys};
use cucinactl::tui::{Event, State, StreamEvent, Theme, render, update};
use ratatui::Terminal;
use ratatui::backend::TestBackend;

fn screen(width: u16, height: u16, script: &str) -> String {
    let fake = FakeManagement::new();
    let mut state = State::new("prod", Theme::PLAIN, true, (width, height));
    fake.prime(&mut state);
    fake.drive_all(&mut state, keys(script));
    let mut term = Terminal::new(TestBackend::new(width, height)).expect("test terminal");
    term.draw(|f| render(&state, f)).expect("draw");
    term.backend().to_string()
}

#[test]
fn key_screens() {
    let screens = [
        // Queue depth/time per platform, workers by state, pools desired vs actual,
        // spend, hosts online, the queued/executing sparkline and alerts.
        ("overview", ""),
        // Pool list, the selected pool's VMs, scale timeline, cold starts, and the
        // confirmation a drain needs (default No).
        ("pools_drain_confirm", "2 Down Enter d"),
        // VM slots, image versions, L2 hit ratio, WAN bytes, cordon state.
        ("hosts", "3"),
        // Operations filtered by platform (incremental filter, field syntax).
        ("operations_filtered", "4 /platform:linux Enter"),
        // Drill-down from an operation into the action inspector.
        ("action_inspector", "4 Enter"),
        // Per-pool spend, itemised lines and the idle projection.
        ("cost", "5"),
    ];
    for (width, height) in [(100, 30), (80, 24)] {
        for (name, script) in screens {
            insta::assert_snapshot!(
                format!("{name}_{width}x{height}"),
                screen(width, height, script)
            );
        }
    }
}

// Guards: R-CLI-4 — PoolSummary.registered already includes its busy/idle subsets.
// T1's single idle VM must render READY=1 and ACTUAL=1, not 2. Count what the API
// reports; never hide a real above-max count by clamping it to configured capacity.
#[test]
fn pool_counts_do_not_double_count_registered_subsets() {
    let base = pb::PoolSummary {
        name: "pool-a".into(),
        provider: "ec2".into(),
        max: 1,
        condition: "Ready".into(),
        ..Default::default()
    };
    let cases = [
        (
            "one idle VM",
            pb::PoolSummary {
                registered: 1,
                idle: 1,
                ..base.clone()
            },
            "1",
            "1",
        ),
        (
            "one busy VM",
            pb::PoolSummary {
                desired: 1,
                registered: 1,
                busy: 1,
                ..base.clone()
            },
            "1",
            "1",
        ),
        (
            "draining and booting count separately; stopped does not run",
            pb::PoolSummary {
                desired: 3,
                max: 3,
                launching: 1,
                registered: 1,
                idle: 1,
                draining: 1,
                stopped: 2,
                ..base.clone()
            },
            "1",
            "2+1",
        ),
        (
            "reported capacity above max stays visible",
            pb::PoolSummary {
                desired: 1,
                registered: 2,
                idle: 2,
                ..base
            },
            "2",
            "2",
        ),
    ];
    let mut mismatches = Vec::new();
    for (name, pool, ready, actual) in cases {
        for (width, height) in [(80, 24), (100, 30), (160, 48)] {
            for (script, column, expected) in [("", 1, actual), ("2", 3, ready)] {
                let mut state = State::new("counts", Theme::PLAIN, false, (width, height));
                update(
                    &mut state,
                    Event::Overview(Box::new(StreamEvent::Data(pb::Overview {
                        pools: vec![pool.clone()],
                        ..Default::default()
                    }))),
                );
                for event in keys(script) {
                    update(&mut state, event);
                }
                let mut terminal = Terminal::new(TestBackend::new(width, height)).unwrap();
                terminal.draw(|frame| render(&state, frame)).unwrap();
                let text = terminal.backend().to_string();
                let row = text
                    .lines()
                    .find(|line| line.contains("pool-a"))
                    .expect("pool row");
                let cells: Vec<_> = row
                    .split_once("pool-a")
                    .unwrap()
                    .1
                    .split_whitespace()
                    .collect();
                if cells[column] != expected {
                    mismatches.push(format!(
                        "{name}, {width}x{height}, tab {script:?}: expected {expected}, got {}",
                        cells[column]
                    ));
                }
            }
        }
    }
    assert!(mismatches.is_empty(), "{}", mismatches.join("\n"));
}
