# Migrate every tier off `init.sh` onto systemd units

Status: shipped in v1.11.9-v1.11.10 (original target was v1.11.3; actual ship took longer because the lohar bridge in d3035d1 needed to land first, then the spawn-helper bug surfaced from real testing and got its own plan).

Shipping commits, in order:

- `d3035d1` — lohar bridge: ExecStartPost directive, `/run/bhatti/config-env` for `EnvironmentFile=-`, binfmt_misc mount (needed by docker buildx). This is the substrate work the plan called for.
- `32dfe2f` — docker tier: docker.service via deb-shipped unit + drop-in (daemon.json cgroup-driver = cgroupfs to avoid the dial-unix-systemd-private failure; ExecStartPost chmod 666 on the socket for uid-1000 callers; buildx baked in).
- `411e8f9` — lohar systemctl: backslash line continuations in unit-file parser. Required by docker.service and several upstream-shipped units.
- `e8cd4d4` — browser tier: headless-chrome.service with Restart=on-failure.
- `5602f34` — computer tier: four units (kasmvnc-firstboot, kasmvnc, xfce-session, bhatti-display-env). dbus daemon explicitly NOT started; pulseaudio dropped. init.sh deleted from this tier.
- `0bc1bce` — README links to per-tier bhatti.sh deep dives.

Follow-on: the computer tier shipped with a latent race — Xkasmvnc's daemon(3) fork escaped the unit's cgroup before placement, so `systemctl stop kasmvnc` left an orphan holding the X socket. Fixed in v1.11.10 by a separate plan (`docs/archive/PLAN-spawn-helper.md`, commits 623e8ad…6cfae9d) that introduced a `lohar spawn` subcommand for race-free cgroup placement.

Touches (as shipped): `cmd/lohar/main.go` (binfmt_misc mount, config-env serialisation, boot-timing reordering), `cmd/lohar/systemctl.go` (ExecStartPost, backslash continuations, spawn-helper rewiring), `cmd/lohar/spawn.go` (new), `scripts/tiers/{computer,docker,browser}.sh`, plus test scaffolding in `cmd/lohar/{spawn,cgroup}_test.go` and `.github/workflows/integration.yml`.

---

## Why

`PLAN-systemd-ship.md` shipped lohar's systemd implementation as the
default init for every tier (`/usr/bin/systemctl` → lohar busybox
dispatch, `multi-user.target.wants/` activation, `Type=simple|oneshot
|forking|notify`, `Restart=`, `After=`/`Before=` topological ordering,
`ConditionPathExists=`, `EnvironmentFile=`, sd_notify, syslog receiver,
journal-per-unit). Item #17 of that plan named the follow-up:

> long-term win is converting [docker/browser/computer tiers] to proper
> systemd units: dockerd.service (ships with docker-ce, just enable it),
> headless-chrome.service (custom unit), kasmvnc.service + xfce.service
> (custom units). This is a follow-up, not a blocker.

Today, all three tiers still bootstrap their workload from a single
imperative `/etc/bhatti/init.sh` that lohar runs once, with everything
backgrounded with `&`. That has four user-visible costs:

1. **No restart on crash.** If `dockerd` / `headless_shell` / `Xkasmvnc`
   dies, it stays dead. The published URL silently 502s.
2. **No status/logs.** `systemctl status dockerd` returns "unit not
   found". Users debug by `ps` and `cat /var/log/...` if the workload
   even logs to a file.
3. **No restart-after-config-edit story.** The expected Linux mental
   model — "edit /etc/foo.conf, `systemctl restart foo`" — doesn't work
   when `foo` was started with `&` from a script.
4. **Demos look amateurish.** The first thing anyone evaluating the
   `computer` tier types is `systemctl status kasmvnc` to see if it's
   real. Today it returns "unit not found".

Converting each tier to proper units fixes all four with a single,
well-scoped change to material we already own.

---

## Scope

### In

- `cmd/lohar/main.go`: bridge `configEnv` (user `--env`) →
  `/run/bhatti/config-env` so unit files can `EnvironmentFile=` it.
  Tiny, generally-useful, ~7 lines.
- `scripts/tiers/computer.sh`: replace `init.sh` with five units.
- `scripts/tiers/docker.sh`: replace `init.sh` with one unit.
- `scripts/tiers/browser.sh`: replace `init.sh` with one unit.
- `docs/tiers.md`: refreshed per-tier sections covering creds,
  tunables, status/log commands, link to upstream docs.
