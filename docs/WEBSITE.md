# Bhatti — Website & Brand Plan

A design document for bhatti.sh — the public face of the project.
Covers brand identity, logo direction, website structure, copy, and
the two primary conversion paths (self-host install and hosted demo).

---

## 1. Brand Identity

### 1.1 The Metaphor

Bhatti (भट्टी) is a furnace. Lohar (लोहार) is the blacksmith who works
inside it. This is the core visual and verbal metaphor — a place where
raw material (code, agents, workloads) is shaped by fire into something
useful. The furnace provides the controlled environment; the blacksmith
does the work.

This metaphor is strong because it maps cleanly to the product:

| Metaphor | Product |
|----------|---------|
| The furnace (bhatti) | The host daemon — manages fire (VMs), controls temperature (thermal states), provides the environment |
| The blacksmith (lohar) | The guest agent — PID 1 inside every VM, the one doing the actual work |
| Fire | Firecracker microVMs — literal fire in the name |
| Hot/warm/cold | Thermal management states — the furnace regulating itself |
| Forging | Creating sandboxes — shaping isolated environments from raw compute |

### 1.2 Logo Direction

The current logo is the hammer-and-pick emoji (⚒). It's placeholder
energy. The new logo should be a proper mark that works at 16px (favicon),
32px (GitHub), and full size (website hero).

**Concept: The Bhatti Mark**

A stylized furnace opening — an arch shape with the suggestion of
contained heat/fire inside. Think of the mouth of a traditional Indian
bhatti (a clay or brick furnace with an arched opening). Abstract it to
a geometric form:

```
     ╭─────────╮
    ╱     ∆     ╲        ← arch/dome (the furnace opening)
   │    ∆ ∆ ∆    │       ← abstract flame shapes inside
   │             │
   └─────────────┘        ← base/foundation
```

**Design principles for the mark:**

- **Geometric, not illustrative.** No realistic flames, no clipart
  anvils. A clean symbol that reads at small sizes.
- **Single color works.** Must be legible in monochrome (README, CLI
  output, terminal). Color is additive, not required.
- **The arch is the signature.** The curved top of a furnace opening is
  the most distinctive shape. It should be recognizable even without
  the fire elements.
- **Warmth, not aggression.** The fire metaphor should feel like a
  craftsman's tool, not destruction. Warm amber/orange tones, not red.

**Color palette:**

| Role | Color | Usage |
|------|-------|-------|
| Primary | `#F97316` (amber-500) | Logo fire element, primary accent, CTAs |
| Primary dark | `#EA580C` (amber-600) | Hover states, secondary emphasis |
| Ember | `#FB923C` (amber-400) | Highlights, gradients, glow effects |
| Background | `#09090B` (zinc-950) | Page background (dark mode default) |
| Surface | `#18181B` (zinc-900) | Cards, code blocks, elevated surfaces |
| Border | `#27272A` (zinc-800) | Subtle dividers |
| Text | `#FAFAFA` (zinc-50) | Primary text |
| Text muted | `#A1A1AA` (zinc-400) | Secondary text, descriptions |

The amber/orange sits naturally in the furnace metaphor and stands out
against the dark zinc palette. It's warm without being alarming.

**Alternative concept: The Anvil Silhouette**

If the furnace arch feels too abstract, an anvil silhouette is the other
strong option — it's the tool of the lohar, universally recognizable,
and has a distinctive angular profile. However, the furnace opening is
more unique (anvils are overused in forge/blacksmith branding) and maps
better to "the environment that contains fire."

**Recommendation:** Commission the furnace-arch mark from a designer,
provide this brief. For immediate use, a typographic lockup (the word
"bhatti" in a clean geometric sans with the arch shape integrated into
a letterform — perhaps the "h" or "tt") works well as a stand-in.

### 1.3 Wordmark

**"bhatti"** — always lowercase. The Hindi origin is the identity; keep
it. No need for "Bhatti Cloud" or "Bhatti VM" — the single word is the
brand.

