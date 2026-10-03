// SPDX-License-Identifier: FSL-1.1-ALv2

//! Command-line definition (clap) and dispatch. The command reference in
//! docs/cli.md is generated from these definitions (`cargo xtask docs`).

use std::ffi::OsString;
use std::path::PathBuf;
use std::time::Duration;

use clap::{ArgAction, Args, CommandFactory as _, Parser, Subcommand, ValueEnum};

use crate::config::CredentialStore;
use crate::exit::ExitCode;
use crate::output::OutputFormat;

fn duration(s: &str) -> Result<Duration, String> {
    crate::util::parse_duration(s)
}

/// Manage a Cucina deployment (a managed Buildbarn for Bazel remote execution).
///
/// Run without arguments on a terminal to open the TUI. Exit codes: 0 ok, 1 error,
/// 2 usage, 3 auth required, 4 permission denied, 5 not found, 6 unavailable,
/// 7 conflict/precondition.
#[derive(Debug, Parser)]
#[command(
    name = "cucinactl",
    version = crate::VERSION,
    propagate_version = true,
    max_term_width = 100
)]
pub struct Cli {
    #[command(flatten)]
    pub global: GlobalArgs,
    #[command(subcommand)]
    pub command: Option<Command>,
}

/// Options accepted by every command.
#[derive(Debug, Clone, Args)]
pub struct GlobalArgs {
    /// Cluster profile (default: the current profile, see `config use`).
    #[arg(long, short = 'p', global = true, env = "CUCINA_PROFILE")]
    pub profile: Option<String>,
    /// Output format: human tables or JSON for scripts.
    #[arg(long, short = 'o', global = true, value_enum, default_value_t = OutputFormat::Table, env = "CUCINA_OUTPUT")]
    pub output: OutputFormat,
    /// Answer "yes" to confirmations of destructive commands (required without a TTY).
    #[arg(long, short = 'y', global = true)]
    pub yes: bool,
    /// Deadline for each API call (e.g. 30s, 2m).
    #[arg(long, global = true, default_value = "30s", value_parser = duration)]
    pub timeout: Duration,
    /// When to use colors (NO_COLOR and non-terminals disable them under `auto`).
    #[arg(long, global = true, value_enum, default_value_t = ColorMode::Auto)]
    pub color: ColorMode,
    /// More logs on stderr (-v debug, -vv trace).
    #[arg(long, short = 'v', global = true, action = ArgAction::Count)]
    pub verbose: u8,
}

/// `--color` values.
#[derive(Debug, Clone, Copy, PartialEq, Eq, ValueEnum)]
pub enum ColorMode {
    Auto,
    Always,
    Never,
}

#[derive(Debug, Subcommand)]
pub enum Command {
    /// Overall state: components, pools, hosts, alerts, worker instances.
    Status,
    /// Worker pools.
    #[command(subcommand)]
    Pools(PoolsCmd),
    /// Workers (VMs) and their logs.
    #[command(subcommand)]
    Workers(WorkersCmd),
    /// macOS hosts (Mac minis running cucina-hostd) and their enrollment.
    #[command(subcommand)]
    Hosts(HostsCmd),
    /// Build queues per platform and size class.
    Queues,
    /// Queued and executing operations.
    #[command(subcommand)]
    Ops(OpsCmd),
    /// Actions in the CAS/AC.
    #[command(subcommand)]
    Action(ActionCmd),
    /// Log in to a Cucina deployment (OIDC loopback + PKCE, or a service-account key).
    Login(LoginArgs),
    /// Delete this machine's session for a profile (tokens and stored secrets).
    Logout(LogoutArgs),
    /// Show who you are logged in as, your grants and the token's expiry.
    Whoami,
    /// Service-account keys and principal revocation.
    #[command(subcommand)]
    Keys(KeysCmd),
    /// Operator-driven rotation of the Cucina certificate authority.
    #[command(subcommand)]
    Ca(CaCmd),
    /// Print ready-to-use .bazelrc lines for this deployment (UC9).
    Bazelrc(BazelrcArgs),
    /// Bazel credential helper (also reachable as `cucina-credential-helper`).
    #[command(name = "credential-helper", subcommand)]
    CredentialHelper(CredentialHelperCmd),
    /// Spend per pool (estimated from instance-hours and the price list).
    Cost(CostArgs),
    /// Worker image versions per pool.
    Images,
    /// Download a support bundle (secrets redacted by the server).
    Diag(DiagArgs),
    /// Profiles and local configuration.
    #[command(subcommand)]
    Config(ConfigCmd),
    /// Print a static shell completion script.
    Completions {
        /// Shell to generate completions for.
        shell: clap_complete::Shell,
    },
    /// Open the interactive terminal UI.
    Tui,
}