- `README.md`: tier examples reflect the new commands.

### Out (deliberately deferred)

- Per-image `--cpus` defaults at the CLI/server level — separate PR;
  ergonomic, unrelated to the unit-file migration.
- A system D-Bus daemon. `PLAN-systemd-rc.md` ruled this out for
  snapshot/restore reasons; this plan honours that decision by
  removing the existing leaky `dbus-daemon --system --fork` rather
  than re-housing it in a unit file.
- Per-session pulseaudio. Not worth the complexity for a use case
  no current user has reported wanting.
- A `headless-chrome` `Type=notify` integration. Chrome has no
  sd_notify support; the current "start it and hope CDP comes up
  fast" pattern is fine when paired with `Restart=on-failure`.

---

## Design — the lohar bridge

`configEnv` (populated from the config drive at line 128 of
`cmd/lohar/main.go`) is currently consumed only in `cmd/lohar/exec.go:216`
to enrich `bhatti exec` environments. Unit files spawned by
`startEnabledServices()` see only `os.Environ()` plus the unit's own
`Environment=` and `EnvironmentFile=` directives (`buildServiceEnv()`
in `cmd/lohar/systemctl.go`). There is no path today by which
`bhatti create --env DISPLAY_WIDTH=1920` reaches a unit file.

**Fix.** Right after `configEnv = cfg.Env` (and the matching `else`
branch that handles the no-config-drive case), serialize the map to
`/run/bhatti/config-env` in canonical `KEY=VALUE\n` form. Units
opt in with:

```ini
EnvironmentFile=-/run/bhatti/config-env
```

The leading `-` makes the file optional; tiers whose units don't need
user env still work, and minimal sandboxes with no config drive aren't
affected. The write happens before `startEnabledServices()` (line
226), so units see the file at activation time.

This is purely additive. Existing behaviour is untouched: the
`/run/bhatti/env` read at line 249 (post-init.sh, post-services)
continues to be the channel by which services tell `bhatti exec`
about side-effects like `DISPLAY=:99`.

### Concrete patch

```go
// In runAgent(), between cfg-application and applyTmpfiles():
//
//   configEnv = cfg.Env       // existing
//   ...                       // existing else-branch, networking, etc.
//
//  if len(configEnv) > 0 {
//      var b strings.Builder
//      for k, v := range configEnv {
//          fmt.Fprintf(&b, "%s=%s\n", k, v)
//      }
//      _ = os.WriteFile("/run/bhatti/config-env", []byte(b.String()), 0644)
//  }
//
//   applyTmpfiles(...)        // existing
//   startEnabledServices()    // existing — now sees the file
```

---

## Design — per-tier units

Every tier's units follow the same convention:

- File location: `/etc/systemd/system/<unit>.service`. (Bhatti-specific
  units; we don't sneak into `/usr/lib/systemd/system/` where Ubuntu
  packages live.)
- Enabled via build-time symlink in
  `/etc/systemd/system/multi-user.target.wants/<unit>.service`.
- `EnvironmentFile=-/run/bhatti/config-env` whenever the unit has any
  user-tunable behaviour. Cheap to include even when there's nothing
  to override; future env additions need no further wiring.
- `Restart=on-failure` + `RestartSec=2s` on every long-running daemon.
  No `Restart=always` — we don't want flapping units masking a real
  bug at boot.

### `computer` tier

Four units. All shipped at build time; init.sh removed.

| Unit | Type | Notes |
|---|---|---|
| `kasmvnc-firstboot.service` | `oneshot`, `RemainAfterExit=yes` | `ConditionPathExists=!/root/.kasmpasswd`. Generates random 16-char password via `kasmvncpasswd`, writes cleartext to `/root/.vnc/cleartext` (mode 600) for the `vnc-creds` helper. Runs once per sandbox lifetime. Resume-from-snapshot: condition fails → skipped, existing creds preserved. |
| `kasmvnc.service` | `simple` | `ExecStart=/usr/bin/Xkasmvnc :99 -geometry ${DISPLAY_WIDTH}x${DISPLAY_HEIGHT} -depth ${DISPLAY_DEPTH} -websocketPort 6080 -interface 0.0.0.0 -BlacklistTimeout 0 -FreeKeyMappings -AlwaysShared -FrameRate=${KASM_FRAMERATE} -RectThreads=${KASM_THREADS}`. Defaults injected via `Environment=`; user overrides via `EnvironmentFile=-/run/bhatti/config-env`. `After=kasmvnc-firstboot.service`. `Restart=on-failure`. |
| `xfce-session.service` | `simple` | `ExecStart=/usr/bin/startxfce4`. `Environment=DISPLAY=:99 HOME=/root`. `After=kasmvnc.service`. `Requires=kasmvnc.service` so killing kasmvnc cascades. `Restart=on-failure`. |
| `bhatti-display-env.service` | `oneshot`, `RemainAfterExit=yes` | `ExecStart=/bin/sh -c 'mkdir -p /run/bhatti && echo DISPLAY=:99 > /run/bhatti/env'`. Replaces the only thing the old init.sh did unconditionally — making `DISPLAY` available to subsequent `bhatti exec` calls. |

