// SPDX-License-Identifier: FSL-1.1-ALv2

//! Reducer rules of `cucinactl tui` that protect the fleet and the operator
//! (R-CLI-4): destructive actions (drain, kill, revoke, re-image) reach the API only
//! after an explicit "yes" in the confirmation dialog (default "No"), and live views
//! say when their stream is down and drop operations that ended meanwhile. Driven
//! through the public reducer with the fake management API, which records every
//! mutating request it serves.

use std::time::Duration;

use cucinactl::tui::fake::{FakeManagement, T0_MS, keys};
use cucinactl::tui::layout;
use cucinactl::tui::rows;
use cucinactl::tui::{Event, Request, State, StreamEvent, Theme, render};
use ratatui::Terminal;
use ratatui::backend::TestBackend;
use ratatui::crossterm::event::{KeyModifiers, MouseButton, MouseEvent, MouseEventKind};
use ratatui::layout::Rect;

const SIZE: (u16, u16) = (100, 30);

fn session() -> (FakeManagement, State) {
    let fake = FakeManagement::new();
    let mut state = State::new("prod", Theme::PLAIN, true, SIZE);
    fake.prime(&mut state);
    (fake, state)
}

fn click(x: u16, y: u16) -> Event {
    Event::Mouse(MouseEvent {
        kind: MouseEventKind::Down(MouseButton::Left),
        column: x,
        row: y,
        modifiers: KeyModifiers::NONE,
    })
}

fn top_line(state: &State) -> String {
    let mut term = Terminal::new(TestBackend::new(SIZE.0, SIZE.1)).unwrap();
    term.draw(|f| render(state, f)).unwrap();
    let buf = term.backend().buffer();
    (0..SIZE.0).map(|x| buf[(x, 0)].symbol()).collect()
}

#[test]
fn destructive_actions_need_an_explicit_yes() {
    let fake = FakeManagement::new();
    let first_op = fake
        .operations()
        .into_iter()
        .find(|o| o.target_id == "//absl/strings:str_cat_test")
        .unwrap()
        .name;
    let worker = "i-0a1b2c3d4e5f60718".to_string();
    let drain = Request::DrainWorker {
        node: worker.clone(),
    };
    let host = "C07DEMO00001".to_string();
    // Every dialog has three body lines (see the reducer's `ask`).
    let dialog = layout::dialog(Rect::new(0, 0, SIZE.0, SIZE.1), 3);
    let with = |script: &str, extra: Vec<Event>| {
        let mut events = keys(script);
        events.extend(extra);
        events
    };
    // "2 Down Enter" selects pool linux-x86-64 and moves to its first worker.
    let rows: Vec<(&str, Vec<Event>, Vec<Request>)> = vec![
        ("drain, Esc", keys("2 Down Enter d Esc"), vec![]),
        (
            "drain, Enter on the default No",
            keys("2 Down Enter d Enter"),
            vec![],
        ),
        ("drain, n", keys("2 Down Enter d n"), vec![]),
        (
            "drain, q closes only the dialog",
            keys("2 Down Enter d q"),
            vec![],
        ),
        (
            "drain, other keys keep asking",
            keys("2 Down Enter d x R u d j 1 / Tab Tab"),
            vec![],
        ),
        (
            "drain, Ctrl-C quits without answering",
            keys("2 Down Enter d C-c"),
            vec![],
        ),
        (
            "drain, a click outside the buttons",
            with(
                "2 Down Enter d",
                vec![click(1, 1), click(dialog.area.x + 1, dialog.area.y + 1)],
            ),
            vec![],
        ),
        (
            "drain, a click on No",
            with("2 Down Enter d", vec![click(dialog.no.x, dialog.no.y)]),
            vec![],
        ),
        ("drain, y", keys("2 Down Enter d y"), vec![drain.clone()]),
        (
            "drain, → then Enter",
            keys("2 Down Enter d Right Enter"),
            vec![drain.clone()],
        ),
        (
            "drain, a click on Yes",
            with("2 Down Enter d", vec![click(dialog.yes.x, dialog.yes.y)]),
            vec![drain.clone()],
        ),
        (
            "undrain is not destructive: no dialog",
            keys("2 Down Enter d y u"),
            vec![drain.clone(), Request::UndrainWorker { node: worker }],
        ),
        ("host drain, Esc", keys("3 d Esc"), vec![]),
        (
            "host drain, y",
            keys("3 d y"),
            vec![Request::DrainHost {
                serial: host.clone(),
            }],
        ),
        (
            "re-image, Enter on the default No",
            keys("3 R Enter"),
            vec![],
        ),
        (
            "re-image, y",
            keys("3 R y"),
            vec![Request::ReimageHost { serial: host }],
        ),
        ("kill, Esc", keys("4 x Esc"), vec![]),
        (
            "kill, y",
            keys("4 x y"),
            vec![Request::KillOperation { name: first_op }],
        ),
        ("revoke, Esc", keys("6 x Esc"), vec![]),
        (
            "revoke, y",
            keys("6 x y"),
            vec![Request::RevokeServiceKey {
                key_id: "sk-7d2f91".into(),
            }],
        ),
    ];
    for (name, events, want) in rows {
        let (fake, mut state) = session();
        fake.drive_all(&mut state, events);
        assert_eq!(fake.mutations(), want, "{name}");
        for r in fake.mutations() {
            assert!(
                !r.is_destructive() || want.contains(&r),
                "{name}: unconfirmed {r:?}"
            );
        }
    }
}

