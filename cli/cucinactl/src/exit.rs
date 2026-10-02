// SPDX-License-Identifier: FSL-1.1-ALv2

//! Stable, documented exit codes (docs/cli.md §Exit codes) and the error type that
//! carries one. Every failure path maps to exactly one code; scripts may rely on them.

use std::fmt;

use serde::Serialize;

/// Process exit codes of `cucinactl` (and `cucina-credential-helper`).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ExitCode {
    /// 0: success.
    Ok,
    /// 1: any other error (server internal error, I/O, unexpected response, …).
    Error,
    /// 2: usage error (bad flags or arguments, refused confirmation, invalid input).
    Usage,
    /// 3: authentication required (no session, expired/revoked session, `UNAUTHENTICATED`).
    AuthRequired,
    /// 4: permission denied (`PERMISSION_DENIED`, STS `access_denied`).
    PermissionDenied,
    /// 5: not found (`NOT_FOUND`, unknown pool/host/operation/digest).
    NotFound,
    /// 6: unavailable (cannot connect, `UNAVAILABLE`, `DEADLINE_EXCEEDED`, rate limited).
    Unavailable,
    /// 7: conflict or failed precondition (`ALREADY_EXISTS`, `FAILED_PRECONDITION`, `ABORTED`).
    Conflict,
}

impl ExitCode {
    /// Every code, in numeric order (used by the docs and the exit-code test).
    pub const ALL: [ExitCode; 8] = [
        ExitCode::Ok,
        ExitCode::Error,
        ExitCode::Usage,
        ExitCode::AuthRequired,
        ExitCode::PermissionDenied,
        ExitCode::NotFound,
        ExitCode::Unavailable,
        ExitCode::Conflict,
    ];

    /// The numeric process exit status.
    pub fn code(self) -> u8 {
        match self {
            ExitCode::Ok => 0,
            ExitCode::Error => 1,
            ExitCode::Usage => 2,
            ExitCode::AuthRequired => 3,
            ExitCode::PermissionDenied => 4,
            ExitCode::NotFound => 5,
            ExitCode::Unavailable => 6,
            ExitCode::Conflict => 7,
        }
    }

    /// Short machine-readable name (used in `--output json` error objects).
    pub fn name(self) -> &'static str {
        match self {
            ExitCode::Ok => "ok",
            ExitCode::Error => "error",
            ExitCode::Usage => "usage",
            ExitCode::AuthRequired => "auth_required",
            ExitCode::PermissionDenied => "permission_denied",
            ExitCode::NotFound => "not_found",
            ExitCode::Unavailable => "unavailable",
            ExitCode::Conflict => "conflict",
        }
    }

    /// Maps a gRPC/Connect status code (management API, REAPI) to an exit code.
    pub fn from_rpc(code: connectrpc::ErrorCode) -> ExitCode {
        use connectrpc::ErrorCode as C;
        match code {
            C::InvalidArgument | C::OutOfRange => ExitCode::Usage,
            C::Unauthenticated => ExitCode::AuthRequired,
            C::PermissionDenied => ExitCode::PermissionDenied,
            C::NotFound => ExitCode::NotFound,
            C::Unavailable | C::DeadlineExceeded | C::ResourceExhausted => ExitCode::Unavailable,
            C::AlreadyExists | C::FailedPrecondition | C::Aborted => ExitCode::Conflict,
            _ => ExitCode::Error,
        }
    }
}

impl From<ExitCode> for std::process::ExitCode {
    fn from(code: ExitCode) -> Self {
        std::process::ExitCode::from(code.code())
    }
}

/// An error with an explicit exit code. Anything else maps through [`exit_code_for`].
#[derive(Debug)]
pub struct CliError {
    pub code: ExitCode,
    pub message: String,
}

impl CliError {
    pub fn new(code: ExitCode, message: impl Into<String>) -> Self {
        CliError {
            code,
            message: message.into(),
        }
    }
    pub fn usage(message: impl Into<String>) -> Self {
        Self::new(ExitCode::Usage, message)
    }
    pub fn auth_required(message: impl Into<String>) -> Self {
        Self::new(ExitCode::AuthRequired, message)
    }
    pub fn permission_denied(message: impl Into<String>) -> Self {
        Self::new(ExitCode::PermissionDenied, message)
    }
    pub fn not_found(message: impl Into<String>) -> Self {
        Self::new(ExitCode::NotFound, message)
    }
    pub fn unavailable(message: impl Into<String>) -> Self {
        Self::new(ExitCode::Unavailable, message)
    }
    pub fn conflict(message: impl Into<String>) -> Self {
        Self::new(ExitCode::Conflict, message)
    }
}

impl fmt::Display for CliError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.message)
    }
}

impl std::error::Error for CliError {}

/// Determines the exit code for an error by walking its cause chain: an explicit
/// [`CliError`] wins, then gRPC statuses, then transport-level failures.
pub fn exit_code_for(err: &anyhow::Error) -> ExitCode {
    for cause in err.chain() {
        if let Some(e) = cause.downcast_ref::<CliError>() {
            return e.code;
        }
        if let Some(e) = cause.downcast_ref::<connectrpc::ConnectError>() {
            return ExitCode::from_rpc(e.code);
        }
        if let Some(e) = cause.downcast_ref::<reqwest::Error>()
            && (e.is_connect() || e.is_timeout())
        {
            return ExitCode::Unavailable;
        }
        if let Some(e) = cause.downcast_ref::<std::io::Error>()
            && matches!(
                e.kind(),
                std::io::ErrorKind::ConnectionRefused
                    | std::io::ErrorKind::ConnectionReset
                    | std::io::ErrorKind::TimedOut
            )
        {
            return ExitCode::Unavailable;
        }
    }
    ExitCode::Error
}
