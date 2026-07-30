# Runtime packaging research

Date: 2026-07-30

## Question

What platform-native package behavior is required for a machine-level Runtime
whose main process is unprivileged, whose network helper is privileged, and
whose uninstall must restore direct networking before removing services?

## Sources

- Debian Policy, [Maintainer Scripts](https://www.debian.org/doc/debian-policy/ch-maintainerscripts.html):
  package scripts must be idempotent, cope with partially completed operations,
  and stop on unhandled errors. This is why the DEB paths use bounded
  `preinst`, `postinst`, `prerm`, and `postrm` scripts with `set -e`.
- Microsoft, [ServiceInstall table](https://learn.microsoft.com/en-us/windows/win32/msi/serviceinstall-table)
  and [ServiceControl table](https://learn.microsoft.com/en-us/windows/win32/msi/servicecontrol-table):
  MSI owns service creation, start/stop, and deletion. The WiX source maps the
  Runtime and network helper to separate service rows.
- WiX Toolset, [ServiceInstall](https://docs.firegiant.com/wix/schema/wxs/serviceinstall/),
  [PermissionEx](https://docs.firegiant.com/wix/schema/wxs/permissionex/), and
  [Util Group](https://docs.firegiant.com/wix/schema/util/group/): the package
  can install a virtual-account service, apply an explicit SDDL, create a local
  operator group, and persist a desktop user membership without creating that
  user.
- Apple, [Packaging Mac software for distribution](https://developer.apple.com/documentation/xcode/packaging-mac-software-for-distribution):
  `pkgbuild` and `productbuild` are the platform package tools, and Installer
  signing uses a Developer ID Installer identity distinct from application
  signing.
- Apple `productbuild(1)`, mirrored at
  [manp.gs](https://manp.gs/mac/1/productbuild): a distribution file can bind a
  component package, allowed OS versions, architectures, and install domain
  into one deployable PKG.

## Applied decisions

- Linux uses one low-privilege system account and operator group, a root-only
  network service, systemd ordering, owner-only state, and explicit
  desktop-user authorization. DEB, RPM, and tar installers share the same
  lifecycle helper.
- Windows uses a virtual account for `SubmuxRuntime`, LocalSystem only for
  `SubmuxRuntimeNet`, a protected ProgramData DACL, and a local operator group.
  Runtime expands persisted direct group members into the Named Pipe DACL so
  the first desktop session does not wait for a new login token.
- macOS uses two LaunchDaemons, a hidden service account, a separate operator
  group, and a per-user LaunchAgent for the GUI. The root helper recreates the
  volatile management directory before the low-privilege daemon starts.
- Every installer uses a fixed install identity, rejects an unmanaged second
  target, requires explicit repair for the same version, and requires both
  operator authorization and database compatibility for a downgrade.
- Uninstall always stops the main Runtime before the privileged helper.
  Default removal retains state; purge is a separate explicit action and
  rejects linked state directories.
- Online packages omit Mihomo. Offline packages require fixed platform
  binaries plus root, timestamp, snapshot, and targets metadata, along with
  release hashes, SBOM, and notices.

## Evidence boundary

The Linux DEB and Windows amd64/arm64 MSI sources have been compiled locally.
The macOS PKG can only be built on a macOS runner because `lipo`, `pkgbuild`,
and `productbuild` are Apple tools. A successful cross-compile or package build
does not promote an OS/architecture to stable; native install, IPC, proxy, TUN,
update, rollback, reboot, and uninstall evidence is recorded separately by the
release matrix.