**Typography:** Use a geometric sans-serif for the wordmark and headings.
Inter, Geist, or Satoshi. The monospace font (for code examples) should
be Geist Mono, JetBrains Mono, or Berkeley Mono.

### 1.4 Voice & Tone

Bhatti's voice is **direct, technical, and confident without being loud.**

- Write like you're explaining to a peer engineer, not selling to a VP.
- State what it does, show the numbers, let the reader decide.
- No superlatives ("blazing fast", "revolutionary"). The benchmarks
  speak louder than adjectives.
- Okay to be opinionated ("We chose X because Y" > "X is supported").
- Hindi/Urdu names are a feature, not something to explain away. The
  name section exists for those who are curious; the homepage doesn't
  need to apologize for non-English naming.

**Examples of voice:**

✓ "A paused sandbox resumes and executes a command in under 3ms."
✓ "Real VMs. Not containers."
✓ "Self-host on a Raspberry Pi. Or a Hetzner box. Or anything with KVM."

✗ "Blazingly fast microVM orchestration platform"
✗ "The future of cloud sandboxing"
✗ "Enterprise-grade isolation for next-gen AI workloads"

---

## 2. Website Structure

One HTML file at `bhatti.sh`. No framework, no build step, no routing.

### 2.1 Page Flow

One page. Scroll down.

```
┌─────────────────────────────────────────────────┐
│  [bhatti]                       GitHub  Try Demo │
├─────────────────────────────────────────────────┤
│  HERO — headline, two CTAs (curl | demo)         │
├─────────────────────────────────────────────────┤
│  WHAT IT IS — three facts + benchmarks           │
├─────────────────────────────────────────────────┤
│  HOW IT WORKS — architecture + thermal diagram   │
├─────────────────────────────────────────────────┤
│  SELF-HOST — the curl command + requirements     │
├─────────────────────────────────────────────────┤
│  TRY THE DEMO — email form → instant API key     │
├─────────────────────────────────────────────────┤
│  USE CASES — agents / browser / docker-in-VM     │
├─────────────────────────────────────────────────┤
│  COMPARISON — honest table vs alternatives       │
├─────────────────────────────────────────────────┤
│  Apache 2.0 · GitHub · Made by Sahil             │
└─────────────────────────────────────────────────┘
```

No routing. No client-side navigation. Anchor links from the nav
scroll to sections. The email form does one `fetch()` and transforms
in-place to show the API key.

---

## 3. Section-by-Section Copy

### 3.1 Navigation

Minimal. Logo + wordmark on the left. Links on the right:

```
[⚒ bhatti]                                GitHub    Try Demo
```

Two links. "GitHub" is plain text (docs live there too — the repo's
`docs/` folder renders fine). "Try Demo" is the only styled link (amber
pill/button) — it scrolls to the email form. Nav is sticky, transparent
over the hero, gains a background on scroll.

### 3.2 Hero

The hero has one job: make the visitor understand what bhatti is in
5 seconds, then give them a clear next action.

**Headline:**

> Self-hostable microVMs for AI agents.

**Subheadline:**

> Each sandbox is a real Linux VM — own kernel, own filesystem, full
> isolation. Created in seconds, paused for free, resumed in
> microseconds. Run it on your hardware.

**Two CTAs, side by side:**

```
┌──────────────────────────────┐  ┌─────────────────────────┐
│  curl -fsSL bhatti.sh/i | sh │  │  Try the hosted demo →  │
└──────────────────────────────┘  └─────────────────────────┘
  Self-host (Linux + KVM)            No install — just an API key
```

The curl command is the primary CTA — it's in a code block styled
element that looks clickable/copyable (click-to-copy with a ✓ toast).
The demo CTA is a standard button, secondary emphasis.

Below the CTAs, a single line of social proof / orientation:

> Open source · Apache 2.0 · ~8,000 lines of Go

**Visual element:** Below or beside the copy, a terminal animation or
static screenshot showing:

