// SPDX-License-Identifier: FSL-1.1-ALv2

//! The event loop: terminal setup and restore, an input thread (crossterm), a
//! 250 ms clock, the two server streams, signals, the token refresher, and the
//! execution of the reducer's effects. Everything that is not pure lives here.

use std::io::stdout;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use anyhow::{Context as _, Result};
use futures::StreamExt as _;
use ratatui::DefaultTerminal;
use ratatui::crossterm::event::{
    self as ct, DisableMouseCapture, EnableMouseCapture, KeyEventKind, MouseEventKind,
};
use ratatui::crossterm::execute;
use tokio::sync::mpsc::{UnboundedSender, unbounded_channel};
use tokio::task::JoinHandle;

use super::backend::Management;
use super::model::State;
use super::update::{Effect, Event, StreamKind, update};
use super::view;
use crate::auth::{self, ProfileCtx};
use crate::client::TokenHandle;

/// Set by the panic hook: the terminal was restored, the loop must stop.
static PANICKED: AtomicBool = AtomicBool::new(false);
const TICK: Duration = Duration::from_millis(250);
const INPUT_POLL: Duration = Duration::from_millis(100);

fn now_ms() -> i64 {
    jiff::Timestamp::now().as_millisecond()
}

/// Raw mode + alternate screen (+ mouse capture) for the lifetime of the guard;
/// restored on drop and, through ratatui's panic hook, on panic.
struct TerminalGuard {
    terminal: DefaultTerminal,
    mouse: bool,
}

impl TerminalGuard {
    fn init(mouse: bool) -> Result<TerminalGuard> {
        // Installed before ratatui's hook, which runs `ratatui::restore()` and then
        // chains to this one.
        let previous = std::panic::take_hook();
        std::panic::set_hook(Box::new(move |info| {
            PANICKED.store(true, Ordering::SeqCst);
            let _ = execute!(stdout(), DisableMouseCapture);
            previous(info);
        }));
        let terminal = match ratatui::try_init() {
            Ok(t) => t,
            Err(e) => {
                ratatui::restore();
                return Err(e).context("initialising the terminal");
            }
        };
        let mut guard = TerminalGuard {
            terminal,
            mouse: false,
        };
        guard.set_mouse(mouse);
        Ok(guard)
    }

    fn set_mouse(&mut self, on: bool) {
        let r = if on {
            execute!(stdout(), EnableMouseCapture)
        } else {
            execute!(stdout(), DisableMouseCapture)
        };
        if r.is_ok() {
            self.mouse = on;
        }
    }
}

impl Drop for TerminalGuard {
    fn drop(&mut self) {
        if self.mouse {
            let _ = execute!(stdout(), DisableMouseCapture);
        }
        ratatui::restore();
    }
}

/// Reads terminal input on a thread of its own (crossterm's blocking API works the
/// same on Unix and Windows consoles) and forwards what the reducer uses.
fn spawn_input(tx: UnboundedSender<Event>, stop: Arc<AtomicBool>) -> std::thread::JoinHandle<()> {
    std::thread::spawn(move || {
        while !stop.load(Ordering::SeqCst) {
            match ct::poll(INPUT_POLL) {
                Ok(false) => continue,
                Ok(true) => {}
                Err(_) => break,
            }
            let ev = match ct::read() {
                Ok(ct::Event::Key(k)) if k.kind != KeyEventKind::Release => Event::Key(k),
                Ok(ct::Event::Mouse(m))
                    if matches!(
                        m.kind,
                        MouseEventKind::Down(_)
                            | MouseEventKind::ScrollDown
                            | MouseEventKind::ScrollUp
                    ) =>
                {
                    Event::Mouse(m)
                }
                Ok(ct::Event::Resize(width, height)) => Event::Resize { width, height },
                Ok(_) => continue,
                Err(_) => break,
            };
            if tx.send(ev).is_err() {
                break;
            }
        }
    })
}

/// The supervised server streams, each forwarded by a task that can be restarted.
struct Streams<M: Management> {
    backend: Arc<M>,
    tx: UnboundedSender<Event>,
    overview: Option<JoinHandle<()>>,
    operations: Option<JoinHandle<()>>,
}

impl<M: Management> Streams<M> {
    fn start(&mut self, kind: StreamKind) {
        let tx = self.tx.clone();
        let handle = match kind {
            StreamKind::Overview => {
                let mut s = self.backend.watch_overview();
                tokio::spawn(async move {
                    while let Some(ev) = s.next().await {
                        if tx.send(Event::Overview(Box::new(ev))).is_err() {
                            break;
                        }
                    }
                })
            }
            StreamKind::Operations => {
                let mut s = self.backend.watch_operations();
                tokio::spawn(async move {
                    while let Some(ev) = s.next().await {
                        if tx.send(Event::Operations(Box::new(ev))).is_err() {
                            break;
                        }
                    }
                })
            }
        };
        let slot = match kind {
            StreamKind::Overview => &mut self.overview,
            StreamKind::Operations => &mut self.operations,
        };
        if let Some(old) = slot.replace(handle) {
            old.abort();
        }
    }
}

impl<M: Management> Drop for Streams<M> {
    fn drop(&mut self) {
        for h in [self.overview.take(), self.operations.take()]
            .into_iter()
            .flatten()
        {
            h.abort();
        }
    }
}