Removed from init.sh / boot, deliberately not replaced:

- **`dbus-daemon --system --fork`.** `PLAN-systemd-rc.md` rejected real
  systemd specifically because *"systemd and its children (dbus,
  journald, lohar) introduce kernel state (timers, epoll sets, inotify
  watches) that doesn't survive snapshot restore cleanly."* The
  current tier brings exactly that risk back in via a freestanding
  `dbus-daemon`. Dropping it removes a long-lived process whose only
  cost on restore is unbounded — the inotify on `/etc/dbus-1/`, the
  epoll on `/var/run/dbus/system_bus_socket`, the bus's internal
  per-connection timers — and whose only benefit is desktop polish
  features (notifications, secret service, gsettings-via-dconf) that
  nobody using a remote VNC desktop in a Firecracker microVM relies
  on. The `dbus`/`dbus-x11` packages stay installed (libdbus links,
  occasional interactive `dbus-launch` use), but no daemon is
  started at boot.
- **`eval $(dbus-launch --sh-syntax)`.** Started a *session* bus
  tied to lohar's PID 1 shell scope, leaked its address into the
  startxfce4 env, and orphaned when init.sh exited. A bug, not a
  feature. Modern XFCE (4.16+) launches its own session bus on demand
  if it actually needs one; we let it.
- **`pulseaudio --start --exit-idle-time=-1`.** See scope/Out.

Degradation risk from no system dbus (to be confirmed empirically in
the test plan):
- XFCE: `xfsettingsd` warnings about settings-daemon registration;
  thunar D-Bus activation paths skipped; some keyboard shortcuts
  routed via xfconf may not propagate. Desktop still loads.
- Chromium: `--test-type` flag (already in our wrapper) suppresses
  most of the dbus-related noise. Notifications/password-manager
  unavailable; not used in any agent or demo flow.
- `xdotool`, `scrot`, the `screenshot` helper: pure X11, no dbus
  involvement.

If XFCE refuses to start at all without dbus on the test box, the
narrowest possible fix is to scope a `dbus.service` to the
`xfce-session.service` lifecycle via `Requires=` + `After=` rather
than running it at multi-user.target — reducing the cross-snapshot
risk window to "only when xfce is up". We hold that as a fallback,
not a default.

`KASM_THREADS` default is computed in
`kasmvnc-firstboot.service`'s ExecStartPre (cheap one-time write to
`/run/bhatti/config-env`) so the dynamic `nproc-1` calculation lives
in shell, not in the unit file. User-supplied `KASM_THREADS` from
`bhatti create --env` overrides it via merge order in
`buildServiceEnv()` — last one wins, which the firstboot oneshot
relies on by writing only when the key is unset.

### `docker` tier

One unit replacing init.sh.

| Unit | Type | Notes |
|---|---|---|
| `docker.service` | `notify` | `ExecStart=/usr/bin/dockerd`. dockerd implements sd_notify natively (Type=notify means lohar waits for `READY=1` before considering the unit active). `ExecStartPost=/bin/chmod 666 /var/run/docker.sock` — same workaround as today: lohar exec runs as uid 1000 without supplementary group membership, so we widen the socket. `Restart=on-failure`, `RestartSec=2s`. |

The current init.sh runs `update-alternatives --set iptables
iptables-legacy` at every boot; this is redundant — the build-time
chroot already does the same. Dropped from boot path.