```
$ bhatti create --name dev --cpus 2 --memory 1024
Created sandbox "dev" (a1b2c3d4) in 3.4s

$ bhatti exec dev -- uname -a
Linux dev 6.1.155 #1 SMP aarch64 GNU/Linux

$ bhatti exec dev -- node --version
v22.16.0

$ bhatti shell dev
lohar@dev:/workspace$
```

Keep it real — actual commands, actual output. No mocked-up fantasy
terminal with gradient backgrounds.

### 3.3 What It Is

Three facts, each with a short explanation. No icons, no cards —
just typographic hierarchy.

---

**Real VMs. Not containers.**

Every sandbox runs its own Linux kernel in a Firecracker microVM.
Process isolation, filesystem isolation, network isolation. Not a
namespace trick — a separate machine.

---

**Memory snapshots. Not just filesystem.**

When bhatti snapshots a VM, it captures everything: running processes,
open file descriptors, TCP connections, in-memory state. Resume picks up
exactly where it left off. An `npm install` running when you paused
continues running after resume.

---

**Three thermal states. Invisible to you.**

Idle 30 seconds → warm (vCPUs paused, ~400µs resume). Idle 30 minutes →
cold (snapshotted to disk, memory freed, ~50ms resume). Any API request
transparently wakes it. From the outside, every sandbox is always
"running."

---

**The numbers:**

```
                                p50       p95       p99
Exec (single command):          1.0ms     1.2ms     1.3ms
File read (1KB):                472µs     826µs     881µs
Resume + exec (warm):           2.5ms     2.6ms     2.6ms
10 concurrent execs:            18ms      19ms      19ms
VM boot:                        ~3.5s
Diff snapshot:                  ~52ms
Pause/Resume:                   ~400µs
```

<small>Measured on a Raspberry Pi 5 with NVMe. A Hetzner dedicated box
is faster.</small>

### 3.4 How It Works

A simplified architecture diagram — not the full ASCII art from the docs,
but a cleaner version that shows the flow:

```
  Your code / AI agent
        │
        ▼
  bhatti daemon (host)
  REST API · Thermal manager · Multi-tenant auth
        │
        ▼
  Firecracker microVM
  ┌──────────────────────┐
  │  Linux kernel 6.1    │
  │  Ubuntu 24.04 rootfs │
  │  lohar (PID 1)       │
  │  Your code runs here │
  └──────────────────────┘
```

Accompanied by the thermal state diagram:

```
    Hot ──30s idle──→ Warm ──30min idle──→ Cold
     ↑    ~400µs       ↑     ~50ms          │
     └────────────────────────────────────────┘
                any API request
```

This section should be visual-forward. If building with a real designer,
this is where a polished SVG diagram earns its keep.

### 3.5 Self-Host

This is the primary conversion path. A developer with a Linux box should
go from reading to running in 30 seconds.

---

**Run it on your hardware.**

One command. Any Linux machine with KVM.

```bash
curl -fsSL bhatti.sh/install | bash
```

This downloads pre-built binaries (~15MB total), a kernel, and a minimal
Ubuntu rootfs. No compilation, no Docker, no root during install. Takes
about 30 seconds.

Then:

```bash
sudo bhatti serve
```

Your server is running. Create an API key:

```bash
sudo bhatti user create --name alice
# → API key: bht_...
```

Give alice the key. She installs the CLI on her Mac:

```bash
curl -fsSL bhatti.sh/install | bash
bhatti setup   # paste the API key
bhatti create --name dev
bhatti shell dev
```

---

**Requirements:**

- Linux (x86_64 or ARM64) with `/dev/kvm`
- Root access for the daemon (Firecracker needs it)
- Tested on: Raspberry Pi 5, Hetzner AX-series, AWS Graviton bare metal

**The CLI works on macOS and Linux. No KVM needed for the CLI.**

---

One install URL for everything:
- `bhatti.sh/install` — unified installer (detects platform, prompts on Linux for CLI vs server)

The same script handles macOS CLI, Linux CLI, and Linux server installs.
Re-running updates an existing installation.

### 3.6 Try the Demo