#[derive(Debug, Subcommand)]
pub enum PoolsCmd {
    /// List pools with desired vs actual workers.
    List,
    /// Show one pool: spec, conditions, workers, scale timeline, cold starts.
    Describe {
        /// Pool name.
        name: String,
    },
    /// Keep at least N workers running for a while (standing cost; always expires).
    /// `--min 0` clears the floor.
    ScaleFloor {
        /// Pool name.
        name: String,
        /// Minimum number of running workers (0 clears the floor).
        #[arg(long)]
        min: u32,
        /// How long the floor lasts (e.g. 2h; at most 7d); required unless --min 0.
        #[arg(long = "for", value_parser = duration)]
        duration: Option<Duration>,
    },
    /// Stop (or with --undo, resume) launching new workers for a pool.
    Cordon {
        /// Pool name.
        name: String,
        /// Uncordon instead.
        #[arg(long)]
        undo: bool,
    },
    /// Delete orphaned volumes/ENIs carrying pool tags.
    Gc {
        /// Pool name (default: all pools).
        name: Option<String>,
        /// Only list what would be deleted.
        #[arg(long)]
        dry_run: bool,
    },
}

#[derive(Debug, Subcommand)]
pub enum WorkersCmd {
    /// List workers (VMs).
    List {
        /// Only workers of this pool.
        #[arg(long)]
        pool: Option<String>,
    },
    /// Drain a worker: running actions finish, no new ones start.
    Drain {
        /// Worker node (EC2 instance ID or <host>/<vm>).
        node: String,
    },
    /// Undo a drain.
    Undrain {
        /// Worker node.
        node: String,
    },
    /// Show a worker's logs.
    Logs {
        /// Worker node.
        node: String,
        /// Which log.
        #[arg(long, value_enum, default_value_t = LogUnit::BbWorker)]
        unit: LogUnit,
        /// Number of trailing lines.
        #[arg(long, default_value_t = 200)]
        tail: u32,
        /// Keep streaming new lines.
        #[arg(long, short = 'f')]
        follow: bool,
    },
}

/// Worker log units.
#[derive(Debug, Clone, Copy, PartialEq, Eq, ValueEnum)]
pub enum LogUnit {
    BbWorker,
    BbRunner,
    Agent,
}

#[derive(Debug, Subcommand)]
pub enum HostsCmd {
    /// List Mac hosts with VM slots, images and cache statistics.
    List,
    /// Drain a host for maintenance (its VMs finish their actions and shut down).
    Drain {
        /// Serial number or name.
        host: String,
    },
    /// Return a drained host to service.
    Uncordon {
        /// Serial number or name.
        host: String,
    },
    /// Site enrollment tokens (multi-use, expiring, revocable).
    #[command(name = "enroll-token", subcommand)]
    EnrollToken(EnrollTokenCmd),
    /// Re-clone a host's VMs from the golden image (next start, or now if stopped).
    ReImage {
        /// Serial number or name.
        host: String,
        /// Only this VM.
        #[arg(long)]
        vm: Option<String>,
    },
    /// Collect a host's diagnostics into a file.
    Diag {
        /// Serial number or name.
        host: String,
        /// Output file (default: cucina-host-<host>-<time>.tar.gz).
        #[arg(long, short = 'f')]
        file: Option<PathBuf>,
    },
    /// Pre-register (and approve) serial numbers, e.g. pasted from Apple Business.
    Register {
        /// Serial numbers.
        #[arg(required = true)]
        serials: Vec<String>,
        /// Site the hosts belong to.
        #[arg(long)]
        site: String,
        /// Labels (key=value), repeatable.
        #[arg(long = "label", value_parser = key_value)]
        labels: Vec<(String, String)>,
    },
    /// Approve a pending host enrollment.
    Approve {
        /// Serial number.
        serial: String,
    },
    /// Remove a host (it must re-enroll to come back).
    Remove {
        /// Serial number or name.
        host: String,
    },
}

