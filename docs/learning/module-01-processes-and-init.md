# Module 1 — Processes, Signals, and the Init System (lohar)

*This module is built out completely. It's the template for every other module
in the curriculum. Expect to spend a focused weekend on it.*

**Prerequisites:** Module 0 (the debugging mindset). A Linux machine you can
break — a local VM or a `bhatti create --name lab --image minimal` sandbox. `gcc`
and `go` installed.

**Why this is Module 1:** lohar is PID 1 in every bhatti VM. It is the first
thing the kernel runs, it forks every command a user executes, it owns every
interactive shell, and in `cmd/lohar/systemctl.go` it reimplements a working
slice of systemd. Almost everything else in bhatti sits on top of the concepts
here: processes, signals, process groups, PTYs, and what it means to be PID 1.
If these are solid, the rest of the system stops feeling like magic.

By the end you will be able to trace `bhatti exec dev -- npm install` from the
`fork()` to the exit code, explain why cancelling it kills the whole tree, and
say precisely why running lohar on a host once powered off two Raspberry Pis.

---

## 1.0 The mental model (read this first)

A **process** is a running program: an address space (its memory), a thread (or
several) of execution, and a bundle of kernel-tracked state (open files, the
current directory, its user id, its parent, its children). Every process except
the first is created by *another* process.

Unix creates processes with a two-step dance that surprises everyone the first
time:

1. **`fork()`** — the kernel makes a near-identical *copy* of the calling
   process. Now there are two. They differ only in the return value of `fork()`:
   the parent gets the child's PID, the child gets `0`.
2. **`execve()`** — the child *replaces its own program image* with a new
   executable. Same process (same PID), totally different code running.

Then:

3. **`wait()`/`waitpid()`** — the parent collects the child's exit status when it
   dies. Until it does, the dead child is a **zombie** (it's done running but the
   kernel keeps a slot so the parent can read the exit code).

Go hides this behind `os/exec`, but it's exactly what happens:

```go
cmd := exec.Command("npm", "install")  // describe the program
cmd.Start()                            // fork() + execve()
cmd.Wait()                             // waitpid() — collects exit status
```

Hold that picture. Everything below is detail on it.

---

## 1.1 fork, exec, wait, and exit codes ★

### First principles

- **Resource (do this):** OSTEP Chapter 5, "Process API"
  (`pages.cs.wisc.edu/~remzi/OSTEP/cpu-api.pdf`, ~15 pages). It walks `fork`,
  `exec`, `wait` with tiny C programs. Read it with a terminal open and run each
  example.
- **Key idea:** an **exit code** is one byte the child returns to the parent. `0`
  = success by convention. But the kernel packs more into the *wait status*: if
  the child was killed by a *signal*, the status encodes the signal number, and
  the shell convention is to report it as `128 + signal`. So `137 = 128 + 9`
  (SIGKILL), `139 = 128 + 11` (SIGSEGV).

### In your code

bhatti decodes exactly this. From `cmd/lohar/exec.go`:

```go
func exitCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		status := exitErr.Sys().(syscall.WaitStatus)
		if status.Signaled() {
			return 128 + int(status.Signal())   // ← the convention, in the wild
		}
		return status.ExitStatus()
	}
	return 1
}
```

`WaitStatus` is the raw integer the kernel fills in via `wait4()`. `Signaled()`
checks a bit; `Signal()` extracts the signal number. This is the literal
mechanism OSTEP Ch.5 describes, in production Go.

### Lab 1.1 — fork/exec/wait, in C and in Go

Create `fork.c`:

```c
#include <stdio.h>
#include <unistd.h>
#include <sys/wait.h>
int main() {
    pid_t pid = fork();
    if (pid == 0) {                 // child
        printf("child: pid=%d, replacing myself with `ls`\n", getpid());
        execlp("ls", "ls", "-l", (char*)NULL);
        perror("execlp");           // only reached if exec FAILS
        _exit(127);
    } else {                        // parent
        int status;
        waitpid(pid, &status, 0);
        printf("parent: child %d exited, WIFEXITED=%d code=%d signaled=%d sig=%d\n",
               pid, WIFEXITED(status), WEXITSTATUS(status),
               WIFSIGNALED(status), WTERMSIG(status));
    }
}
```

