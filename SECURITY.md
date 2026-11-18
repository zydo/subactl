# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately, not in a public issue: open the
repository's **Security** tab and choose **Report a vulnerability**
([direct link](https://github.com/zydo/subactl/security/advisories/new)).
Include what you did, what happened, what you expected, and the platform
(Linux kernel version or macOS version, and the backend). The report stays
private until a fix is out.

## Supported versions

subactl is in early development and has no releases yet. Only the latest
commit on `main` is supported; fixes land there.

## Scope

`subactld` runs as root on Linux, so these are in scope and taken
seriously:

- a process in a lease leaving its cgroup, or outliving its lease;
- an agent gaining privileges it did not start with (subactl sets
  `no_new_privs` for every lease);
- one lead, or a process in it, reading, signalling or controlling
  another lead's leases through the daemon;
- the daemon admitting more memory than it should, or failing to end a
  lease that is over its limit;
- injecting text into an agent's inbox from a source that is not subactl
  or one of the agent's ancestors.

Out of scope, because the [design](docs/design.md) excludes them
explicitly:

- escapes through a third party started over IPC (`systemd-run --user`, a
  tmux server, the docker daemon, launchd); subactl does not sandbox;
- access to files or the network; subactl is not a file system or
  network sandbox;
- limits on macOS overshooting between two checks: the macOS backend only
  monitors and says so (`monitor_only`).
