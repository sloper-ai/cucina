// SPDX-License-Identifier: FSL-1.1-ALv2

//! `cucinactl action inspect <digest|operation>` (UC13, R-CLI-3): command,
//! environment, platform, input tree, outputs, exit code, stdout/stderr, timing and
//! worker of one action, read from the CAS/AC through the client endpoint with the
//! user's own credentials.
//!
//! Accepted subjects:
//! * an action digest `<hash>/<size>` or `<hash>-<size>`;
//! * a link printed by Buildbarn in Bazel's output, e.g.
//!   `…/<instance>/blobs/sha256/action/<hash>-<size>/` (cached result) or
//!   `…/blobs/sha256/historical_execute_response/<hash>-<size>/` (uncached, e.g.
//!   failed, result stored by `bb_worker` in the CAS);
//! * anything else is an operation name, resolved through the management API; a
//!   completed operation also carries the scheduler's `ExecuteResponse` (exit code,
//!   stdout/stderr, timing of a just-failed action the action cache never stores),
//!   otherwise the result comes from the action cache.

use std::collections::BTreeMap;
use std::io::{self, Write};

use anyhow::{Context, Result};
use cucina_api::proto::build::bazel::remote::execution::v2 as re;
use cucina_api::proto::buildbarn::cas::HistoricalExecuteResponse;
use serde::Serialize;

use crate::client::Session;
use crate::client::reapi::{Reapi, TreeListing, digest_string, parse_digest};
use crate::exit::CliError;
use crate::output::{self, Render};
use crate::util::proto_time;

/// What to inspect.
#[derive(Debug, Clone, PartialEq)]
pub enum Subject {
    Action(re::Digest),
    Historical(re::Digest),
    Operation(String),
}

/// Parses the command-line subject; also returns an instance name found in a link.
pub fn parse_subject(input: &str) -> (Subject, Option<String>) {
    let s = input.trim();
    if s.contains("/blobs/") {
        let path = s
            .split_once("://")
            .map_or(s, |(_, rest)| rest.split_once('/').map_or("", |(_, p)| p));
        let segments: Vec<&str> = path.split('/').filter(|p| !p.is_empty()).collect();
        if let Some(i) = segments.iter().position(|p| *p == "blobs")
            && segments.len() > i + 3
            && let Some(d) = parse_digest(segments[i + 3])
        {
            let instance = segments[..i].join("/");
            let instance = (!instance.is_empty()).then_some(instance);
            return match segments[i + 2] {
                "historical_execute_response" => (Subject::Historical(d), instance),
                _ => (Subject::Action(d), instance),
            };
        }
    }
    match parse_digest(s) {
        Some(d) => (Subject::Action(d), None),
        None => (Subject::Operation(s.to_string()), None),
    }
}