Run it: `gcc fork.c -o fork && ./fork`. Then make the child die by a signal —
replace the `execlp` with `raise(SIGKILL);` (add `#include <signal.h>`) and watch
`signaled=1 sig=9`.

Now the Go equivalent, `forkgo.go`:

```go
package main
import ("fmt"; "os/exec"; "syscall")
func main() {
	cmd := exec.Command("sh", "-c", "exit 42")
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		ws := ee.Sys().(syscall.WaitStatus)
		fmt.Println("exit code:", ws.ExitStatus()) // 42
	}
	cmd2 := exec.Command("sh", "-c", "kill -9 $$") // kill self with SIGKILL
	err = cmd2.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		ws := ee.Sys().(syscall.WaitStatus)
		fmt.Println("signaled:", ws.Signaled(), "→ 128+sig =", 128+int(ws.Signal()))
	}
}
```

`go run forkgo.go` should print `exit code: 42` then `signaled: true → 128+sig =
137`. You just reproduced `exitCodeFromErr`.

### Mastery check 1.1

1. What is exit code 137? 139? Why the `128 +` convention?
2. In `fork.c`, why is the line *after* `execlp` only reached on failure?
3. What's a zombie, and which call reaps it?

*(Answers at the bottom.)*

---

## 1.2 Signals and process groups ★★

### First principles

- **Resource:** `man 7 signal` (the catalog), then `man 2 setpgid` and
  `man 2 kill`. Read the "Standard signals" table in `signal(7)`.