/// Keeps the session's JWT fresh (like `client::spawn_refresher`) and reports to
/// the TUI instead of logging, which would scribble over the screen.
fn spawn_refresher(
    ctx: ProfileCtx,
    handle: TokenHandle,
    tx: UnboundedSender<Event>,
) -> JoinHandle<()> {
    tokio::spawn(async move {
        loop {
            // fallback_secs = MAX: on failure return the error instead of warning.
            let wait =
                match auth::ensure_token(&ctx, auth::token::RENEW_BEFORE_SECS, i64::MAX).await {
                    Ok(t) => {
                        handle.set(t.access_token.clone());
                        let _ = tx.send(Event::Token(Ok(())));
                        let left =
                            t.remaining(crate::util::now_unix()) - auth::token::RENEW_BEFORE_SECS;
                        Duration::from_secs(u64::try_from(left.max(5)).unwrap_or(5))
                    }
                    Err(e) => {
                        let _ = tx.send(Event::Token(Err(format!("{e:#}"))));
                        Duration::from_secs(15)
                    }
                };
            tokio::time::sleep(wait).await;
        }
    })
}

/// The clock: wall time, or a fixed instant (deterministic demo modes).
#[derive(Debug, Clone, Copy)]
pub enum Clock {
    Wall,
    Fixed(i64),
}

impl Clock {
    fn now_ms(self) -> i64 {
        match self {
            Clock::Wall => now_ms(),
            Clock::Fixed(ms) => ms,
        }
    }
}

fn spawn_clock(tx: UnboundedSender<Event>, clock: Clock) -> JoinHandle<()> {
    tokio::spawn(async move {
        let mut tick = tokio::time::interval(TICK);
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            tick.tick().await;
            if tx
                .send(Event::Tick {
                    now_ms: clock.now_ms(),
                })
                .is_err()
            {
                break;
            }
        }
    })
}

fn spawn_signals(tx: UnboundedSender<Event>) -> JoinHandle<()> {
    tokio::spawn(async move {
        #[cfg(unix)]
        {
            use tokio::signal::unix::{SignalKind, signal};
            let (Ok(mut term), Ok(mut hup)) = (
                signal(SignalKind::terminate()),
                signal(SignalKind::hangup()),
            ) else {
                let _ = tokio::signal::ctrl_c().await;
                let _ = tx.send(Event::Quit);
                return;
            };
            tokio::select! {
                _ = tokio::signal::ctrl_c() => {}
                _ = term.recv() => {}
                _ = hup.recv() => {}
            }
        }
        #[cfg(not(unix))]
        {
            let _ = tokio::signal::ctrl_c().await;
        }
        let _ = tx.send(Event::Quit);
    })
}

/// Runs the TUI against `backend` until the user quits.
pub async fn run<M: Management>(
    backend: Arc<M>,
    mut state: State,
    refresher: Option<(ProfileCtx, TokenHandle)>,
    clock: Clock,
) -> Result<()> {
    let (tx, mut rx) = unbounded_channel::<Event>();
    let mut term = TerminalGuard::init(state.mouse)?;
    let size = term.terminal.size().context("reading the terminal size")?;
    state.size = (size.width, size.height);

    let stop = Arc::new(AtomicBool::new(false));
    let input = spawn_input(tx.clone(), stop.clone());
    let mut tasks = vec![spawn_clock(tx.clone(), clock), spawn_signals(tx.clone())];
    if let Some((ctx, handle)) = refresher {
        tasks.push(spawn_refresher(ctx, handle, tx.clone()));
    }
    let mut streams = Streams {
        backend: backend.clone(),
        tx: tx.clone(),
        overview: None,
        operations: None,
    };
    streams.start(StreamKind::Overview);
    streams.start(StreamKind::Operations);
    let _ = tx.send(Event::Tick {
        now_ms: clock.now_ms(),
    });

    let result = loop {
        if let Err(e) = term.terminal.draw(|f| view::render(&state, f)) {
            break Err(anyhow::Error::from(e).context("drawing the screen"));
        }
        let Some(first) = rx.recv().await else {
            break Ok(());
        };
        let mut effects = update(&mut state, first);
        // Apply whatever else is already queued before drawing again.
        while !state.quit {
            match rx.try_recv() {
                Ok(ev) => effects.extend(update(&mut state, ev)),
                Err(_) => break,
            }
        }
        for effect in effects {
            match effect {
                Effect::Call { id, request } => {
                    let call = backend.call(request.clone());
                    let tx = tx.clone();
                    tokio::spawn(async move {
                        let result = call.await;
                        let _ = tx.send(Event::Reply {
                            id,
                            request,
                            result,
                        });
                    });
                }
                Effect::StartStream(kind) => streams.start(kind),
                Effect::MouseCapture(on) => term.set_mouse(on),
                Effect::Repaint => {
                    let _ = term.terminal.clear();
                }
            }
        }
        if PANICKED.load(Ordering::SeqCst) {
            break Err(anyhow::anyhow!(
                "internal error (see the panic message above)"
            ));
        }
        if state.quit {
            break Ok(());
        }
    };

    stop.store(true, Ordering::SeqCst);
    for t in &tasks {
        t.abort();
    }
    drop(streams);
    drop(term);
    let _ = input.join();
    result
}
