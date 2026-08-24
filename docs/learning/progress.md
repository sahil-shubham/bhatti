# Progress Logbook

The single most important habit: write here after every study session. Not for
me — for you. The act of writing "what surprised me" is what converts reading
into understanding. Future-you debugging at 3am will thank present-you.

For each module: tick it, rate your confidence (1–5), and write 2–3 lines of
*what surprised you* and *what you still can't explain*. The "can't explain" notes
are your real curriculum.

---

## Tracker

| # | Module | Done | Confidence (1–5) | Doc rewritten? |
|---|--------|------|------------------|----------------|
| 0.1 | Observe, hypothesize, test, narrow | ☐ | | |
| 0.2 | The tools that see everything | ☐ | | |
| 1.1 | fork, exec, wait, exit codes | ☐ | | |
| 1.2 | Signals and process groups | ☐ | | |
| 1.3 | PID 1: mounts, reboot, zombies | ☐ | | |
| 1.4 | PTYs, sessions, SIGHUP | ☐ | | |
| 1.5 | Reimplementing systemd | ☐ | | |
| 2.1 | HW virt: VT-x, EPT, /dev/kvm | ☐ | | |
| 2.2 | Firecracker: the minimal VMM | ☐ | | |
| 2.3 | virtio | ☐ | | |
| 3.1 | What a snapshot is | ☐ | | |
| 3.2 | Diff vs Full & dirty-page tracking | ☐ | | |
| 3.3 | Memory ballooning | ☐ | | |
| 3.4 | Thermal state machine & cold-wake | ☐ | | |
| 4.1 | TAP devices and bridges | ☐ | | |
| 4.2 | iptables, NAT, FORWARD | ☐ | | |
| 4.3 | The kernel ip= trick | ☐ | | |
| 4.4 | Reverse proxy & WebSocket gotcha | ☐ | | |
| 4.5 | ARP trick & DNS forwarding | ☐ | | |
| 5.1 | Binary framing & interleaving | ☐ | | |
| 5.2 | Connection lifecycle, auth, streaming | ☐ | | |
| 6.1 | Crash consistency: fsync+rename | ☐ | | |
| 6.2 | ext4 images, overlays, btrfs | ☐ | | |
| 6.3 | SQLite, WAL, pure-Go | ☐ | | |
| 6.4 | Config drive | ☐ | | |
| 6.5 | Secrets at rest: age | ☐ | | |
| 7.1 | Kernel config, building, boot args | ☐ | | |
| 7.2 | FUSE | ☐ | | |
| 7.3 | binfmt_misc & multi-arch | ☐ | | |
| 8.1 | Go memory model & capture-and-release | ☐ | | |
| 8.2 | State machines & circuit breakers | ☐ | | |
| 8.3 | Crash recovery on restart | ☐ | | |
| 9.1 | The jailer | ☐ | | |
| 9.2 | Multi-tenant isolation | ☐ | | |
| 9.3 | Guest hardening & unscrapable URLs | ☐ | | |
| 10.1 | Benchmarking honestly | ☐ | | |
| 10.2 | Zero-downtime deploys | ☐ | | |
| 10.3 | Observability | ☐ | | |
| 10.4 | VMM portability (krucible/libkrun/HVF) | ☐ | | |

## War stories reproduced

| # | Story | Lab done | Could explain from memory? |
|---|-------|----------|----------------------------|
| W1 | rory diff-snapshot corruption | ☐ | ☐ |
| W2 | kowshik file corruption | ☐ | ☐ |
| W3 | vsock dies after restore | ☐ | ☐ |
| W4 | two bricked Pis (PID-1 reboot) | ☐ | ☐ |
| W5 | share WebSocket / Cloudflare | ☐ | ☐ |
| W6 | browser tier / leaked jails | ☐ | ☐ |
| W7 | cold-wake 42ms vs 360ms | ☐ | ☐ |
| W8 | boot/create perf | ☐ | ☐ |
| W9 | systemd-shim reckoning | ☐ | ☐ |
| W10 | VMM-agnostic frontier | ☐ | ☐ |

---

## Log

### [date] — Module X.Y
- **What I did:**
- **What surprised me:**
- **What I still can't explain (← revisit):**
- **A `???` in the capstone I couldn't fill:**

<!-- copy the block above for each session -->

---

## The five "done" tests (from the README) — check when true

- ☐ I can trace `bhatti exec dev -- echo hello` keystroke→byte→back, naming every
  layer, without looking.
- ☐ I can read a bug report and know which file to open in under a minute.
- ☐ I can explain the rory incident (W1) from memory, including why "always Full".
- ☐ Given a `dmesg` / `strace` / hung process, I have a procedure, not a vibe.
- ☐ I say "here's why it's built this way" instead of "the AI wrote that part."
