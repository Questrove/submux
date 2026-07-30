# Windows Runtime MSI

The WiX v6 package installs one machine-level Runtime. `SubmuxRuntime` runs as
the virtual account `NT SERVICE\SubmuxRuntime`; `SubmuxRuntimeNet` is the
separate LocalSystem network helper. The management Named Pipe is restricted
to the Runtime service SID, LocalSystem, elevated administrators, the local
`Submux Runtime Operators` group, and the group members enumerated when the
Pipe is created.

The server form authorizes elevated administrators only. For a desktop
installation, use `Install-SubmuxRuntime.ps1 -AuthorizeCurrentUser`. The MSI
adds the invoking user to the local operator group, while Runtime also writes
that persisted member SID into the Pipe DACL. The current login session
therefore does not need to acquire a new group token.

The MSI registration, fixed UpgradeCode, fixed install root, and service names
form the Windows installation record. The MSI rejects a newer installed version by default. An intentional
downgrade must use both `-AllowDowngrade` and `-DatabaseCompatible`.
Same-version replacement uses Windows Installer Repair; those fixed identities
prevent a second machine instance.

`Uninstall-SubmuxRuntime.ps1` stops the main Runtime first so its shutdown can
restore direct networking, then stops the privileged helper and invokes MSI
uninstall. State under `%ProgramData%\SubmuxRuntime` is retained by default;
`-Purge` removes it after rejecting reparse points. The script rejects an
uninstall that leaves services, routes, firewall rules, or Runtime-named
interfaces, and does not purge retained state in that case.

Build with WiX v6 plus `WixToolset.Util.wixext`. Online MSI files omit Mihomo.
Offline MSI files contain a fixed Mihomo executable and complete
root/timestamp/snapshot/targets metadata. Both include the SBOM, license
notice, artifact manifest, Runtime, network helper, and GUI.

Until paid Authenticode signing is configured, these MSI files are deliberately
unsigned and display `Unknown Publisher`. Windows arm64 remains preview until
the native install, IPC, TUN, update, rollback, and uninstall matrix passes.