#[derive(Debug, Subcommand)]
pub enum EnrollTokenCmd {
    /// Create a site enrollment token (shown once).
    Create {
        /// Site the token is bound to.
        #[arg(long)]
        site: String,
        /// Lifetime (e.g. 7d).
        #[arg(long, default_value = "7d", value_parser = duration)]
        ttl: Duration,
        /// Maximum number of hosts that may enroll with it.
        #[arg(long, default_value_t = 10)]
        max_hosts: u32,
        /// Free-form description.
        #[arg(long, default_value = "")]
        description: String,
    },
    /// List enrollment tokens (never their values).
    List,
    /// Revoke a token (blocks new enrollments only).
    Revoke {
        /// Token ID.
        id: String,
    },
}

#[derive(Debug, Subcommand)]
pub enum OpsCmd {
    /// List queued and executing operations.
    List(OpsFilter),
    /// Stream operation changes until interrupted (JSON lines with -o json).
    Watch(OpsFilter),
    /// Kill an operation, or fail every operation queued on a queue without workers.
    Kill {
        /// Operation name.
        #[arg(required_unless_present = "queue_without_workers")]
        operation: Option<String>,
        /// Kill everything queued on this queue (needs --platform).
        #[arg(long, conflicts_with = "operation")]
        queue_without_workers: bool,
        /// Queue platform property (name=value), repeatable.
        #[arg(long = "platform", value_parser = key_value)]
        platform: Vec<(String, String)>,
        /// Queue instance name prefix.
        #[arg(long, default_value = "")]
        instance: String,
        /// Queue size class.
        #[arg(long, default_value_t = 0)]
        size_class: u32,
        /// Message reported to the client.
        #[arg(long, default_value = "killed by cucinactl")]
        message: String,
    },
}

/// Operation filters.
#[derive(Debug, Clone, Args)]
pub struct OpsFilter {
    /// Only operations in this stage.
    #[arg(long, value_enum)]
    pub stage: Option<Stage>,
    /// Only this instance name.
    #[arg(long, default_value = "")]
    pub instance: String,
    /// Only this Bazel invocation ID.
    #[arg(long, default_value = "")]
    pub invocation: String,
    /// Only this queue platform (name=value), repeatable.
    #[arg(long = "platform", value_parser = key_value)]
    pub platform: Vec<(String, String)>,
    /// Page size for `list`.
    #[arg(long, default_value_t = 100)]
    pub limit: u32,
    /// `watch`: stop after this many operation events.
    #[arg(long)]
    pub count: Option<u64>,
}

/// Operation stages.
#[derive(Debug, Clone, Copy, PartialEq, Eq, ValueEnum)]
pub enum Stage {
    Queued,
    Executing,
    Completed,
}

#[derive(Debug, Subcommand)]
pub enum ActionCmd {
    /// Show an action: command, environment, platform, inputs, outputs, exit code,
    /// stdout/stderr, timing and worker.
    Inspect(InspectArgs),
}

/// `action inspect` arguments.
#[derive(Debug, Clone, Args)]
pub struct InspectArgs {
    /// Action digest (<hash>/<size>), a Buildbarn action or historical_execute_response
    /// link, or an operation name.
    pub subject: String,
    /// Instance name (default: the profile's).
    #[arg(long)]
    pub instance: Option<String>,
    /// Maximum input-tree entries to list.
    #[arg(long, default_value_t = 200)]
    pub max_tree_entries: usize,
    /// Maximum bytes of stdout/stderr to show each.
    #[arg(long, default_value_t = 65_536)]
    pub max_output_bytes: usize,
}

