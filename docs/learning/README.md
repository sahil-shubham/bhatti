# bhatti as a Systems Curriculum

*Last updated: 2026-06-13. Reflects code as of the `cmd/vmm` + `pkg/engine/krucible` era.*

You built bhatti — a Firecracker microVM orchestrator — with heavy help from AI
tools and real engineering discipline. It works in production on `agni-01`,
survives crashes, and wakes a paused VM in under 4ms. But building something
with an assistant and *understanding it down to the metal* are different skills.
This directory closes that gap.

The goal is not "read the docs." The goal is: **you can be paged at 3am about a
production bhatti box, SSH in, and reason from symptom to kernel without
guessing.** That is a learnable skill, and bhatti is an unusually good textbook
because *you already lived the hard parts* — they're recorded in 139 pi sessions
(~232MB, 44,789 messages, 2,157 of your own prompts) and in `docs/archive/`.

This is **not** a from-scratch CS degree. It's a targeted program that uses the
system you already built as the spine, and pulls in first-principles theory
exactly where you need it.

---

## Why this works (and why generic courses don't)

A generic OS course teaches you `fork()` with a toy example you forget in a week.
Here, `fork()` is the thing that runs `npm install` inside a customer's sandbox,
and `Kill(-pid)` is why cancelling it doesn't leave orphaned `node` processes
eating their RAM. You have **stakes and memory**. We exploit that.

Every topic is taught on four legs:

1. **Symptom** — a real, observable behavior, usually a bug you actually hit.
   (See [`war-stories.md`](./war-stories.md) — your production incidents, mined
   from the sessions and `docs/archive/INVESTIGATION-*.md`.)
2. **First principles** — the OS/hardware/network concept underneath, with one
   canonical, finishable resource (a chapter, not a textbook).
3. **In your code** — the exact file and function where bhatti does this thing,
   so the abstraction has a body.
4. **Hands-on** — build a 50-line version from scratch, or instrument the real
   thing, or *break it and fix it*. Reading is not understanding; reproducing is.

Then a **mastery check**: questions you must answer *without looking*. If you
can't, you skimmed.

---

## The files

| File | What it is | Use it when |
|------|-----------|-------------|
| [`00-curriculum.md`](./00-curriculum.md) | The whole map. 10 domains → ~30 modules. Each maps concept → resource → your code → war story → exercises → mastery check. | Start here. This is the answer to "how do I cover the gap." |
| [`war-stories.md`](./war-stories.md) | Your real production incidents as debugging case studies. The rory snapshot corruption, the two bricked Pis, vsock dying after restore, the cascading TAP deletion. | When you want to learn debugging *as a skill*, not topics. |
| [`module-01-processes-and-init.md`](./module-01-processes-and-init.md) | One module, built out completely, as the template for the rest. Processes, signals, PID 1, the guest agent. | Start your *actual studying* here. Then we build the next module together. |
| [`progress.md`](./progress.md) | Your logbook. Tick modules, note what surprised you, record questions. | Every session. This is how you'll know it worked. |

Predecessor: [`../archive/PLAN-learning.md`](../archive/PLAN-learning.md) — your
earlier 8-week doc-rewrite plan. Still good. This curriculum supersedes it:
it's debugging-first, it incorporates the war stories from the sessions, and it
covers the subsystems that didn't exist when that plan was written (the systemd
shim in `cmd/lohar/systemctl.go`, the `pkg/engine/krucible` VMM abstraction, the
`cmd/vmm` macOS Hypervisor.framework experiment).

---

## How to actually use this (the method, concretely)

Pick a module from `00-curriculum.md`. For each:

1. **Read the war story first** if one is linked. Feel the pain. That's your
   motivation and your test case.
2. **Read your own code** for the area (files are listed). Don't try to
   understand it yet — just map the territory. Where are the functions? What
   calls what?
3. **Study the one linked resource.** One. Finish it. Don't rabbit-hole into a
   textbook; the curriculum is sequenced so gaps get filled later.
4. **Do the lab.** This is non-negotiable and where 80% of the learning is. The
   labs are designed to run on your Pi5 (`raspi-5a`) or in a local Linux VM —
   never on `agni-01` (production). When a lab says "break it," do it on a
   throwaway sandbox.
5. **Answer the mastery check out loud or in writing**, in `progress.md`. If you
   reach for the code, you haven't got it yet — that's fine, loop back.
6. **Optionally rewrite the matching doc** in your own voice (the old
   PLAN-learning insight: rewriting a doc is proof you understood it).

**Pace:** one module is roughly one focused evening to one weekend. The whole
program is a few months at a relaxed pace. Don't binge — systems knowledge needs
sleep between sessions to consolidate. Depth compounds; breadth doesn't.

**Order:** the curriculum is sequenced bottom-up (processes → memory → I/O →
virtualization → networking → the distributed/operational layer) because each
layer assumes the one below. You *can* jump to a domain you're debugging right
now — each module lists its prerequisites — but the first time through, go in
order. The foundations (Domain 1 and 2) make everything above them click.

---

## A safety note you of all people should respect

`cmd/lohar/main.go` has this guard, and it exists because you learned the hard way:

```go
// Running it on a real host as root has powered off two Pi5 machines
// in this project's history. Refuse to proceed unless we really are PID 1.
if os.Getpid() != 1 { ... os.Exit(2) }
```

Several labs involve namespaces, mounts, `unshare`, raw sockets, and PID-1
behavior. **Do them in a disposable VM or a bhatti sandbox, not on a machine you
care about.** The whole point of bhatti is that you have a safe place to break
things. Use it: `bhatti create --name lab --image minimal` and go wild.

---

## What "done" looks like

You'll know you own this when:

1. You can trace `bhatti exec dev -- echo hello` from your keystroke to the byte
   coming back, naming every layer (CLI → HTTP → engine → wire frame → vsock/TAP
   → lohar → `fork`/`exec` → PTY/pipe → frame → back) without looking.
2. You can read a new bug report and know *which file to open* in under a minute.
3. You can explain the rory incident — diff snapshots, dirty-page tracking,
   why a restored VM's Firecracker process dies in <1s — to another engineer,
   from memory, including why the fix was "always Full snapshots."
4. Given a `dmesg`, an `strace`, or a hung process, you have a *procedure*, not a
   vibe.
5. You stop saying "the AI wrote that part" and start saying "here's why it's
   built this way, and here's what I'd change."