The secondary conversion path for people who want to kick the tires
without setting up a server.

---

**Try it now. No signup.**

Enter your email. Get an API key. Start creating sandboxes.

```
┌──────────────────────────────────────────────┐
│  Email   [                    ]  [Get key →] │
└──────────────────────────────────────────────┘
```

This is a demo running on shared hardware in Europe (Hetzner, Germany).
Sandboxes are limited to 1 vCPU, 512MB RAM, and are destroyed after
24 hours of inactivity. For production use, self-host.

After submitting, the same page reveals:

```
Your API key: bht_abc123...          [Copy]

Get started:
  curl -fsSL https://bhatti.sh/cli | sh
  bhatti setup
  # Endpoint: https://demo.bhatti.sh
  # API key:  bht_abc123...
```

No page change. The form section transforms into the setup instructions.
One scroll position, zero navigation.

We'll only email you about bhatti launches. Nothing else. (Say this
next to the input.) See §5 for the server-side implementation.

### 3.7 Use Cases

Three panels, each with a title, a short paragraph, and a code snippet.

---

**AI Coding Agents**

Give every agent invocation its own isolated Linux VM. Install
dependencies, run tests, modify files — in an environment that can be
snapshotted and resumed. Streaming exec, server-side file truncation,
and process group kill are built for the agent workload.

```bash
bhatti create --name agent-run-42 --cpus 2 --memory 1024
bhatti exec agent-run-42 -- npm install
bhatti exec agent-run-42 -- npm test
bhatti file read agent-run-42 /workspace/results.json
bhatti destroy agent-run-42
```

---

**Browser Automation**

Boot a sandbox with headless Chromium and Playwright pre-installed.
Chromium starts automatically on CDP port 9222. Snapshot a logged-in
browser state, resume it 100 times — no re-login, no cookie management.

```bash
bhatti create --name scraper --image browser
bhatti exec scraper -- python3 -c "
from playwright.sync_api import sync_playwright
with sync_playwright() as p:
    browser = p.chromium.connect_over_cdp('http://localhost:9222')
    page = browser.new_page()
    page.goto('https://example.com')
    print(page.title())
"
```

---

**Docker Inside VMs**

Run Docker Engine inside a microVM. Full bridge networking, overlay
filesystem, `docker compose`. Snapshot your entire stack — Postgres,
Redis, your app — and resume it instantly with all containers running.

```bash
bhatti create --name ci --image docker --memory 2048
bhatti exec ci -- docker compose up -d
bhatti exec ci -- docker compose ps
# snapshot the entire running stack
bhatti stop ci
# ... later ...
bhatti start ci
# everything is back, containers running, data intact
```

---

### 3.8 Comparison

A table that's honest about tradeoffs. Don't pretend bhatti wins
everywhere — it doesn't. The honesty builds trust.

| | bhatti | Docker | E2B | Fly Machines | EC2 |
|---|---|---|---|---|---|
| **Isolation** | VM (own kernel) | Container (shared kernel) | VM | VM | VM |
| **Boot time** | ~3.5s | <1s | ~2s | ~1s | 30-60s |
| **Pause/resume** | ~400µs / ~50ms | SIGSTOP (no memory snapshot) | Filesystem only | Filesystem only | Not available |
| **Memory snapshots** | ✓ (processes survive) | ✗ | ✗ | ✗ | ✗ (AMI is disk only) |
| **Self-hostable** | ✓ | ✓ | ✗ | ✗ | ✗ |
| **Idle cost** | Zero (cold = freed) | Container stays allocated | Per-minute billing | Per-second billing | Per-hour billing |
| **Open source** | Apache 2.0 | Apache 2.0 | Proprietary | Proprietary | Proprietary |
| **Multi-tenant** | ✓ (per-user isolation) | Manual | ✓ | ✓ | IAM |
| **Hardware** | Any Linux + KVM | Any Linux | Their cloud | Their cloud | AWS |

