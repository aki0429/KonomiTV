# KonomiTV Go beta installer — akki-0.14.1-β0.1

This is a beta installer for Windows amd64 and Linux amd64. The display/tag
version is `akki-0.14.1-β0.1`; ASCII asset names use `akki-0.14.1-beta0.1`.
The server API compatibility version remains `0.14.1`. These Go executables do
not currently carry a Windows numeric VERSIONINFO or a code-signing signature.

**Publication is on hold pending integration of the standalone Go work.**
Pre-integration builds are test artifacts, not a completed public release.

## Install

Run the Windows `.exe` or `bash KonomiTV-Go-Installer-...linux-amd64.sh` and
answer the directory and IPTV prompts. Use a **new directory that does not
exist**. Administrative rights are not required. No service or firewall rule is
created, and no tuner is contacted during installation.

Non-interactive examples:

```powershell
.\KonomiTV-Go-Installer-akki-0.14.1-beta0.1-windows-amd64.exe --yes --dir C:\KonomiTV-Go-Beta --iptv disabled --backend EDCB --backend-url tcp://127.0.0.1:4510/
```

```bash
bash ./KonomiTV-Go-Installer-akki-0.14.1-beta0.1-linux-amd64.sh --yes --dir "$HOME/KonomiTV-Go-Beta" --iptv enabled --backend Mirakurun --backend-url http://127.0.0.1:40772/
```

`--recordings PATH` references an existing recording folder; files are not
copied, modified or deleted by the installer. See `--help` for options.
`--iptv enabled` includes the IPTV screen and enables its backend/API.
`--iptv disabled` deploys a client with no IPTV route or navigation and blocks
IPTV APIs/stream URLs on the server. Shared Go IPTV code remains in the server
binary: this is a **disabled feature**, not physical code removal. No IPTV
playlist or user data is bundled. The IPTV backend cannot be selected with
IPTV disabled. The marker `installer-features.json` persists the install choice;
the settings API cannot enable a feature disabled at installation.

After installation, start `Run-Go.cmd` or `bash Run-Go.sh`, then open
`http://localhost:7002/`. The default listener is loopback only. Existing normal
KonomiTV ports/services are unaffected. Ctrl+C stops this foreground server.
Remote HTTPS access and service supervision require separate manual setup.

## Database and data protection

An empty SQLite database is generated locally by applying the existing Python
upgrade SQL migrations 0–12 in order. No user's DB is shipped. This bootstrap
is new-install-only, and never runs on an existing DB. It preserves the current
schema but does not write an Aerich model snapshot: do not point Python/Aerich
at this standalone beta database or assume cross-edition migrations are supported.
JWT secrets are generated only by the application on first launch, locally.

Installing into any existing directory is rejected, including previous installs.
A failed install removes only its newly created sibling staging folder. It does
not alter the destination, existing configuration, recordings or another install.

## Update / remove

For updates, keep the current installation, stop its foreground server, make a
backup of `config.yaml`, `installer-features.json`, and the whole `server/data`
directory (including any SQLite sidecars), then install to a new directory.
Only transfer the backup after confirming the target release's database schema
compatibility. Do not copy a live SQLite database. Retain the old directory for
rollback. This beta intentionally provides no in-place migration/overwrite.

To remove managed application files, stop the application and run the installer
outside the install directory with `--uninstall --dir PATH` (add `--yes` for
non-interactive use). Only manifest-listed files with their original SHA256 are
removed. Modified/unlisted files, `config.yaml`, databases, logs and recordings
remain. Empty directories and the manifest may remain; remove them manually
only after checking your retained data.

## Dependencies / provenance

Requires Windows 10/11 with built-in `tar`, or Linux with `tar` and `xz`.
The optional Go Twitter backend requires an existing Chrome/Chromium/Brave
installation (`KONOMITV_BROWSER_PATH` can select its executable). The installer
does not install a browser, use an existing browser profile, or authenticate any
external account. Browser-backed Twitter behavior is tested with synthetic CDP
peers; real account/login/post compatibility remains unverified.
Official dependency archive URLs are fixed to upstream Actions run
`39397337417`, source commit `4eece4f4b8ec410a06a3f2c016965f1e86d04a8c`:

- [Windows artifact](https://nightly.link/tsukumijima/KonomiTV/actions/runs/39397337417/thirdparty-windows.7z.zip):
  SHA256 `f0704fea273fe7961cc3251ee97e9dca105d2c58e574b14cb48983cd7c8a160f`
- [Linux artifact](https://nightly.link/tsukumijima/KonomiTV/actions/runs/39397337417/thirdparty-linux.tar.xz.zip):
  SHA256 `e1274c0971ba9fd2e5fc20eef73b5cc7027b56a494ed8fdee6be9802d4dc4a76`

The installer downloads and verifies the official ZIP before extraction.
`--dependency-archive PATH` accepts an offline copy with the same pinned SHA256.
Expired artifacts, changed bytes, missing dependencies and extraction errors
fail explicitly; there is no fallback to a mutable latest build. The upstream
archive's Python runtime and GPU encoder executables are excluded. This CPU beta
supplies FFmpeg, ffprobe, tsreadex, psisiarc, psisimux and Linux shared libraries,
retaining their license files. Optional VCEEncC was blocked by Windows protection
during isolation testing; it is never restored, executed or included by this
installer. Select FFmpeg for this CPU beta. These dependencies are fetched directly
from the official distribution rather than republished in our installer.
Upstream build recipes and source versions are in
[the pinned build workflow](https://github.com/tsukumijima/KonomiTV/blob/4eece4f4b8ec410a06a3f2c016965f1e86d04a8c/.github/workflows/build_thirdparty.yaml).

KonomiTV is MIT licensed; `License.txt` is retained. Bundled Go/client notices
are under `licenses/`, including the Go runtime and CPython-derived ARIB tables.
Third-party dependency licenses remain under `server/thirdparty` after install.
No production config, credentials, DB, recording, log, or private key is bundled.

## Verification limits

See the release notes and test evidence for the exact integrated source commit.
Installer tests use clean generated DBs and mocks; live production tuners and
recordings are never used for installation tests. GPU hardware compatibility,
remote HTTPS, service registration and live reception require separate testing.