/// `login` arguments.
#[derive(Debug, Clone, Args)]
pub struct LoginArgs {
    /// Cucina URL, e.g. https://cucina.example.com (default: the profile's).
    pub url: Option<String>,
    /// Identity provider to use when the deployment offers several.
    #[arg(long)]
    pub provider: Option<String>,
    /// Loopback port for the redirect (e.g. for `ssh -L 127.0.0.1:P:127.0.0.1:P`).
    #[arg(long)]
    pub port: Option<u16>,
    /// Do not open a browser; only print the URL.
    #[arg(long)]
    pub no_browser: bool,
    /// Paste the final redirect URL instead of running a local listener.
    #[arg(long, conflicts_with = "no_browser")]
    pub manual: bool,
    /// Google hosted-domain hint (`hd`).
    #[arg(long)]
    pub hd: Option<String>,
    /// Redirect path (some providers register a path).
    #[arg(long, default_value = "/")]
    pub redirect_path: String,
    /// Run this program with the URL instead of the default browser.
    #[arg(long, env = "CUCINA_BROWSER_COMMAND")]
    pub browser_command: Option<String>,
    /// How long to wait for the browser.
    #[arg(long, default_value = "5m", value_parser = duration)]
    pub login_timeout: Duration,
    /// Log in again even if a stored session can be renewed.
    #[arg(long)]
    pub force: bool,
    /// Service-account key file (break-glass / systems without OIDC); `-` reads stdin.
    #[arg(long, value_name = "FILE")]
    pub key: Option<PathBuf>,
    /// Read the service-account key from this environment variable.
    #[arg(long, value_name = "VAR", conflicts_with = "key")]
    pub key_env: Option<String>,
    /// Where to store the refresh token or key.
    #[arg(long, value_enum)]
    pub credential_store: Option<CredentialStore>,
    /// Extra CA bundle (PEM) for a private CA, stored in the profile (default:
    /// $CUCINA_CA_FILE; $SSL_CERT_FILE is trusted too).
    #[arg(long)]
    pub ca_file: Option<PathBuf>,
}

/// `logout` arguments.
#[derive(Debug, Clone, Args)]
pub struct LogoutArgs {
    /// Log out of every profile.
    #[arg(long)]
    pub all: bool,
    /// Also delete the profile from config.toml.
    #[arg(long)]
    pub forget: bool,
}

#[derive(Debug, Subcommand)]
pub enum KeysCmd {
    /// Create a service-account key (shown once; stored hashed by the server).
    Create {
        /// Service account (TrustPolicy type serviceAccount).
        #[arg(long)]
        account: String,
        /// Description.
        #[arg(long, default_value = "")]
        description: String,
        /// Expiry (default: no expiry).
        #[arg(long, value_parser = duration)]
        ttl: Option<Duration>,
    },
    /// List service-account keys.
    List {
        /// Only this account.
        #[arg(long, default_value = "")]
        account: String,
    },
    /// Revoke a service key, or deny-list a principal (--sub) or session (--sid).
    Revoke {
        /// Key ID.
        #[arg(required_unless_present_any = ["sub", "sid"])]
        key_id: Option<String>,
        /// Principal (JWT `sub`) to deny-list.
        #[arg(long, conflicts_with = "key_id")]
        sub: Option<String>,
        /// Session (JWT `sid`) to deny-list.
        #[arg(long, conflicts_with = "key_id")]
        sid: Option<String>,
        /// Reason (audit log).
        #[arg(long, default_value = "")]
        reason: String,
    },
    /// List deny-list entries.
    Revocations,
}

/// One phase of the operator-driven two-root CA rotation (R-OPS-5/-6).
#[derive(Debug, Clone, Copy, PartialEq, Eq, ValueEnum)]
pub enum CaRotationPhase {
    /// Publish a new root while the current CA keeps signing.
    Introduce,
    /// Switch signers only after every verifier trusts both roots.
    Activate,
    /// Remove the old root only after all old-CA leaves have retired.
    Retire,
}

#[derive(Debug, Subcommand)]
pub enum CaCmd {
    /// Apply one CA phase (cluster admin; no automatic fleet restarts or MDM changes).
    Rotate {
        #[arg(value_enum)]
        phase: CaRotationPhase,
        /// Attest that every verifier already trusts both roots (required for activate).
        #[arg(long, required_if_eq("phase", "activate"))]
        trust_distributed: bool,
        /// Attest that no old-CA leaves remain after the waiting period (required for retire).
        #[arg(long, required_if_eq("phase", "retire"))]
        old_leaves_retired: bool,
    },
}