**Where bhatti is not the right choice:**
- You need sub-second boot (Docker is faster to start)
- You need GPU access (Firecracker doesn't support GPU passthrough)
- You need Windows VMs (Firecracker is Linux-only)
- You don't have bare metal / KVM access (most cloud VMs don't expose nested KVM)

---

## 4. It's One Page

`bhatti.sh` is a single HTML file. No subdomains, no `/docs` renderer,
no dashboard route. One file, one domain.

| URL | What |
|-----|------|
| `bhatti.sh` | The page. Everything above lives here. |
| `bhatti.sh/install` | Serves `scripts/install.sh` (unified installer) |
| `bhatti.sh/install.sh` | Alias for `/install` |

That's it. Three routes. Two of them serve the install script.

**Docs** link to GitHub. The markdown files in `docs/` render fine on
GitHub. No need to build a docs site — the audience reads markdown.

**The demo API** runs on the Hetzner box at its own address (e.g.,
`demo.bhatti.sh` or just an IP with a port). The demo section of the
page points users at it. It's not part of the website — it's a bhatti
server that happens to be public.

**The dashboard** (`web/index.html`) is served by the bhatti daemon
itself at `/` — it already does this. Anyone who has the demo API
address can open it in a browser and get the terminal UI. No separate
hosting needed.

The hero CTA is:

```bash
curl -fsSL bhatti.sh/install | bash
```

This must work. Currently served as a 302 redirect to GitHub raw.
Could self-host the script for zero GitHub CDN dependency.

---

## 5. The Demo Server

A bhatti server running on the Hetzner box. Same binary, same config,
tighter user limits. The email form on `bhatti.sh` hits a `/register`
endpoint on this server.

**The flow:** User enters email on `bhatti.sh` → JS `POST`s to the demo
server's `/register` → server creates a user with tight limits → returns
the API key → page shows the key + CLI setup instructions. All on one
page, no redirects, no "check your inbox."

**`/register` logic:**

1. Validate email format (basic regex).
2. If email already registered → return existing key (idempotent).
3. `bhatti user create --max-sandboxes 3 --max-cpus 1 --max-memory 512`.
4. Store email → user mapping in SQLite.
5. Return the key immediately.

**No email verification.** The key appears on-page in under a second.
This is a demo — friction must be near zero. The email is for occasional
follow-up, not access control.

**Demo limits:** 1 vCPU, 512MB RAM, 2GB disk, 3 sandboxes, 24h
inactivity TTL. Clearly stated on the page.

**Abuse mitigation:** 3 registrations per IP per hour. Daily cron
destroys sandboxes idle >24h and users with no sandboxes for >7 days.
If abuse happens, add a CAPTCHA later. Don't pre-optimize.

**The dashboard comes free.** The bhatti daemon already serves
`web/index.html` at `/`. Anyone with the demo server URL can open it
in a browser and get the full terminal UI. No extra work.

---

## 6. Meta Tags

```html
<title>bhatti — self-hostable microVMs for AI agents</title>
<meta name="description" content="Open-source Firecracker microVM orchestrator. Real Linux VMs with memory snapshots, sub-millisecond resume, and three-tier thermal management. Self-host on any Linux machine with KVM.">
<meta property="og:title" content="bhatti — self-hostable microVMs for AI agents">
<meta property="og:description" content="Real Linux VMs. Memory snapshots. Sub-millisecond resume. Self-host on a Raspberry Pi or bare metal.">
<meta property="og:image" content="https://bhatti.sh/og.png">
<meta property="og:url" content="https://bhatti.sh">
<meta name="twitter:card" content="summary_large_image">
```

The OG image: logo/wordmark + tagline + a terminal snippet or the
benchmark table. Dark background matching the site. Static PNG, nothing
generated.

---

## 7. What to Build First

### Phase 1: Domain + install URLs

1. Register `bhatti.sh`.
2. Host a static site (Cloudflare Pages, Vercel, or nginx on the
   Hetzner box itself).
3. Serve the install scripts at `/install`, `/install-cli`, `/i`, `/cli`.
4. Verify: `curl -fsSL bhatti.sh/i | sh` works on a fresh Linux box.

