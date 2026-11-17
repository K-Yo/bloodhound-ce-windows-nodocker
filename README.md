# BloodHound CE, single Windows executable with embedded PostgreSQL

`dist/BloodHoundCE.exe` is one self-extracting Windows x64 executable containing:

- the BloodHound Community Edition API server and UI (built from `src/`, UI assets embedded),
- PostgreSQL 18.7 server binaries plus the Microsoft VC++ runtime they need,
- SharpHound v2.18.0 for the collector download page.

PostgreSQL is used for **both** the application database and the graph
(`graph_driver: "pg"`), so no Neo4j is required.

## Use

```
BloodHoundCE.exe
```

First start extracts to `%LOCALAPPDATA%\BloodHoundCE` and initializes a private
cluster in `%LOCALAPPDATA%\BloodHoundCE\pgdata`. The launcher starts PostgreSQL on
a free loopback port, writes `bhapi.json`, starts BloodHound, waits for the UI,
then opens `http://127.0.0.1:8080/ui/`.

- Admin credentials are generated once into `secrets.json`; the initial password
  is printed in the console and stored in `%LOCALAPPDATA%\BloodHoundCE\secrets.json`.
  User: `admin`.
- Logs: `%LOCALAPPDATA%\BloodHoundCE\logs\{launcher,postgres,bloodhound}.log`.
- Data survives restarts; the server reuses the last HTTP port when it is free.
- `-home <dir>` overrides the data directory, `-no-browser` suppresses the browser.
- Ctrl+C (or closing the console) stops BloodHound and PostgreSQL. If the launcher
  is killed hard, the next start reaps the stale postmaster.

Only loopback is bound; there is no TLS and no remote exposure.

## Antivirus exclusions (required)

Windows Defender (and third-party AV) flags unsigned self-extracting
executables, embedded database servers, and SharpHound. Without exclusions the
`sharphound-v2.18.0.zip` (and potentially `bloodhound.exe`) can be quarantined
mid-extraction, leaving `%LOCALAPPDATA%\BloodHoundCE\app.tmp` behind and no
working `app` directory.

Add the exclusions from an **elevated** PowerShell before the first run:

```powershell
powershell -ExecutionPolicy Bypass -File scripts\allow-defender.ps1 -ExePath C:\path\to\BloodHoundCE.exe
```

Equivalent manual commands:

```powershell
Add-MpPreference -ExclusionPath "$env:LOCALAPPDATA\BloodHoundCE"
Add-MpPreference -ExclusionPath "C:\path\to\BloodHoundCE.exe"
Add-MpPreference -ExclusionProcess 'bloodhound.exe'
Add-MpPreference -ExclusionProcess 'BloodHoundCE.exe'
```

The same entries can be added through Windows Security → Virus & threat
protection → Manage settings → Exclusions. Verify as an administrator:

```powershell
Get-MpPreference | Select-Object -ExpandProperty ExclusionPath
```

For a third-party AV, add equivalent folder and process exclusions for
`%LOCALAPPDATA%\BloodHoundCE` and `bloodhound.exe` / `BloodHoundCE.exe`.

If a file was already quarantined, stop the launcher, delete
`%LOCALAPPDATA%\BloodHoundCE\app` and `app.tmp` (keep `pgdata` and `secrets.json`),
then run `BloodHoundCE.exe` again; the payload is re-extracted from the executable.

On this machine the exclusions are already applied
(`C:\Users\olivi\Downloads\bh-defender-exclusion.status` = `OK`).

## Build

Requirements: Linux build host, Go 1.27.4 (toolchain is downloaded by hand),
Node 20+/Yarn 4 via Corepack, `zip`, `unzip`, `7z`, `curl`, and the BloodHound
source checkout in `src/`.

```bash
git clone --depth 1 https://github.com/SpecterOps/BloodHound.git src
scripts/fetch-postgres.sh all        # pgbin/{win,linux}
BH_VERSION=v9.10.0 scripts/build.sh windows   # dist/BloodHoundCE.exe
BH_VERSION=v9.10.0 scripts/build.sh linux     # dist/BloodHoundCE-linux
```

`BH_VERSION` stamps `cmd/api/src/version` via ldflags; without it the UI shows
no server version (`v999.999.999`). The build also fetches SharpHound v2.18.0
and AzureHound v3.1.3 for the Download Collectors page. The embedded
`launcher/payload/payload.zip` is regenerated from `pgbin/` + the freshly built
`bloodhound` binary on every run.

The upstream sources need these fixes:

