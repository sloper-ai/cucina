// SPDX-License-Identifier: FSL-1.1-ALv2

//! Key screens of `cucinactl tui` (R-CLI-4; R-TEST-6: a handful of screens with
//! `insta` + ratatui's `TestBackend`, at 100×30 and the classic 80×24). The state is
//! reached through the public reducer, fed by the deterministic fake at its fixed
//! clock, so no wall-clock time or randomness reaches a snapshot. Colours are not
//! part of the text snapshot; the screens are rendered with the plain palette.
//! Review changes with `cargo insta review` (or `INSTA_UPDATE=always` + diff).

use cucinactl::tui::fake::{FakeManagement, keys};
use cucinactl::tui::{State, Theme, render};
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