/// `bazelrc` arguments.
#[derive(Debug, Clone, Args)]
pub struct BazelrcArgs {
    /// Execute on this OS's pool (native configuration).
    #[arg(long, value_enum, required_unless_present_any = ["cross", "emit_build_file", "list_targets"])]
    pub platform: Option<crate::bazelrc::Os>,
    /// Cross configuration for a hermetic-llvm target (needs --target).
    #[arg(long, requires = "target", conflicts_with = "platform")]
    pub cross: bool,
    /// Target platform for --cross (see --list-targets).
    #[arg(long)]
    pub target: Option<String>,
    /// Pool that runs compile actions (default: the cheapest capable pool).
    #[arg(long)]
    pub exec_pool: Option<String>,
    /// macOS: the pool serving this Xcode version.
    #[arg(long)]
    pub xcode: Option<String>,
    /// CI flags (minimal downloads).
    #[arg(long)]
    pub ci: bool,
    /// The principal cannot write the action cache.
    #[arg(long)]
    pub read_only: bool,
    /// Remote cache only, local execution.
    #[arg(long, conflicts_with = "cross")]
    pub cache_only: bool,
    /// Emit lines for `--config=<NAME>` instead of unconditional ones.
    #[arg(long)]
    pub config_name: Option<String>,
    /// Credential helper path (default: the installed cucina-credential-helper).
    #[arg(long)]
    pub helper_path: Option<String>,
    /// Local disk cache directory (`none` disables it).
    #[arg(long)]
    pub disk_cache: Option<String>,
    /// Disk cache size limit.
    #[arg(long, default_value = "50G")]
    pub disk_cache_max_size: String,
    /// Client OS (default: this one): Windows clients get the Linux/macOS action environment
    /// in cross configurations, others the @bazel_tools overlay note for Windows tests.
    #[arg(long, value_enum)]
    pub client_os: Option<crate::bazelrc::Os>,
    /// Use platforms from this package (output of --emit-build-file) instead of @cucina_platforms.
    #[arg(long)]
    pub platforms_package: Option<String>,
    /// Print platform() definitions for every pool runner instead of .bazelrc lines.
    #[arg(long, conflicts_with_all = ["cross", "platform"])]
    pub emit_build_file: bool,
    /// List the cross targets and the pools that may compile them.
    #[arg(long, conflicts_with_all = ["cross", "platform"])]
    pub list_targets: bool,
}

#[derive(Debug, Subcommand)]
pub enum CredentialHelperCmd {
    /// Bazel's `get` request: JSON {"uri": …} on stdin, headers on stdout.
    Get,
    /// Create the `cucina-credential-helper` hardlink (or copy) of this binary.
    Install {
        /// Directory (default: next to cucinactl).
        #[arg(long)]
        dir: Option<PathBuf>,
    },
}

/// `cost` arguments.
#[derive(Debug, Clone, Args)]
pub struct CostArgs {
    /// Only this pool.
    #[arg(long)]
    pub pool: Option<String>,
    /// Start of the period (RFC 3339 or YYYY-MM-DD; default: start of the month).
    #[arg(long)]
    pub since: Option<String>,
}

/// `diag` arguments.
#[derive(Debug, Clone, Args)]
pub struct DiagArgs {
    /// Output file (default: cucina-support-<time>.tar.gz).
    #[arg(long, short = 'f')]
    pub file: Option<PathBuf>,
    /// Include component logs.
    #[arg(long)]
    pub include_logs: bool,
}

#[derive(Debug, Subcommand)]
pub enum ConfigCmd {
    /// Show the configuration (profiles; never secrets).
    View,
    /// List profile names.
    Profiles,
    /// Make a profile the default.
    Use {
        /// Profile name.
        name: String,
    },
    /// Set a profile field (instance-name, management, remote-executor, ca-file, credential-store).
    Set {
        /// Field.
        key: String,
        /// Value.
        value: String,
    },
    /// Delete a profile and its local session.
    Delete {
        /// Profile name.
        name: String,
    },
    /// Print the configuration directory.
    Path,
}

fn key_value(s: &str) -> Result<(String, String), String> {
    s.split_once('=')
        .map(|(k, v)| (k.to_string(), v.to_string()))
        .filter(|(k, _)| !k.is_empty())
        .ok_or_else(|| format!("expected name=value, got {s:?}"))
}