The current init.sh polls `/var/run/docker.sock` for up to 10s. With
`Type=notify` lohar blocks `startEnabledServices()` until dockerd
itself signals ready, which is the same observable behaviour but
more accurate (dockerd knows when *it* is ready better than we do).
Boot-time is unchanged in the happy path; clearer in the sad path
(failed unit shows up in `is-failed`/`status` instead of "did the
poll just time out?").

### `browser` tier

One unit replacing init.sh.

| Unit | Type | Notes |
|---|---|---|
| `headless-chrome.service` | `simple` | `ExecStart=` runs the playwright `headless_shell` binary with the same flags init.sh used: `--no-sandbox --disable-gpu --disable-dev-shm-usage --remote-debugging-port=9222 --remote-debugging-address=0.0.0.0`. The path to `headless_shell` is resolved at build time and baked into the unit (not searched at boot — chrome paths in `~/.cache/ms-playwright/chromium-*/` are stable per-image). `Restart=on-failure`, `RestartSec=2s`. |

The CDP-readiness poll in init.sh becomes unnecessary: the unit is
"active" the moment the process starts, and Restart handles crashes.
Existing `bhatti exec browser -- curl http://localhost:9222/json/version`
is the same readiness check users already run.

---

## What stays in `init.sh`

After this change, `init.sh` is empty — and we remove it entirely
from each tier. Lohar's main.go currently does:

```go
if _, err := os.Stat("/etc/bhatti/init.sh"); err == nil {
    // run it
}
```

The check survives — `init.sh` is now an *optional* extension point
that future tiers (or user-built images via `bhatti image save`) can
use without us needing to ship it. Removing the file from each
production tier is the clean signal "we don't use this anymore".

---

## Tunables surface, post-migration

Every tier exposes its tunables as plain env vars settable via
`bhatti create --env`. The unit files pull from
`/run/bhatti/config-env` so set-at-create-time just works.

| Tier | Variable | Default | Effect |
|---|---|---|---|
| computer | `DISPLAY_WIDTH` | 1280 | X server width |
| computer | `DISPLAY_HEIGHT` | 720 | X server height |
| computer | `DISPLAY_DEPTH` | 24 | X server colour depth |
| computer | `KASM_FRAMERATE` | 60 | encoder max fps |
| computer | `KASM_THREADS` | nproc-1 | encoder thread count |
| docker | (none today) | — | future: `DOCKER_DATA_ROOT`, `DOCKER_REGISTRY_MIRROR` |
| browser | `CHROME_REMOTE_PORT` | 9222 | CDP port |
| browser | `CHROME_FLAGS` | "" | extra space-separated flags appended to `ExecStart` |

The `CHROME_FLAGS` knob is the escape hatch we want for the browser
tier — users running automation with non-default flags
(`--user-agent`, `--proxy-server`, etc.) currently have no way to
inject them without rebuilding the image.

---

## User-facing story (post-ship)

Goes into `docs/tiers.md` per-tier section, referenced from the
quickstart:

```
$ bhatti create --name d --image computer --cpus 2 --memory 4096
$ bhatti publish d -p 6080
$ bhatti exec d -- vnc-creds                # username + password
$ bhatti exec d -- systemctl status kasmvnc # is it healthy?
$ bhatti exec d -- journalctl -u kasmvnc -n 50
$ # if you edit /etc/kasmvnc/kasmvnc.yaml:
$ bhatti exec d -- systemctl restart kasmvnc
```

For docker:

```
$ bhatti create --name c --image docker
$ bhatti exec c -- systemctl status docker
$ bhatti exec c -- journalctl -u docker -n 50
$ bhatti exec c -- docker run hello-world
```

For browser:

```
$ bhatti create --name b --image browser \
    --env CHROME_FLAGS="--user-agent=Mozilla/5.0..."
$ bhatti exec b -- systemctl status headless-chrome
$ bhatti exec b -- curl -s http://localhost:9222/json/version
```

The shape — `systemctl status <foo>`, `journalctl -u <foo>`,
`systemctl restart <foo>` — is the same on every tier. Demoable.

---

## Test plan

The single most important verification is `XFCE starts without a
system dbus, after a fresh boot`. If true, the design above stands.
If false, the fallback in the `computer` tier section kicks in.
Everything else is mechanical.

All on `agni-01`, in a build dir under `/tmp/bhatti-rc/` so we don't
touch `/var/lib/bhatti/images/` until the test passes.

```bash
# 1. Build all three rootfs images locally
sudo ./scripts/build-tier.sh computer amd64 ./bin/lohar-linux-amd64
sudo ./scripts/build-tier.sh docker   amd64 ./bin/lohar-linux-amd64
sudo ./scripts/build-tier.sh browser  amd64 ./bin/lohar-linux-amd64

# 2. Static checks on each rootfs (mount loopback, inspect)
for tier in computer docker browser; do
    mkdir -p /tmp/m && sudo mount -o loop dist/rootfs-$tier-amd64.ext4 /tmp/m
    test -e /tmp/m/etc/bhatti/init.sh && echo "FAIL: init.sh still present in $tier"
    ls /tmp/m/etc/systemd/system/multi-user.target.wants/
    sudo umount /tmp/m
done

# 3. Smoke-test each tier in a side sandbox using the new image.
#    Import as <tier>-rc so it doesn't shadow the production image.
bhatti image import dist/rootfs-computer-amd64.ext4 --as computer-rc
bhatti image import dist/rootfs-docker-amd64.ext4   --as docker-rc
bhatti image import dist/rootfs-browser-amd64.ext4  --as browser-rc

# computer
bhatti create --name rc-c --image computer-rc --cpus 4 --memory 4096
sleep 5
bhatti exec rc-c -- systemctl is-system-running              # expect: running or starting
bhatti exec rc-c -- systemctl status kasmvnc xfce-session    # both active
bhatti exec rc-c -- journalctl -u kasmvnc -n 5
bhatti exec rc-c -- vnc-creds                                # not empty
bhatti exec rc-c -- pkill -9 Xkasmvnc; sleep 4
bhatti exec rc-c -- systemctl status kasmvnc                 # active again (Restart fired)
bhatti stop rc-c && bhatti start rc-c                        # snapshot/resume
bhatti exec rc-c -- systemctl status kasmvnc                 # active after thaw
bhatti exec rc-c -- vnc-creds                                # same creds as before
bhatti destroy rc-c

# docker
bhatti create --name rc-d --image docker-rc --cpus 2 --memory 2048
sleep 5
bhatti exec rc-d -- systemctl status docker                  # active
bhatti exec rc-d -- docker run --rm hello-world              # works
bhatti exec rc-d -- pkill -9 dockerd; sleep 4
bhatti exec rc-d -- systemctl status docker                  # active again
bhatti destroy rc-d

# browser
bhatti create --name rc-b --image browser-rc
sleep 3
bhatti exec rc-b -- systemctl status headless-chrome
bhatti exec rc-b -- curl -sf http://localhost:9222/json/version | head -1
bhatti destroy rc-b

# 4. Tunables work
bhatti create --name rc-c2 --image computer-rc --cpus 4 --memory 4096 \
    --env DISPLAY_WIDTH=1920 --env DISPLAY_HEIGHT=1080 \
    --env KASM_FRAMERATE=60
bhatti exec rc-c2 -- screen-size                             # expect: 1920x1080
bhatti exec rc-c2 -- ps -o cmd -C Xkasmvnc                   # expect: -FrameRate=60
bhatti destroy rc-c2

bhatti create --name rc-b2 --image browser-rc \
    --env CHROME_FLAGS="--user-agent=bhatti-test/1.0"
bhatti exec rc-b2 -- ps -o cmd -C headless_shell             # expect: --user-agent visible
bhatti destroy rc-b2

# 5. Bench regression check
bench/run.sh --image minimal-rc      # ensure no exec/file/create regression
```

If all green: tag, push, CI rebuilds the canonical
`/var/lib/bhatti/images/rootfs-*-{amd64,arm64}.ext4` artifacts.
Server operators run `sudo bhatti update --tiers all` to pull the new
images. Existing sandboxes are unaffected (frozen on old rootfs);
new sandboxes pick up the new tier on `bhatti create`.

---

## Risk + mitigations

| Risk | Mitigation |
|---|---|
| Unit dependency cycle that lohar's depgraph breaks awkwardly | Hand-build the start graph on paper; verify `kasmvnc-firstboot → kasmvnc → xfce-session` is a clean chain with no back-edges. `bhatti-display-env` is a leaf with no `After=`. The unit definitions above respect that. |
| XFCE refuses to start without a system dbus | Caught by the test plan smoke-step; fallback in `computer` tier section above (scope `dbus.service` to xfce-session lifecycle, not multi-user.target) keeps the snapshot-risk window minimised. We do not pre-emptively ship dbus. |
| `Type=notify` for `dockerd` doesn't fire READY in lohar's notify receiver | Fall back to `Type=simple` + a separate `docker-ready.service` oneshot that polls the socket, like today's init.sh does. Worst case: equivalent to current behaviour. |
| `EnvironmentFile=-` semantics differ from upstream systemd | Verified in `cmd/lohar/systemctl.go:1533`: leading `-` is stripped, file-not-found is silently skipped. Matches systemd. |
| Build-time symlinks into `multi-user.target.wants/` aren't picked up | Verified: `minimal.sh` already creates the directory; lohar's `startEnabledServices` reads it; the existing tier scripts already rely on this for *services that ubuntu packages drop in*. We're adding bhatti-specific symlinks via the same mechanism. |
| Production sandboxes break because they were created on old rootfs | Sandboxes carry their rootfs at create-time; updating the canonical image affects only future creates. Tested explicitly via the snapshot/resume case. |
| `headless_shell` path moves between Playwright versions | We bake the path into the unit at build time (resolved by the same `find` command init.sh uses today). If Playwright version bumps the directory layout, the unit needs a regenerated path — caught at build time, not at runtime. |
| The `vnc-creds` helper races with `kasmvnc-firstboot.service` | The helper already prints "KasmVNC may still be initializing" if the cleartext file isn't there yet. Users invoking `vnc-creds` within seconds of `bhatti create` will see this once and retry. Acceptable. |
| Pulseaudio removal breaks an app someone uses | Document the removal in the release notes. Re-adding a per-session pulseaudio is a follow-up if it bites real users. |

---

## Phasing

One PR, one tag (`v1.11.3`), in this commit order:

1. **lohar bridge.** `cmd/lohar/main.go` change + a unit test in
   `cmd/lohar/main_test.go` covering the file write. Independent
   commit so it can be reviewed standalone.
2. **computer tier units.** `scripts/tiers/computer.sh` rewrite,
   `vnc-creds` helper, drop init.sh.
3. **docker tier units.** `scripts/tiers/docker.sh` rewrite,
   drop init.sh.
4. **browser tier units.** `scripts/tiers/browser.sh` rewrite,
   drop init.sh, add `CHROME_FLAGS` env knob.
5. **docs.** `docs/tiers.md` per-tier sections; `README.md`
   examples; release-note bullet for `pulseaudio` removal.
6. **PLAN archive.** Move this doc to `docs/archive/` (matches the
   convention from `PLAN-systemd-ship.md`).

Tag after the test plan above passes on agni.

---

## Pre-flight verifications (done while writing this plan)

1. **`journalctl -f` works** — `cmd/lohar/systemctl.go:1493` calls
   `tailFollow(logPath)`. The user-facing `journalctl -u kasmvnc -f`
   story holds.
2. **`Type=notify` end-to-end** — `cmd/lohar/notify.go` binds the
   notify socket, attributes senders via SCM_CREDENTIALS + cgroup
   walk, clears the `.activating` marker on `READY=1`. `dockerd`
   uses sd_notify natively (matches upstream `docker.service`
   `Type=notify`). No fallback wrapper needed.
3. **`ConditionPathExists=!` negation** — `cmd/lohar/conditions.go:65`
   handles the leading `!` and treats failed conditions as
   skip-without-error. Snapshot/resume firstboot semantics work.
4. **`EnvironmentFile=-` optional-file** —
   `cmd/lohar/systemctl.go:1533` strips leading `-`, treats missing
   file as no-op. Tier units that include the file work on minimal
   sandboxes too.
5. **`After=` activation waves** — `cmd/lohar/depgraph.go` does
   topological sort; each wave waits for its units to become active
   before the next. Our `firstboot → dbus → kasmvnc → xfce-session`
   chain serializes correctly.

## Open questions

1. **Type=notify for `headless-chrome`?** Chromium has no native
   sd_notify. We could write a tiny wrapper that polls CDP
   `/json/version` and calls `systemd-notify --ready` — but that's
   accreting a wrapper for a marginal gain (lohar already restarts
   on crash). Skipping unless someone hits the case.

2. **`bhatti image save` of a computer-tier sandbox carries the
   password.** If the saved image is shared, that's a leaked secret.
   Document in the "image save" docs separately. Out of scope here
   — but the right long-term fix is to teach `image save` to scrub
   well-known secret paths (`/root/.kasmpasswd`, `/root/.vnc/
   cleartext`) and let firstboot re-run on the new sandbox. Filed
   as a follow-up.

3. **`docker run hello-world` from `bhatti exec` (uid 1000)** still
   relies on the world-writable socket workaround. The cleaner
   path — lohar exec preserves supplementary groups so `docker`
   group membership applies — is a separate lohar change. Today's
   `chmod 666 /var/run/docker.sock` ExecStartPost preserves
   identical user-visible behaviour to the current init.sh.
