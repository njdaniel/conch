//! A spawned process that is killed and waited for when the test lets go of it, on every
//! path: the end of the test, an assertion that fails, a panic, a timeout.
//!
//! Without it a test that panics drops its [`Child`], which neither kills nor waits for it.
//! A `conch-voice join` with a key device to watch does not leave at the end of its standard
//! input (conch-voice.md, "Exit codes"), so one so dropped runs on, with a retry loop, until
//! someone sends it a signal (#1157). Every test that starts the binary holds it through
//! this type.

use std::io;
use std::ops::{Deref, DerefMut};
#[cfg(unix)]
use std::os::unix::process::CommandExt;
use std::process::{Child, Command};

/// A child process that is killed and reaped when this is dropped.
pub struct Reaped {
    child: Child,
    /// Whether the child leads a process group of its own, which is then killed whole.
    #[cfg(unix)]
    group: bool,
}

impl Reaped {
    /// Starts the command in this test's process group, as `Command::spawn` does.
    pub fn spawn(command: &mut Command) -> io::Result<Self> {
        Ok(Self {
            child: command.spawn()?,
            #[cfg(unix)]
            group: false,
        })
    }

    /// Starts the command as the leader of a process group of its own, and at the end kills
    /// the whole group: for a shell that leaves the binary running in the background.
    #[cfg(unix)]
    pub fn spawn_in_group(command: &mut Command) -> io::Result<Self> {
        Ok(Self {
            child: command.process_group(0).spawn()?,
            group: true,
        })
    }
}

impl Deref for Reaped {
    type Target = Child;

    fn deref(&self) -> &Child {
        &self.child
    }
}

impl DerefMut for Reaped {
    fn deref_mut(&mut self) -> &mut Child {
        &mut self.child
    }
}

impl Drop for Reaped {
    fn drop(&mut self) {
        #[cfg(unix)]
        if self.group {
            // The group is named by its leader's pid, which is not reused while the leader
            // has not been waited for. A group that is already gone is not an error.
            let _ = Command::new("kill")
                .args(["-KILL", "--", &format!("-{}", self.child.id())])
                .stderr(std::process::Stdio::null())
                .status();
        }
        // Errors mean the process has already been waited for or is already gone.
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}