- `patches/windows-portability.patch`:
  - `api/static/static.go`: UI assets live in an `embed.FS`, which requires `/`
    separators; `filepath.Join` produced `assets\...` and every asset 404'd.
  - `database/migration/goose.go`, `manifest.go`: same issue for embedded
    migration and extension SQL paths (`extensions\ad_graph_schema.sql`).
  - `packages/go/storage/localstore.go`: Windows cannot `fsync` a directory handle
    (`ERROR_ACCESS_DENIED`); every ingest upload failed with
    `sync ...\work\tmp\.: Access is denied`.
- `patches/collector-manifest.patch`: empty collector directories returned
  `"versions": null`; the Download Collectors page then crashed reading
  `.length`. It now returns `[]`.

Apply both with
`git -C src apply "$PWD/patches/windows-portability.patch" "$PWD/patches/collector-manifest.patch"`
after a fresh clone. The launcher itself also avoids pipe-backed `cmd.Stdout`,
because `postgres.exe` inherits that pipe from `pg_ctl` on Windows and `Wait`
never sees EOF.

## CI and releases

`upstream.env` pins the upstream version and commit:

```
BH_VERSION=v9.10.0
BH_COMMIT=93dcb33af324ef6b080f9bd110d65c8ee2ea8bc4
```

`.github/workflows/windows-build.yml` has a `prepare` job that decides what to
build, then a `build` job (Go 1.27.4, Node 22, p7zip; clones BloodHound at the
pinned commit, applies both patches, fetches PostgreSQL/VC runtime and both
collectors) and a `record` job:

- **push to `main`** — build only, artifact `BloodHoundCE-windows-amd64`.
- **push of a `v*` tag** — build and attach the exe + checksum to that release.
- **schedule (daily 06:17 UTC)** — checks SpecterOps' latest stable release.
  If it is newer than `BH_VERSION`, it resolves the release tag's commit, builds
  it, and publishes a release whose tag mirrors upstream (for example
  `v9.10.0`). Only after a successful release does the `record` job advance
  `upstream.env`.
- **workflow_dispatch** — optional `bh_version`, `bh_commit`, and `release_tag`
  inputs for manual builds/releases; empty inputs use `upstream.env`.

```bash
# manual build+release of a specific upstream version
gh workflow run windows-build.yml -f bh_version=v9.10.0 -f release_tag=v9.10.0
```

Caveats:

- Only stable upstream releases are considered (no drafts/prereleases).
- GitHub scheduled workflows can be delayed and are disabled after 60 days of
  repository inactivity; re-enable or use `workflow_dispatch`.
- The two patches touch upstream files. If a new release changes those regions,
  `git apply` fails and the run stops before publishing anything; update the
  patch and re-run.
- Automatic release tags track upstream (`v9.10.0`); the initial packaging tags
  (`v0.1.0`, `v0.1.1`) remain lower in semver order.

## Verification performed

`dist/BloodHoundCE-linux` is the same launcher and payload pipeline with Linux
binaries. End-to-end on this machine:

- first run: extraction, `initdb`, PostgreSQL start, database creation,
  migrations (including `pg_trgm` and the `database_switch` driver table),
  analysis pipeline start, UI `GET /ui/` → 200;
- `POST /api/v2/login` with the generated admin password → 200, and the returned
  session token authorizes `/api/version` and `/api/v2/collectors/sharphound`;
- ingest upload: `POST /api/v2/file-upload/start` → upload a ZIP →
  `POST /api/v2/file-upload/{id}/end` → 202/200, `File written and validated`;
- browser: Download Collectors renders SharpHound v2.18.0 and AzureHound
  v3.1.3, no error boundary; footer shows `BloodHound: v9.10.0`;
- restart reuses the cluster and the persisted secrets;
- Ctrl+C stops BloodHound and PostgreSQL cleanly (no postmaster left).

`dist/BloodHoundCE.exe` was then run on the Windows host: extraction, `initdb`,
PostgreSQL, migrations, extension data population, UI `/ui/` → 200, a hashed JS
asset → 200, admin login → session token, `/api/version` → `v9.10.0`, collector
manifests for SharpHound and AzureHound with working `latest` downloads, and a
ZIP ingest upload → 202 (`File written and validated`). Both the PostgreSQL
server (`postgres.exe`) and BloodHound (`bloodhound.exe`) were running from
`%LOCALAPPDATA%\BloodHoundCE\app`.

## Limits / risks

- Windows 10/11 x64 only. The UCRT (`api-ms-win-crt-*`) must be present
  (default on supported Windows); the VC++ runtime itself is bundled.
- Antivirus may flag a self-extracting executable containing a database server;
  apply the exclusions above before the first run.
- Non-ASCII paths and paths near the 260-character limit can break PostgreSQL on
  Windows; use an ASCII `-home` path if that happens.
- PostgreSQL runs as the logged-in user; `pgdata` is only as protected as that
  profile directory.
- The build pins BloodHound `src/` HEAD at clone time and PostgreSQL 18.7.0.