/// Limits for one inspection.
#[derive(Debug, Clone)]
pub struct InspectOptions {
    pub instance: String,
    pub max_tree_entries: usize,
    pub max_output_bytes: usize,
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct OperationInfo {
    pub name: String,
    pub stage: String,
    pub queued_at: Option<String>,
    pub instance_name: String,
    pub platform: BTreeMap<String, String>,
    pub size_class: u32,
    pub invocation_id: String,
    pub target_id: String,
    pub worker_node: String,
    pub worker_thread: u32,
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct CommandInfo {
    pub arguments: Vec<String>,
    pub environment: BTreeMap<String, String>,
    pub working_directory: String,
    pub output_paths: Vec<String>,
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct OutputFile {
    pub path: String,
    pub digest: Option<String>,
    pub size_bytes: u64,
    pub executable: bool,
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct OutputLink {
    pub path: String,
    pub target: String,
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct StreamText {
    pub digest: Option<String>,
    pub size_bytes: u64,
    /// UTF-8 (lossy) text, possibly truncated.
    pub text: String,
    pub truncated: bool,
}

#[derive(Debug, Clone, Serialize, PartialEq, Default)]
pub struct Timing {
    pub queued_at: Option<String>,
    pub worker_start: Option<String>,
    pub worker_completed: Option<String>,
    pub queue_seconds: Option<f64>,
    pub input_fetch_seconds: Option<f64>,
    pub execution_seconds: Option<f64>,
    pub output_upload_seconds: Option<f64>,
    pub total_seconds: Option<f64>,
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ResultInfo {
    /// `action-cache`, `execute-response` (the operation's, from the scheduler) or
    /// `historical-execute-response`.
    pub source: String,
    pub exit_code: i32,
    pub worker: String,
    pub output_files: Vec<OutputFile>,
    pub output_directories: Vec<OutputFile>,
    pub output_symlinks: Vec<OutputLink>,
    pub stdout: Option<StreamText>,
    pub stderr: Option<StreamText>,
    pub timing: Timing,
}

/// The `action.v1` document.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct ActionView {
    pub instance_name: String,
    pub action_digest: String,
    pub operation: Option<OperationInfo>,
    pub command: CommandInfo,
    pub platform: BTreeMap<String, String>,
    pub timeout_seconds: Option<f64>,
    pub do_not_cache: bool,
    pub input_root: TreeListing,
    pub result: Option<ResultInfo>,
    /// Server message or status of the execution (e.g. why it failed).
    pub message: Option<String>,
}

fn props(p: Option<&re::Platform>) -> BTreeMap<String, String> {
    p.map(|p| {
        p.properties
            .iter()
            .map(|kv| (kv.name.clone(), kv.value.clone()))
            .collect()
    })
    .unwrap_or_default()
}

fn secs_between(
    a: Option<&crate::util::PbTimestamp>,
    b: Option<&crate::util::PbTimestamp>,
) -> Option<f64> {
    let (a, b) = (a?, b?);
    if a.seconds == 0 || b.seconds == 0 {
        return None;
    }
    let d = (b.seconds - a.seconds) as f64 + f64::from(b.nanos - a.nanos) / 1e9;
    (d >= 0.0).then_some(d)
}

fn timing(m: Option<&re::ExecutedActionMetadata>) -> Timing {
    let Some(m) = m else {
        return Timing::default();
    };
    Timing {
        queued_at: proto_time(m.queued_timestamp.as_option()),
        worker_start: proto_time(m.worker_start_timestamp.as_option()),
        worker_completed: proto_time(m.worker_completed_timestamp.as_option()),
        queue_seconds: secs_between(
            m.queued_timestamp.as_option(),
            m.worker_start_timestamp.as_option(),
        ),
        input_fetch_seconds: secs_between(
            m.input_fetch_start_timestamp.as_option(),
            m.input_fetch_completed_timestamp.as_option(),
        ),
        execution_seconds: secs_between(
            m.execution_start_timestamp.as_option(),
            m.execution_completed_timestamp.as_option(),
        ),
        output_upload_seconds: secs_between(
            m.output_upload_start_timestamp.as_option(),
            m.output_upload_completed_timestamp.as_option(),
        ),
        total_seconds: secs_between(
            m.queued_timestamp.as_option(),
            m.worker_completed_timestamp.as_option(),
        ),
    }
}

async fn stream_text(
    reapi: &Reapi,
    raw: &[u8],
    digest: Option<&re::Digest>,
    max: usize,
) -> Result<Option<StreamText>> {
    let (bytes, digest, size) = if !raw.is_empty() {
        (raw.to_vec(), digest.map(digest_string), raw.len() as u64)
    } else if let Some(d) = digest.filter(|d| d.size_bytes > 0) {
        (
            reapi.read_blob(d).await?,
            Some(digest_string(d)),
            d.size_bytes as u64,
        )
    } else {
        return Ok(None);
    };
    let truncated = bytes.len() > max;
    let text = String::from_utf8_lossy(&bytes[..bytes.len().min(max)]).into_owned();
    Ok(Some(StreamText {
        digest,
        size_bytes: size,
        text,
        truncated,
    }))
}

async fn result_info(
    reapi: &Reapi,
    r: &re::ActionResult,
    source: &str,
    opts: &InspectOptions,
) -> Result<ResultInfo> {
    let file = |path: &str, d: Option<&re::Digest>, exec: bool| OutputFile {
        path: path.to_string(),
        digest: d.map(digest_string),
        size_bytes: d.map_or(0, |d| d.size_bytes.max(0) as u64),
        executable: exec,
    };
    // Older servers fill the deprecated per-kind symlink lists instead.
    #[allow(deprecated)]
    let legacy = r
        .output_file_symlinks
        .iter()
        .chain(&r.output_directory_symlinks);
    let mut links: Vec<OutputLink> = r
        .output_symlinks
        .iter()
        .chain(legacy)
        .map(|s| OutputLink {
            path: s.path.clone(),
            target: s.target.clone(),
        })
        .collect();
    links.dedup();
    Ok(ResultInfo {
        source: source.to_string(),
        exit_code: r.exit_code,
        worker: r
            .execution_metadata
            .as_option()
            .map(|m| m.worker.clone())
            .unwrap_or_default(),
        output_files: r
            .output_files
            .iter()
            .map(|f| file(&f.path, f.digest.as_option(), f.is_executable))
            .collect(),
        output_directories: r
            .output_directories
            .iter()
            .map(|d| file(&d.path, d.tree_digest.as_option(), false))
            .collect(),
        output_symlinks: links,
        stdout: stream_text(
            reapi,
            &r.stdout_raw,
            r.stdout_digest.as_option(),
            opts.max_output_bytes,
        )
        .await?,
        stderr: stream_text(
            reapi,
            &r.stderr_raw,
            r.stderr_digest.as_option(),
            opts.max_output_bytes,
        )
        .await?,
        timing: timing(r.execution_metadata.as_option()),
    })
}

/// Inspects one action.
pub async fn inspect(session: &Session, input: &str, opts: &InspectOptions) -> Result<ActionView> {
    let (subject, link_instance) = parse_subject(input);
    let mut instance = link_instance.unwrap_or_else(|| opts.instance.clone());
    let mut operation = None;
    // A completed execution's response and where it came from.
    let mut completed: Option<(re::ExecuteResponse, &'static str)> = None;

    let action_digest = match &subject {
        Subject::Action(d) => d.clone(),
        Subject::Operation(name) => {
            let response = session
                .management
                .get_operation(cucina_api::proto::cucina::v1::GetOperationRequest {
                    name: name.clone(),
                    ..Default::default()
                })
                .await
                .with_context(|| format!("looking up operation {name:?}"))?;
            let op = response
                .operation
                .as_option()
                .cloned()
                .ok_or_else(|| CliError::not_found(format!("operation {name:?} not found")))?;
            if !response.execute_response.is_empty() {
                let resp = <re::ExecuteResponse as buffa::Message>::decode_from_slice(
                    &response.execute_response,
                )
                .with_context(|| format!("decoding the ExecuteResponse of operation {name:?}"))?;
                let source = if resp.cached_result {
                    "action-cache"
                } else {
                    "execute-response"
                };
                completed = Some((resp, source));
            }
            let digest = parse_digest(&op.action_digest).ok_or_else(|| {
                anyhow::anyhow!(
                    "operation {name:?} has an invalid action digest {:?}",
                    op.action_digest
                )
            })?;
            let queue = op.queue.as_option().cloned().unwrap_or_default();
            if !queue.instance_name_prefix.is_empty() {
                instance = queue.instance_name_prefix.clone();
            }
            operation = Some(OperationInfo {
                name: op.name.clone(),
                stage: op.stage.clone(),
                queued_at: proto_time(op.queued_at.as_option()),
                instance_name: queue.instance_name_prefix.clone(),
                platform: queue
                    .platform
                    .iter()
                    .map(|p| (p.name.clone(), p.value.clone()))
                    .collect(),
                size_class: queue.size_class,
                invocation_id: op.invocation_id.clone(),
                target_id: op.target_id.clone(),
                worker_node: op.worker_node.clone(),
                worker_thread: op.worker_thread,
            });
            digest
        }
        Subject::Historical(d) => {
            let reapi = session.reapi(&instance)?;
            let h: HistoricalExecuteResponse = reapi.read_proto(d).await?;
            completed = h
                .execute_response
                .as_option()
                .cloned()
                .map(|r| (r, "historical-execute-response"));
            h.action_digest
                .as_option()
                .cloned()
                .context("historical execute response without an action digest")?
        }
    };

    let reapi = session.reapi(&instance)?;
    let action: re::Action = reapi
        .read_proto(&action_digest)
        .await
        .with_context(|| format!("reading action {}", digest_string(&action_digest)))?;
    let command_digest = action
        .command_digest
        .as_option()
        .cloned()
        .context("action has no command digest")?;
    let command: re::Command = reapi
        .read_proto(&command_digest)
        .await
        .context("reading the command")?;
    let input_root = match action.input_root_digest.as_option() {
        Some(d) => reapi.walk_tree(d, opts.max_tree_entries).await?,
        None => TreeListing::default(),
    };

    let mut message = None;
    let result = if let Some((resp, source)) = &completed {
        message = (!resp.message.is_empty()).then(|| resp.message.clone());
        if let Some(st) = resp.status.as_option().filter(|s| s.code != 0) {
            message = Some(format!("status {}: {}", st.code, st.message));
        }
        match resp.result.as_option() {
            Some(r) => Some(result_info(&reapi, r, source, opts).await?),
            None => None,
        }
    } else {
        match reapi.get_action_result(&action_digest).await? {
            Some(r) => Some(result_info(&reapi, &r, "action-cache", opts).await?),
            None => {
                message = Some(
                    "no cached result: the action has not completed, failed (inspect its \
                     operation while the scheduler remembers it, or the \
                     historical_execute_response link Buildbarn printed), or was evicted"
                        .into(),
                );
                None
            }
        }
    };

    let mut output_paths = command.output_paths.clone();
    if output_paths.is_empty() {
        #[allow(deprecated)]
        {
            output_paths.extend(command.output_files.iter().cloned());
            output_paths.extend(command.output_directories.iter().cloned());
        }
    }
    #[allow(deprecated)]
    let platform = if action.platform.is_set() {
        props(action.platform.as_option())
    } else {
        props(command.platform.as_option())
    };
    Ok(ActionView {
        instance_name: instance,
        action_digest: digest_string(&action_digest),
        operation,
        command: CommandInfo {
            arguments: command.arguments.clone(),
            environment: command
                .environment_variables
                .iter()
                .map(|e| (e.name.clone(), e.value.clone()))
                .collect(),
            working_directory: command.working_directory.clone(),
            output_paths,
        },
        platform,
        timeout_seconds: crate::util::proto_secs(action.timeout.as_option()),
        do_not_cache: action.do_not_cache,
        input_root,
        result,
        message,
    })
}

fn shell_join(args: &[String]) -> String {
    args.iter()
        .map(|a| {
            if !a.is_empty()
                && a.chars()
                    .all(|c| c.is_ascii_alphanumeric() || "-_./=:,+@%".contains(c))
            {
                a.clone()
            } else {
                format!("'{}'", a.replace('\'', r"'\''"))
            }
        })
        .collect::<Vec<_>>()
        .join(" ")
}

impl Render for ActionView {
    const SCHEMA: &'static str = "action.v1";

    fn human(&self, out: &mut dyn Write) -> io::Result<()> {
        let mut fields = vec![
            ("Action", self.action_digest.clone()),
            ("Instance", self.instance_name.clone()),
        ];
        if let Some(op) = &self.operation {
            fields.push((
                "Operation",
                format!("{} ({})", op.name, output::state(&op.stage)),
            ));
            if !op.invocation_id.is_empty() {
                fields.push(("Invocation", op.invocation_id.clone()));
            }
            if !op.worker_node.is_empty() {
                fields.push((
                    "Worker",
                    format!("{} thread {}", op.worker_node, op.worker_thread),
                ));
            }
        }
        let platform: Vec<String> = self
            .platform
            .iter()
            .map(|(k, v)| format!("{k}={v}"))
            .collect();
        fields.push(("Platform", output::dash(&platform.join(", "))));
        if let Some(t) = self.timeout_seconds {
            fields.push(("Timeout", crate::util::human_secs(Some(t))));
        }
        if self.do_not_cache {
            fields.push(("Caching", "do_not_cache".into()));
        }
        output::write_fields(out, &fields)?;

        writeln!(out)?;
        output::heading(out, "Command")?;
        writeln!(out, "  {}", shell_join(&self.command.arguments))?;
        if !self.command.working_directory.is_empty() {
            writeln!(out, "  (in {})", self.command.working_directory)?;
        }
        output::heading(out, "Environment")?;
        for (k, v) in &self.command.environment {
            writeln!(out, "  {k}={v}")?;
        }
        output::heading(out, "Outputs requested")?;
        for p in &self.command.output_paths {
            writeln!(out, "  {p}")?;
        }

        writeln!(out)?;
        let t = &self.input_root;
        output::heading(out, "Input root")?;
        writeln!(
            out,
            "  {}  {} files, {} directories, {} symlinks, {} bytes{}",
            t.root_digest,
            t.files,
            t.directories,
            t.symlinks,
            t.total_file_bytes,
            if t.truncated {
                " (truncated listing)"
            } else {
                ""
            }
        )?;
        for e in &t.entries {
            let detail = match e.kind {
                "file" => format!(
                    "{}{}",
                    e.size_bytes,
                    if e.executable { " (executable)" } else { "" }
                ),
                "symlink" => format!("-> {}", e.target.as_deref().unwrap_or_default()),
                _ => "/".into(),
            };
            writeln!(out, "  {}  {detail}", e.path)?;
        }

        writeln!(out)?;
        output::heading(out, "Result")?;
        match &self.result {
            None => writeln!(out, "  none")?,
            Some(r) => {
                let code = if r.exit_code == 0 {
                    output::state("ok")
                } else {
                    output::state("failed")
                };
                writeln!(
                    out,
                    "  exit code {} ({code}), from {}",
                    r.exit_code, r.source
                )?;
                if !r.worker.is_empty() {
                    writeln!(out, "  worker {}", r.worker)?;
                }
                let tm = &r.timing;
                let parts = [
                    ("queued", tm.queue_seconds),
                    ("input fetch", tm.input_fetch_seconds),
                    ("execution", tm.execution_seconds),
                    ("output upload", tm.output_upload_seconds),
                    ("total", tm.total_seconds),
                ];
                let timing: Vec<String> = parts
                    .iter()
                    .filter_map(|(n, s)| s.map(|s| format!("{n} {s:.3}s")))
                    .collect();
                if !timing.is_empty() {
                    writeln!(out, "  timing: {}", timing.join(", "))?;
                }
                if let Some(q) = &tm.queued_at {
                    writeln!(out, "  queued at {q}")?;
                }
                for f in &r.output_files {
                    writeln!(
                        out,
                        "  output {}  {}  {} bytes",
                        f.path,
                        f.digest.as_deref().unwrap_or("-"),
                        f.size_bytes
                    )?;
                }
                for d in &r.output_directories {
                    writeln!(
                        out,
                        "  output dir {}  tree {}",
                        d.path,
                        d.digest.as_deref().unwrap_or("-")
                    )?;
                }
                for l in &r.output_symlinks {
                    writeln!(out, "  output link {} -> {}", l.path, l.target)?;
                }
                for (name, s) in [("stdout", &r.stdout), ("stderr", &r.stderr)] {
                    if let Some(s) = s {
                        output::heading(out, &format!("{name} ({} bytes)", s.size_bytes))?;
                        write!(out, "{}", s.text)?;
                        if !s.text.ends_with('\n') {
                            writeln!(out)?;
                        }
                        if s.truncated {
                            writeln!(out, "… (truncated; raise --max-output-bytes)")?;
                        }
                    }
                }
            }
        }
        if let Some(m) = &self.message {
            writeln!(out, "  note: {m}")?;
        }
        Ok(())
    }
}
