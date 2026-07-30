# macOS Runtime PKG

`build.sh` creates Universal online and complete offline PKG files from
separate amd64 and arm64 inputs. The package installs:

- `_submux-runtime` as the low-privilege Runtime account and service group;
- `submux-runtime-operators` as the management Socket group;
- two root-owned LaunchDaemons, with only `submux-runtime-net` running as root;
- a normal `/Applications/Submux Runtime.app` GUI;
- owner-only Runtime and privileged state directories;
- SHA-256 evidence, SBOM, notices, and, for offline packages, fixed Mihomo and
  complete TUF metadata;
- for offline packages, the exact Mihomo provenance, GPLv3 text, and matching
  source archive.

The server form authorizes only root. A desktop administrator explicitly runs
`submux-runtime-authorize-user --confirm USER`; it adds only that account to
the operator group and installs a per-user LaunchAgent. The GUI remains a
normal user process.

Package scripts reject a same-version replacement unless
`SUBMUX_RUNTIME_REPAIR=1` is supplied. A downgrade additionally requires both
`SUBMUX_RUNTIME_ALLOW_DOWNGRADE=1` and
`SUBMUX_RUNTIME_DATABASE_COMPATIBLE=1`. The install root, LaunchDaemon labels,
and receipt are fixed, so a second machine instance is rejected.

`submux-runtime-uninstall` stops the main Runtime first, allowing shutdown to
restore direct networking, then stops the root helper. It removes the
LaunchDaemons, per-user authorization, programs, and login items. State is
retained by default; `--purge` removes both state roots after rejecting
symbolic links and then removes only identities recorded by the protected
installation receipt. The script rejects an uninstall that leaves services,
routes, DNS entries, packet-filter rules, TUN interfaces, or sockets.

The PKG output is intentionally named `*_unsigned.pkg`. Until Developer ID
Installer signing and notarization are configured it must be described as
unsigned and unnotarized, with SHA-256 verified manually. macOS remains
preview until native install, IPC, TUN, update, rollback, reboot, and uninstall
tests pass on both Apple Silicon and Intel.
