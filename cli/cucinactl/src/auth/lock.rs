// SPDX-License-Identifier: FSL-1.1-ALv2

//! Cross-process advisory file lock serializing token renewals and secret-file
//! updates. Bazel runs the credential helper concurrently (once per gRPC service),
//! so only one process may renew a session at a time; the others wait and then
//! read the renewed token. Unix: `flock(2)` (rustix). Windows: an exclusive
//! share-mode open (no other handle can open the lock file while it is held).
//! The lock is released when the [`FileLock`] is dropped (or the process dies).

use std::fs::File;
use std::path::Path;
use std::time::{Duration, Instant};

use anyhow::{Context, Result};

use crate::exit::CliError;

/// A held lock; dropping it releases the lock.
#[derive(Debug)]
pub struct FileLock {
    _file: File,
}

const POLL: Duration = Duration::from_millis(5);

/// Acquires an exclusive lock on `path` (created if missing), waiting up to `timeout`.
pub fn lock_exclusive(path: &Path, timeout: Duration) -> Result<FileLock> {
    if let Some(dir) = path.parent() {
        crate::config::ensure_private_dir(dir)?;
    }
    let deadline = Instant::now() + timeout;
    loop {
        match try_lock(path)? {
            Some(file) => return Ok(FileLock { _file: file }),
            None if Instant::now() >= deadline => {
                return Err(CliError::unavailable(format!(
                    "timed out after {timeout:?} waiting for the lock {} (another cucinactl is renewing the session)",
                    path.display()
                ))
                .into());
            }
            None => std::thread::sleep(POLL),
        }
    }
}

#[cfg(unix)]
fn try_lock(path: &Path) -> Result<Option<File>> {
    use std::os::unix::fs::OpenOptionsExt as _;
    let file = std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(path)
        .with_context(|| format!("opening lock file {}", path.display()))?;
    match rustix::fs::flock(&file, rustix::fs::FlockOperation::NonBlockingLockExclusive) {
        Ok(()) => Ok(Some(file)),
        Err(e) if e == rustix::io::Errno::WOULDBLOCK || e == rustix::io::Errno::INTR => Ok(None),
        Err(e) => {
            Err(std::io::Error::from(e)).with_context(|| format!("locking {}", path.display()))
        }
    }
}

#[cfg(windows)]
fn try_lock(path: &Path) -> Result<Option<File>> {
    use std::os::windows::fs::OpenOptionsExt as _;
    // ERROR_SHARING_VIOLATION (32) / ERROR_LOCK_VIOLATION (33): someone holds it.
    match std::fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .share_mode(0)
        .open(path)
    {
        Ok(file) => Ok(Some(file)),
        Err(e) if matches!(e.raw_os_error(), Some(32) | Some(33)) => Ok(None),
        Err(e) if e.kind() == std::io::ErrorKind::PermissionDenied => Ok(None),
        Err(e) => Err(e).with_context(|| format!("opening lock file {}", path.display())),
    }
}