/// Parses arguments and runs; returns the process exit code.
pub fn main(args: Vec<OsString>) -> std::process::ExitCode {
    let cli = match Cli::try_parse_from(args) {
        Ok(cli) => cli,
        Err(e) => {
            let _ = e.print();
            return std::process::ExitCode::from(u8::try_from(e.exit_code()).unwrap_or(2));
        }
    };
    match cli.global.color {
        ColorMode::Auto => {}
        ColorMode::Always => anstream::ColorChoice::Always.write_global(),
        ColorMode::Never => anstream::ColorChoice::Never.write_global(),
    }
    init_tracing(cli.global.verbose, cli.global.output);
    let format = cli.global.output;
    match crate::commands::run(cli) {
        Ok(()) => ExitCode::Ok.into(),
        // The reader went away (e.g. `| head`): nothing left to report.
        Err(err)
            if err.chain().any(|c| {
                c.downcast_ref::<std::io::Error>()
                    .is_some_and(|e| e.kind() == std::io::ErrorKind::BrokenPipe)
            }) =>
        {
            ExitCode::Ok.into()
        }
        Err(err) => {
            let code = crate::exit::exit_code_for(&err);
            // An empty CliError means the failure was already reported.
            let reported = err
                .downcast_ref::<crate::exit::CliError>()
                .is_some_and(|e| e.message.is_empty());
            if !reported {
                crate::output::error(format, code, &err);
            }
            code.into()
        }
    }
}

fn init_tracing(verbose: u8, format: OutputFormat) {
    let default = match verbose {
        // JSON errors must remain machine-readable. Debug logs on stderr are opt-in
        // with --verbose or CUCINA_LOG, including diagnostics from TLS dependencies.
        0 if format == OutputFormat::Json => "off",
        0 => "warn",
        1 => "cucinactl=debug,info",
        _ => "trace",
    };
    let filter = tracing_subscriber::EnvFilter::try_from_env("CUCINA_LOG")
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new(default));
    let _ = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_writer(std::io::stderr)
        .with_target(false)
        .try_init();
}

/// The clap command (completions, docs).
pub fn command() -> clap::Command {
    Cli::command()
}

/// Markdown command reference for docs/cli.md (`cargo xtask docs`).
pub fn markdown_reference() -> String {
    let mut out = String::new();
    let mut cmd = command();
    cmd.build();
    render_markdown(&cmd, "cucinactl", &mut out);
    out
}

fn render_markdown(cmd: &clap::Command, path: &str, out: &mut String) {
    let level = path.split(' ').count() + 2;
    out.push_str(&format!("{} `{path}`\n\n", "#".repeat(level.min(6))));
    if let Some(about) = cmd.get_long_about().or(cmd.get_about()) {
        out.push_str(&format!("{}\n\n", about.to_string().trim()));
    }
    let mut c = cmd.clone().bin_name(path).disable_help_subcommand(true);
    let usage = c.render_usage().to_string();
    out.push_str(&format!("```text\n{}\n```\n\n", usage.trim()));
    let args: Vec<&clap::Arg> = cmd
        .get_arguments()
        .filter(|a| {
            // Global options are listed once, on the root command.
            let root = !path.contains(' ');
            !a.is_hide_set()
                && (root || !a.is_global_set())
                && a.get_id() != "help"
                && a.get_id() != "version"
        })
        .collect();
    if !args.is_empty() {
        out.push_str("| Argument | Description |\n| --- | --- |\n");
        for a in args {
            let name = match (a.get_long(), a.get_short()) {
                (Some(l), Some(s)) => format!("`-{s}`, `--{l}`"),
                (Some(l), None) => format!("`--{l}`"),
                (None, Some(s)) => format!("`-{s}`"),
                (None, None) => format!("`<{}>`", a.get_id().as_str().to_ascii_uppercase()),
            };
            let mut help = a.get_help().map(|h| h.to_string()).unwrap_or_default();
            let defaults: Vec<String> = a
                .get_default_values()
                .iter()
                .map(|v| v.to_string_lossy().to_string())
                .filter(|v| !v.is_empty())
                .collect();
            if !defaults.is_empty() && a.get_action().takes_values() {
                help.push_str(&format!(" (default: `{}`)", defaults.join(",")));
            }
            let values: Vec<String> = a
                .get_possible_values()
                .iter()
                .filter(|v| !v.is_hide_set())
                .map(|v| v.get_name().to_string())
                .collect();
            if !values.is_empty() && values.len() <= 12 {
                help.push_str(&format!(" [values: {}]", values.join(", ")));
            }
            out.push_str(&format!("| {name} | {} |\n", help.replace('|', "\\|")));
        }
        out.push('\n');
    }
    for sub in cmd
        .get_subcommands()
        .filter(|s| !s.is_hide_set() && s.get_name() != "help")
    {
        render_markdown(sub, &format!("{path} {}", sub.get_name()), out);
    }
}
