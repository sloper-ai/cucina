// SPDX-License-Identifier: FSL-1.1-ALv2

//! Command implementations (dispatch from [`crate::cli`]).

mod account;
mod fleet;
mod local;

use std::io::{BufRead as _, Write as _};

use anyhow::{Context, Result};
use clap::CommandFactory as _;

use crate::auth::ProfileCtx;
use crate::cli::{Cli, Command, CredentialHelperCmd, GlobalArgs};
use crate::client::Session;
use crate::config::{Config, Paths};
use crate::exit::{CliError, ExitCode};

/// Per-invocation context.
pub struct Ctx {
    pub global: GlobalArgs,
    pub paths: Paths,
}

impl Ctx {
    pub fn config(&self) -> Result<Config> {
        Config::load(&self.paths)
    }

    pub fn profile(&self) -> Result<ProfileCtx> {
        let config = self.config()?;
        let (name, profile) = config.select(self.global.profile.as_deref())?;
        Ok(ProfileCtx::new(&self.paths, &name, &profile))
    }

    /// An authenticated management session (renews the token when needed).
    pub async fn session(&self) -> Result<Session> {
        Session::open(self.profile()?, self.global.timeout).await
    }

    /// Asks for confirmation of a destructive action: `--yes`, or an interactive
    /// "y" on a terminal. Without a terminal and without `--yes` it refuses (exit 2).
    pub fn confirm(&self, what: &str) -> Result<()> {
        if self.global.yes {
            return Ok(());
        }
        if !crate::util::interactive() {
            return Err(CliError::usage(format!(
                "refusing to {what} without confirmation; pass --yes"
            ))
            .into());
        }
        let mut err = std::io::stderr().lock();
        write!(err, "{what}? [y/N] ")?;
        err.flush()?;
        let mut answer = String::new();
        std::io::stdin().lock().read_line(&mut answer)?;
        if matches!(answer.trim(), "y" | "Y" | "yes" | "YES" | "Yes") {
            Ok(())
        } else {
            Err(CliError::usage("aborted").into())
        }
    }
}

/// Builds the async runtime for network commands.
pub fn runtime() -> Result<tokio::runtime::Runtime> {
    tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .context("starting the async runtime")
}

/// Runs a parsed command line.
pub fn run(cli: Cli) -> Result<()> {
    let ctx = Ctx {
        global: cli.global,
        paths: Paths::resolve()?,
    };
    let Some(command) = cli.command else {
        if crate::util::interactive() {
            return runtime()?.block_on(crate::tui::run(&ctx));
        }
        let mut cmd = Cli::command();
        let _ = cmd.write_help(&mut std::io::stderr());
        return Err(CliError::usage("no command given (the TUI needs a terminal)").into());
    };
    match command {
        // Local commands: no network, no runtime.
        Command::Completions { shell } => local::completions(shell),
        Command::Config(cmd) => local::config(&ctx, cmd),
        Command::Bazelrc(args) => local::bazelrc(&ctx, &args),
        Command::CredentialHelper(CredentialHelperCmd::Get) => {
            match crate::credential_helper::run_get() {
                ExitCode::Ok => Ok(()),
                // The helper already reported the error on stderr.
                code => Err(CliError::new(code, String::new()).into()),
            }
        }
        Command::CredentialHelper(CredentialHelperCmd::Install { dir }) => {
            local::install_helper(&ctx, dir)
        }
        Command::Logout(args) => account::logout(&ctx, &args),
        Command::Tui => runtime()?.block_on(crate::tui::run(&ctx)),
        // Network commands.
        Command::Login(args) => runtime()?.block_on(account::login(&ctx, &args)),
        Command::Whoami => runtime()?.block_on(account::whoami(&ctx)),
        Command::Keys(cmd) => runtime()?.block_on(account::keys(&ctx, cmd)),
        Command::Status => runtime()?.block_on(fleet::status(&ctx)),
        Command::Pools(cmd) => runtime()?.block_on(fleet::pools(&ctx, cmd)),
        Command::Workers(cmd) => runtime()?.block_on(fleet::workers(&ctx, cmd)),
        Command::Hosts(cmd) => runtime()?.block_on(fleet::hosts(&ctx, cmd)),
        Command::Queues => runtime()?.block_on(fleet::queues(&ctx)),
        Command::Ops(cmd) => runtime()?.block_on(fleet::ops(&ctx, cmd)),
        Command::Action(cmd) => runtime()?.block_on(fleet::action(&ctx, cmd)),
        Command::Cost(args) => runtime()?.block_on(fleet::cost(&ctx, &args)),
        Command::Images => runtime()?.block_on(fleet::images(&ctx)),
        Command::Diag(args) => runtime()?.block_on(fleet::diag(&ctx, &args)),
    }
}