### Phase 2: The page

One HTML file. Copy from §3. The email form is a `fetch()` to the demo
server — 20 lines of JS.

### Phase 3: Demo server

1. The Hetzner box already exists (agni-01 or a new one). Install bhatti.
2. Add the `/register` endpoint (small handler, see §5).
3. Point `demo.bhatti.sh` at it.
4. Wire the email form.
5. Add the cleanup cron.

### Phase 4: Logo

Commission a designer with the brief from §1.2. Replace the ⚒ emoji.

---

## 8. Copy Inventory

Every piece of copy the website needs, in one place. Use as-is or adapt.

### Tagline (hero)

> Self-hostable microVMs for AI agents.

### One-liner (meta, social, quick descriptions)

> Open-source Firecracker microVM orchestrator with memory snapshots and sub-millisecond resume.

### Elevator pitch (about section, README opening)

> Bhatti gives every coding agent its own Linux VM — full kernel, full
> filesystem, full process isolation — with sub-millisecond pause/resume
> and transparent resource management. Self-host it on a Raspberry Pi,
> a Hetzner box, or any Linux machine with KVM.

### The name (footer or about page, for the curious)

> **Bhatti** (भट्टी) is Hindi for *furnace* — the system that manages
> fire, provides the environment where work happens.
> **Lohar** (लोहार) means *blacksmith* — the guest agent that runs as
> PID 1 inside every microVM, the one doing the actual work.

### CTA labels

| Element | Copy |
|---------|------|
| Primary CTA (hero) | `curl -fsSL bhatti.sh/i \| sh` with "Copy" button |
| Secondary CTA (hero) | "Try the hosted demo →" |
| Demo form button | "Get API key" |
| Demo form label | "Enter your email — get an API key instantly." |
| Demo disclaimer | "Free demo on shared hardware in Europe. Sandboxes limited to 1 vCPU, 512MB, 24h TTL. For production, self-host." |
| Nav demo link | "Try Demo" |
| Post-demo self-host nudge | "Ready for production? Self-host in 30 seconds: `bhatti.sh/install`" |

### Section headers

| Section | Header | Subheader |
|---------|--------|-----------|
| What it is | "What you get" | (none — the three blocks speak for themselves) |
| How it works | "Architecture" | "Two binaries. One on the host, one in every VM." |
| Self-host | "Run it on your hardware" | "One command. Any Linux machine with KVM." |
| Demo | "Try it now" | "No signup. No credit card. Just an API key." |
| Use cases | "Built for" | (none) |
| Comparison | "How it compares" | (none) |
| Footer | (none) | "bhatti — open-source microVM orchestrator · Apache 2.0 · GitHub · Made by Sahil" |

---

## 9. Things to Explicitly Not Do

- **No pricing page.** Bhatti is open source. The demo is free. There's
  nothing to price. If managed hosting becomes a thing later, that's a
  separate product with a separate page.

- **No "enterprise" tier or "contact sales."** This is a solo open-source
  project. Pretending there's a sales team is dishonest and embarrassing.

- **No changelog/blog on the marketing site.** GitHub Releases is the
  changelog. A blog requires ongoing content. Don't create a ghost town.
  If there's something to announce, write it on a personal blog or X
  and link to it.

- **No testimonials or "trusted by" logos.** The project is new. Fake
  social proof is worse than no social proof. The benchmark table and
  the "try it in 30 seconds" flow are the credibility.

- **No JavaScript framework for the marketing page.** It's a static
  page with one form. HTML, CSS, and 20 lines of JS for the email
  submission and copy-to-clipboard. Ship a single HTML file.

- **No light mode (for now).** Dark mode is the default for developer
  tools. The existing dashboard is dark. The terminal is dark. The
  code blocks are dark. Ship one theme, ship it well.

- **No cookie banner.** Don't use analytics that require cookies. If
  you want analytics, use Plausible or Fathom (cookie-free) or nothing.
  The form collects an email with explicit consent; that's it.