#[test]
fn live_views_say_when_their_stream_is_down() {
    let (fake, mut s) = session();
    assert!(top_line(&s).contains("LIVE"), "{}", top_line(&s));

    // The overview stream breaks: once the data is older than 5 s the header says so.
    let broken = StreamEvent::Reconnecting {
        attempt: 1,
        retry_in: Duration::from_secs(4),
        reason: "unavailable: connection reset".into(),
    };
    fake.drive(&mut s, Event::Overview(Box::new(broken)));
    fake.drive(
        &mut s,
        Event::Tick {
            now_ms: T0_MS + 4_000,
        },
    );
    assert!(top_line(&s).contains("LIVE"), "{}", top_line(&s));
    fake.drive(
        &mut s,
        Event::Tick {
            now_ms: T0_MS + 6_000,
        },
    );
    assert!(top_line(&s).contains("STALE 6s"), "{}", top_line(&s));
    fake.drive(
        &mut s,
        Event::Overview(Box::new(StreamEvent::Data(fake.overview(0)))),
    );
    assert!(top_line(&s).contains("LIVE"), "{}", top_line(&s));
    let denied = StreamEvent::Failed {
        code: Some("permission_denied".into()),
        message: "permission_denied: not an admin".into(),
    };
    fake.drive(&mut s, Event::Overview(Box::new(denied)));
    assert!(top_line(&s).contains("DISCONNECTED"), "{}", top_line(&s));

    // The operations stream breaks while an operation ends: after the reconnect the
    // list is re-read, so the ended operation does not linger.
    let ended = rows::ops(&s)[0].name.clone();
    fake.respond(&Request::KillOperation {
        name: ended.clone(),
    })
    .unwrap();
    let broken = StreamEvent::Reconnecting {
        attempt: 1,
        retry_in: Duration::from_millis(250),
        reason: "unavailable".into(),
    };
    fake.drive(&mut s, Event::Operations(Box::new(broken)));
    assert!(rows::ops(&s).iter().any(|o| o.name == ended));
    fake.drive(
        &mut s,
        Event::Tick {
            now_ms: T0_MS + 7_000,
        },
    );
    assert!(!rows::ops(&s).iter().any(|o| o.name == ended));
    assert_eq!(rows::ops(&s).len(), fake.operations().len());
}
