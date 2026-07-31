# Linux Runtime packages

`build.sh` produces DEB, RPM, and tar.zst packages for systemd/glibc hosts.
`--gui` is optional for server packages; when supplied it must be the real
Runtime GUI executable for the target architecture.
Offline packages additionally require the fixed Mihomo executable, its signed
provenance JSON, GPLv3 text, matching source archive, and complete TUF metadata.
The Linux airgap kit is assembled separately by `packaging/airgap/build.sh`
from an extracted offline tar bundle. It adds a separately released and
attested stable control plane plus the complete offline TUF verification bundle
without changing the native Runtime package.
The server form authorizes only root by default. A desktop installation must
name the user explicitly:

```sh
SUBMUX_RUNTIME_AUTHORIZE_USER="$SUDO_USER" dpkg -i submux-runtime.deb
SUBMUX_RUNTIME_AUTHORIZE_USER="$SUDO_USER" rpm -U submux-runtime.rpm
sudo ./install.sh --authorize-desktop "$USER"
```

Same-version replacement requires `SUBMUX_RUNTIME_REPAIR=1` for DEB/RPM or
`--repair` for tar. A downgrade additionally requires
`SUBMUX_RUNTIME_ALLOW_DOWNGRADE=1` and
`SUBMUX_RUNTIME_DATABASE_COMPATIBLE=1`, or the equivalent tar flags.

Uninstall retains `/etc/submux-runtime`, `/var/lib/submux-runtime`, and the
root-only `/var/lib/submux-runtime-privileged` by default. DEB purge removes
them. RPM uses the explicit
`SUBMUX_RUNTIME_PURGE=1` uninstall setting. The tar installer accepts
`--uninstall --purge`.

The package scripts stop the Runtime before the privileged network helper so
that Runtime shutdown restores direct networking. They then report any
remaining Runtime-owned services, routes, DNS settings, firewall tables, TUN
interfaces, or sockets. Uninstall returns a failure and does not purge retained
state while any such residual remains.