- **Key ideas:**
  - A **signal** is an asynchronous notification to a process. The process can
    install a *handler*, or use the default action (often "terminate").
  - **SIGKILL (9)** and **SIGSTOP (19)** cannot be caught, blocked, or ignored.
    SIGKILL is the kernel's "you die now." SIGTERM (15) is the polite "please
    clean up and exit" — *catchable*.
  - **SIGHUP (1)** = "the terminal hung up." Closing a PTY master sends SIGHUP to
    the foreground process group (this is Module 1.4's punchline).
  - **SIGCHLD** is sent to a parent when a child dies — how `wait` knows to wake.
  - A **process group** is a set of processes you can signal together. A shell
    running `npm install` creates a tree (`npm` → `node` → workers). Put them all
    in one group and you can kill the whole thing with one call: `kill(-pgid)`
    (note the *negative* PID).

### In your code

`cmd/lohar/exec.go`, `handlePipedExec` — this is the heart of `bhatti exec`:

```go
cmd.SysProcAttr = &syscall.SysProcAttr{
	Setpgid:    true,                                  // child leads a new process group
	Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
}
// ... later, when the host sends a KILL frame:
case proto.KILL:
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)  // negative PID = whole group
	}
```

`Setpgid: true` makes the spawned command the leader of its own process group
whose ID equals its PID. `Kill(-pid, SIGKILL)` then signals *every* process in
that group — so `npm`, the `node` it spawned, and all their children die
together. Without this, you'd kill `npm` and orphan a `node` that keeps chewing
the customer's RAM.

> **Spot-the-drift exercise (do this).** Open `pkg/agent/proto/constants.go` and
> find the `KILL` constant. Its comment says *"agent sends SIGTERM to child."*
> Now look at the code above — it sends `SIGKILL`. The doc and the code disagree.
> **Which is right? Why might the implementation have changed from SIGTERM to
> SIGKILL?** (Think: a misbehaving process that *ignores* SIGTERM. What do you
> want "cancel" to guarantee?) This is exactly the kind of stale comment that
> bites you in production. Decide the correct behavior and fix the comment (or
> the code) — then you'll have made your first real contribution from
> understanding, not vibes.

### Lab 1.2 — kill a tree vs kill a leader

In a shell:

```bash
# Start a parent that spawns children, all in one process group
sh -c 'sleep 300 & sleep 300 & echo "pgid=$$ children spawned"; wait' &
PARENT=$!
# Inspect the tree
ps -o pid,ppid,pgid,comm -g $(ps -o pgid= -p $PARENT)
# Kill the LEADER only:
kill $PARENT
ps -o pid,ppid,pgid,comm | grep sleep   # the sleeps are still alive — orphaned!
# Now kill the GROUP:
kill -- -$(ps -o pgid= -p $PARENT | tr -d ' ')   # negative = whole group
```

Watch the orphaned `sleep`s get reparented to PID 1 (`ppid` becomes 1) when you
kill only the leader. Then watch them all vanish when you signal the group.

### Mastery check 1.2

1. You run `bhatti exec dev -- npm install` and Ctrl-C it. Why does the `node`
   child die too? Which one line of `SysProcAttr` makes that work?
2. Why can't a process install a handler for SIGKILL?
3. What's the difference between `kill 1234` and `kill -1234`?

---

## 1.3 PID 1: mounts, the reboot handler, and the zombie decision ★★★

*This is the module's centerpiece and the one with a body count (two Pis).*

### First principles

- **Resource:** read "What is PID 1 / the responsibilities of init" — Rich
  Felker's `dumb-init`/`minit` rationale is the clearest short version. Then
  `man 2 reboot` and `man 2 mount`.
- **Key ideas:** PID 1 is the *first* userspace process the kernel starts (via the
  `init=` boot arg). It is special:
  - It is the ancestor of everything. When any process's parent dies, the
    orphan is **reparented to PID 1**.
  - If PID 1 *exits*, the kernel **panics** ("Attempted to kill init!"). So PID 1
    must never return.
  - It is responsible for early system setup: mounting `/proc`, `/sys`, `/dev`,
    `/tmp`, bringing up loopback, and (classically) **reaping zombies** — the
    orphans reparented to it become its children, and if it never `wait()`s them
    they pile up as zombies forever.
  - Default signal dispositions are different for PID 1 (the kernel won't apply
    default-fatal actions to it for signals it hasn't handled — a safety so you
    can't accidentally `kill` init).

### In your code

`cmd/lohar/main.go`, `runAgent()`. First, the guard that exists *because of W4*:

```go
// SAFETY GUARD: lohar's runAgent does PID-1 things (mounts /proc, installs a
// SIGTERM handler that calls reboot(POWER_OFF), brings up loopback, starts every
// enabled service). All of that is only safe inside a Firecracker microVM where
// lohar IS PID 1. Running it on a real host as root has powered off two Pi5
// machines in this project's history. Refuse to proceed unless we really are PID 1.
if os.Getpid() != 1 {
	fmt.Fprintf(os.Stderr, "lohar: refusing to runAgent: not PID 1 (PID=%d). ...")
	os.Exit(2)
}
```

Then the PID-1 init duties — the mount sequence:

```go
mustMount("proc", "/proc", "proc", 0, "")
mustMount("sysfs", "/sys", "sysfs", 0, "")
mustMount("devtmpfs", "/dev", "devtmpfs", 0, "")
os.Chmod("/dev/fuse", 0666)                 // ← enables FUSE (Module 7.2)
mustMount("devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666")
mustMount("tmpfs", "/tmp", "tmpfs", 0, "")
// ... cgroup2, binfmt_misc (Module 7.3), /dev/shm ...
bringUpInterface("lo")
```

And the deliberate non-decision about zombies, from `decisions.md §3`:

> The zombie reaping is intentionally omitted — Go's runtime manages `SIGCHLD`
> for `exec.Command` processes, and a manual `Wait4(-1)` reaper would race with
> `cmd.Wait()`. Orphan zombies are acceptable because the VM is short-lived.

This is a *real engineering judgment*, not laziness: a textbook PID 1 reaps
zombies, but doing so in Go would race with the runtime's own child handling. The
mitigating fact (short-lived VM) makes the tradeoff safe. **Understanding when to
*break* the textbook rule is exactly the senior skill you're building.**

Finally, why lohar never returns — it ends by blocking forever (a `select{}` or
equivalent after starting listeners). If `runAgent` ever returned, the kernel
would panic.

### Lab 1.3 — be PID 1 (safely, in a throwaway VM)

> Do this in a disposable VM or sandbox. Re-read the W4 warning. We are
> deliberately doing the dangerous thing in a safe place.

Write `tinyinit.go`:

```go
//go:build linux
package main
import ("fmt"; "os"; "syscall"; "os/signal")
func main() {
	if os.Getpid() != 1 {
		fmt.Println("not pid 1, refusing"); os.Exit(2)   // copy lohar's guard!
	}
	syscall.Mount("proc", "/proc", "proc", 0, "")
	fmt.Println("tinyinit: I am PID 1. /proc mounted. Try `cat /proc/1/status` from another shell.")
	// Catch SIGTERM so we can power off cleanly — DEMONSTRATES the W4 danger.
	c := make(chan os.Signal, 1); signal.Notify(c, syscall.SIGTERM)
	<-c
	fmt.Println("tinyinit: got SIGTERM, powering off")
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF) // ← the line that bricked the Pis
	select {}                                          // never return
}
```

Build it `GOOS=linux go build -o tinyinit tinyinit.go`, drop it into a minimal
rootfs as `/tinyinit`, and boot a kernel with `init=/tinyinit`. Observe: it's PID
1, `/proc` exists, and a `kill` (which sends SIGTERM) powers off the VM. **Now you
understand W4 in your bones** — that exact `Reboot` call running on a host with
PID != 1 is what powered off the Pis. The guard is the fix.

(If you don't want to build a rootfs yet, do the lighter version: run `tinyinit`
*not* as PID 1 and watch the guard refuse. Then read `runAgent` and list every
PID-1 duty it performs.)

### Mastery check 1.3

1. Why must `runAgent` never return? What does the kernel do if PID 1 exits?
2. A process inside your VM forks a child, then the parent exits. What is the
   child's parent now? Who is responsible for reaping it?
3. Why is bhatti's choice to *not* reap zombies acceptable here but would be a
   bug in a long-running host init like systemd?
4. Explain W4 to someone in three sentences.

---

## 1.4 PTYs, sessions, and SIGHUP ★★★

### First principles

- **Resource:** "The TTY demystified" (Linus Åkesson, `linusakesson.net/programming/tty/`)
  — the canonical explainer. Then `man 4 pts` and `man 3 openpty`.
- **Key ideas:**
  - A **pseudo-terminal (PTY)** is a pair: a **master** and a **slave**. A program
    that wants to *drive* a terminal (your SSH server, lohar, `tmux`) holds the
    master. The program that thinks it's *at* a terminal (a shell) runs on the
    slave.
  - Between them sits the kernel's **line discipline**: it echoes typed
    characters, handles backspace/line editing, and turns Ctrl-C into SIGINT to
    the foreground group, Ctrl-Z into SIGTSTP, etc.
  - When the **master is closed**, the kernel sends **SIGHUP** to the foreground
    process group on the slave. That's why closing a terminal kills the shell and
    its jobs. *This is the bug bhatti's session model exists to defeat.*

### In your code

`decisions.md §4` ("Exec is sessions") tells you *why* this matters — it was born
from your own pain:

> During development, SSH connections to the Pi would drop (Wi-Fi, laptop sleep).
> A running `npm install` inside a shell would get killed (SIGHUP from PTY
> close). With sessions: the host disconnects → session detaches (no SIGHUP) →
> the process keeps running, output goes to the 64KB scrollback ring buffer →
> the host reconnects → scrollback is replayed, live I/O resumes.

The mechanics live in `cmd/lohar/tty.go` (PTY allocation: open `/dev/ptmx`,
unlock, start the shell on the slave) and `cmd/lohar/session.go` (the session
registry and the 64KB ring buffer, see also
`pkg/engine/firecracker/ringbuffer.go`). The key trick: lohar **keeps the master
fd open** even when no host is attached, so the slave-side shell never gets
SIGHUP. Output flows into the ring buffer; on reattach it's replayed.

### Lab 1.4 — open a PTY, then break it with SIGHUP

`pty.go`:

```go
//go:build linux
package main
import ("fmt"; "io"; "os"; "os/exec"; "github.com/creack/pty") // go get github.com/creack/pty
func main() {
	c := exec.Command("bash")
	ptmx, err := pty.Start(c)   // allocates master/slave, runs bash on the slave
	if err != nil { panic(err) }
	go io.Copy(os.Stdout, ptmx) // master → our stdout
	go io.Copy(ptmx, os.Stdin)  // our stdin → master
	fmt.Fprintln(ptmx, "echo hello from the slave; ps -o pid,tty,comm")
	// EXPERIMENT: close the master and watch bash get SIGHUP and die:
	// ptmx.Close()
	c.Wait()
}
```

Run it, type into the shell, see the echo (that's the line discipline). Then
uncomment `ptmx.Close()` — the bash dies immediately (SIGHUP). That single line
*is* what happens when your SSH connection drops. bhatti's fix is to **not** close
the master and to buffer output instead — go read `session.go` and find where it
does exactly that.

### Mastery check 1.4

1. Walk the path of a single keystroke: your terminal → ??? → the shell's stdin.
   Where does echo happen?
2. Why does a bhatti session survive a host disconnect when a raw SSH session
   wouldn't? Name the one fd that must stay open.
3. Where does a session's output go while no host is attached, and what's the
   size limit?

---

## 1.5 Reimplementing systemd: units, dependencies, conditions, cgroups ★★★

*This is the big one and the most "you built this without fully understanding it"
surface in the whole project.*

### First principles

- **Resource:** `man systemd.unit` and `man systemd.service` (skim — you want the
  *vocabulary*: `After`, `Before`, `Wants`, `Requires`, `Condition*`, `Type=`,
  `ExecStart`, `WantedBy`). Then Lennart Poettering's "systemd for Administrators,
  Part 1" for the *why*.
- **Key idea:** an init/service manager is, at its core, **a dependency-ordered,
  condition-gated supervisor that places services into cgroups**. Given a set of
  unit files declaring `After=`/`Wants=`, it builds a graph, topologically sorts
  it, evaluates `Condition*` gates, and starts services in order, each in its own
  cgroup so it can be tracked and resource-limited.

### In your code (the W9 surface)

You built a shim for this, and W9 is the moment you realized it had grown without
a model. The pieces:

| File | What it is | systemd analog |
|------|-----------|----------------|
| `cmd/lohar/systemctl.go` (56KB) | the `systemctl` command surface + manager | `systemctl` + PID-1 manager |
| `cmd/lohar/unit.go` | unit-file parser | systemd's unit loader |
| `cmd/lohar/depgraph.go` | dependency graph + topological ordering | the transaction/job engine |
| `cmd/lohar/conditions.go` | `Condition*` evaluation | `ConditionPathExists=` etc. |
| `cmd/lohar/cgroup.go` | cgroup creation/placement | systemd's cgroup hierarchy |
| `cmd/lohar/tmpfiles.go` | `tmpfiles.d` handling | `systemd-tmpfiles` |
| `cmd/lohar/notify.go` | `sd_notify` readiness protocol | `Type=notify` |
| `cmd/lohar/spawn.go` | `lohar spawn` helper | fixes a cgroup-placement race for forking daemons |

The `spawn.go` helper is the subtle one. When a daemon **forks** (the parent exits
and a child keeps running, `Type=forking`), *who is in the cgroup* at the moment
of the fork matters — there's a race between placing the process in its cgroup and
the fork happening. `lohar spawn` (a private, non-PATH supervisor primitive, see
the comment in `main.go`) exists to win that race deterministically.

### Lab 1.5 — a 100-line init that orders units

Write a tiny init that reads three "unit" files and starts them in dependency
order:

```
# a.unit
ExecStart=/bin/echo starting A
# b.unit
After=a.unit
ExecStart=/bin/echo starting B
# c.unit
After=b.unit
ConditionPathExists=/tmp/go
ExecStart=/bin/echo starting C
```

Your program should: parse the files, build a graph from `After=`, topologically
sort it, skip any unit whose `ConditionPathExists` fails, and `exec` the rest in
order. Run it once without `/tmp/go` (C is skipped), then `touch /tmp/go` and run
again (C runs last). Now open `depgraph.go` and `conditions.go` and compare your
toy to the real thing — find how it detects dependency *cycles* (what should
happen if `a` requires `b` and `b` requires `a`?).

### Mastery check 1.5

1. Why does a `Type=forking` daemon need `lohar spawn` instead of a plain
   `exec.Command`? What exactly is the race?
2. Given units with `A After=B`, `B After=C`, in what order do they start? What
   does the graph look like?
3. What's the difference between `Wants=` and `Requires=` and why does it matter
   when a dependency fails?
4. (W9 teach-back) Explain why "compare our shims to their real systemd parents"
   was the right move instead of patching the symptom fastidious reported.

---

## Capstone: trace `bhatti exec dev -- npm install` end to end

You now know enough to narrate the process side of a real command. Fill in every
`???` from memory; then verify against the code. (Networking/wire-protocol legs
get their full treatment in Modules 4 and 5 — here, focus on the process legs.)

1. CLI sends an HTTP request to the daemon → daemon calls the engine → engine
   opens a wire connection to lohar and sends an `EXEC_REQ` frame
   (`pkg/agent/proto/constants.go`).
2. lohar's handler dispatches to `handlePipedExec` (`cmd/lohar/exec.go`).
3. It builds `exec.Command`, sets `SysProcAttr{Setpgid: ???, Credential: uid ???}`.
4. `cmd.Start()` does `???` + `???` (two syscalls).
5. Three goroutines pump stdout→`STDOUT` frames, stderr→`STDERR` frames, and
   conn→stdin; all frame writes serialize through the `tx` channel (why? →
   Module 5.1).
6. You Ctrl-C. The host sends a `KILL` frame. lohar runs
   `syscall.Kill(???, SIGKILL)` — the `???` is negative because `???`.
7. `cmd.Wait()` returns; `exitCodeFromErr` turns the wait status into `128 + ???`
   if signaled.
8. An `EXIT` frame carries the code back; the CLI prints it.

If you can fill every `???` without looking, you've got Module 1.

---

## Answer key (peek only after trying)

**1.1** — (1) 137 = 128+9 = killed by SIGKILL; 139 = 128+11 = SIGSEGV; the `128+`
convention lets a shell distinguish "exited with code N" from "killed by signal N"
in one byte. (2) `execve` *replaces* the process image, so on success there's no
code left to run the next line; reaching it means exec failed. (3) A zombie is a
dead-but-not-yet-collected child; `wait`/`waitpid`/`wait4` reaps it.

**1.2** — (1) The `node` child is in the same process group, and lohar does
`Kill(-pid, SIGKILL)` which signals the whole group; `Setpgid: true` is the line
that creates that group. (2) SIGKILL is uncatchable by design so a process can
always be killed by the kernel. (3) `kill 1234` signals process 1234; `kill
-1234` signals every process in process group 1234.

**1.3** — (1) PID 1 exiting panics the kernel ("Attempted to kill init!"), so
`runAgent` blocks forever. (2) The child is reparented to PID 1 (lohar);
normally PID 1 reaps it, but bhatti deliberately doesn't (short-lived VM). (3)
A long-running host init that never reaps would accumulate zombies indefinitely,
exhausting the PID table; a microVM is destroyed long before that matters. (4)
lohar's SIGTERM handler calls `reboot(POWER_OFF)`, which is correct when it's PID
1 in a VM but catastrophic on a host; it was invoked on a host, so it powered the
machine off; the `os.Getpid() != 1` guard now prevents it.

**1.4** — (1) keystroke → terminal → PTY master → kernel line discipline (echo
happens here, plus it may send to the slave) → slave → shell stdin. (2) lohar
keeps the *master fd open* on detach, so the slave-side shell never receives
SIGHUP; SSH closes its PTY master on disconnect, sending SIGHUP. (3) Into the
64KB ring buffer (`session.go` / `ringbuffer.go`), replayed on reattach.

**1.5** — (1) For `Type=forking`, the parent exits and a child continues; without
the helper there's a race over whether the surviving child lands in the correct
cgroup before the parent vanishes; `lohar spawn` performs the placement
deterministically. (2) C, then B, then A (you must start what others depend on
first); the graph is a chain C→B→A. (3) `Requires=` makes the dependent fail if
the dependency fails; `Wants=` is best-effort (dependent still starts). (4)
Because the symptom was a manifestation of a missing model (no real dependency
ordering/conditions/cgroup placement); patching it would leave the next service to
re-trigger it — fixing the model fixes the class of bug.

---

*Done with Module 1? Log it in [`progress.md`](./progress.md) with: what
surprised you, the doc/code drift you found in 1.2, and your answer to "explain W4
in three sentences." Then we build Module 2 (the wire protocol + storage) or jump
to whichever domain you're debugging right now.*
