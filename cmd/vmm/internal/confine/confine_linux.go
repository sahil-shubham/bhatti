//go:build linux

package confine

// Why a C constructor, not Go: no_new_privs, credentials, capabilities,
// Landlock and seccomp belong to a thread, and a thread only inherits them
// from the one that creates it. Go's syscall.AllThreadsSyscall would set them
// on every thread, but it refuses to run in a cgo binary, and bhatti-vmm is
// one. Setting them from main.main, even locked to the main thread, is too
// late: the Go runtime has already started threads (sysmon, and Ms for the
// init goroutines), and any goroutine — a control-socket SAVE, say — can run
// on one of them; an unconfined thread in the same address space is also a
// way out for code that compromises a confined one. This constructor runs from
// the ELF init array: cgo binaries are externally linked, so libc runs it
// before handing control to the Go runtime's entry point, while the process
// still has exactly one thread. Every thread after — the runtime's and
// libkrun's vCPU and device threads — inherits what it sets.
//
// The helper drops its own identity (rather than the daemon starting it as the
// unprivileged user) so that exec, the dynamic loader and libkrun's load run
// as root, wherever the binary and libraries live, and identity, capabilities,
// no_new_privs, Landlock and seccomp are set in one place, in this order:
//
//   - capability bounding set reduced to what the policy keeps;
//   - supplementary groups, gid and uid set to the policy's (all three ids);
//   - capabilities reduced to what it keeps (normally none; a sandbox with a
//     virtio-fs mount keeps the few its file server needs, SECBIT_NO_SETUID_FIXUP
//     holding them across the per-request uid switches it makes);
//   - no_new_privs;
//   - Landlock: every filesystem right this kernel knows is denied except on
//     the policy's paths; TCP bind/connect denied (ABI >= 4); signals and
//     abstract unix sockets scoped to the helper itself (ABI >= 6);
//   - seccomp: the build architecture is enforced (tagged and legacy untagged
//     x32 syscalls killed on x86_64); dangerous kernel-control, module, mount,
//     keyring, tracing and namespace syscalls return EPERM; clone3 returns
//     ENOSYS so libc falls back to clone, which rejects namespace flags.
//     Checkpoint probes also deny socket operations, including on kernels
//     without Landlock's TCP restrictions.
//
// Any failure exits before the VM exists. A kernel without Landlock can only
// run helpers without virtio-fs mounts, with a warning; a mount requires
// inode-based filesystem confinement to prevent path-swap escapes.
// An allowlist needs libkrun's full syscall surface (KVM ioctls, eventfd,
// epoll, vectored I/O, guest RAM and checkpoints) profiled first; it is future
// work, not a reason to leave known-dangerous syscalls open.

/*
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <linux/audit.h>
#include <linux/capability.h>
#include <linux/filter.h>
#include <linux/sched.h>
#include <linux/seccomp.h>
#include <linux/securebits.h>

#ifndef PR_CAP_AMBIENT
#define PR_CAP_AMBIENT 47
#define PR_CAP_AMBIENT_CLEAR_ALL 4
#endif
#ifndef SYS_landlock_create_ruleset
#define SYS_landlock_create_ruleset 444
#define SYS_landlock_add_rule 445
#define SYS_landlock_restrict_self 446
#endif
#ifndef SYS_openat2
#define SYS_openat2 437
#endif
#define BV_RESOLVE_NO_MAGICLINKS 0x02u
#define BV_RESOLVE_NO_SYMLINKS 0x04u

struct bv_open_how {
	uint64_t flags;
	uint64_t mode;
	uint64_t resolve;
};

// Landlock's uapi (linux/landlock.h), spelled out so older headers build.
#define BV_LL_VERSION 1u
#define BV_LL_RULE_PATH_BENEATH 1
#define BV_FS_EXECUTE (1ull << 0)
#define BV_FS_WRITE_FILE (1ull << 1)
#define BV_FS_READ_FILE (1ull << 2)
#define BV_FS_ABI1 ((1ull << 13) - 1)
#define BV_FS_REFER (1ull << 13)
#define BV_FS_TRUNCATE (1ull << 14)
#define BV_FS_IOCTL_DEV (1ull << 15)
#define BV_FS_FILE (BV_FS_EXECUTE | BV_FS_WRITE_FILE | BV_FS_READ_FILE | BV_FS_TRUNCATE | BV_FS_IOCTL_DEV)
#define BV_NET_BIND_TCP (1ull << 0)
#define BV_NET_CONNECT_TCP (1ull << 1)
#define BV_SCOPE_ABSTRACT_UNIX_SOCKET (1ull << 0)
#define BV_SCOPE_SIGNAL (1ull << 1)

struct bv_ruleset_attr {
	uint64_t handled_access_fs;
	uint64_t handled_access_net;
	uint64_t scoped;
};

struct bv_path_beneath_attr {
	uint64_t allowed_access;
	int32_t parent_fd;
} __attribute__((packed));

#define BV_MAX_GROUPS 32
#define BV_MAX_RULES 256
#define BV_MAX_POLICY (1 << 20)

static void bv_die(const char *what, const char *arg, int err) {
	fprintf(stderr, "vmm: confine: fatal: %s%s%s: %s\n", what, arg ? " " : "", arg ? arg : "",
		err ? strerror(err) : "refused");
	_exit(1);
}

// bv_read_policy returns the policy file's bytes, NUL-terminated past the end.
static char *bv_read_policy(const char *path, size_t *len) {
	int fd = open(path, O_RDONLY | O_CLOEXEC);
	if (fd < 0)
		bv_die("open policy", path, errno);
	struct stat st;
	if (fstat(fd, &st) != 0)
		bv_die("stat policy", path, errno);
	if (st.st_size < 0 || st.st_size > BV_MAX_POLICY)
		bv_die("policy size", path, EFBIG);
	char *buf = malloc((size_t)st.st_size + 1);
	if (!buf)
		bv_die("policy", path, ENOMEM);
	size_t n = 0;
	while (n < (size_t)st.st_size) {
		ssize_t r = read(fd, buf + n, (size_t)st.st_size - n);
		if (r < 0 && errno == EINTR)
			continue;
		if (r <= 0)
			bv_die("read policy", path, r < 0 ? errno : EIO);
		n += (size_t)r;
	}
	close(fd);
	buf[n] = 0;
	*len = n;
	return buf;
}

static unsigned long long bv_number(const char *s, int base, const char *what) {
	char *end;
	errno = 0;
	unsigned long long v = strtoull(s, &end, base);
	if (errno || end == s || (*end && *end != ' '))
		bv_die("policy: bad number for", what, EINVAL);
	return v;
}

// On kernels without openat2, pin each component with an O_PATH directory fd
// before resolving the next. O_NOFOLLOW at the final component alone would
// still let a changed ancestor redirect a mount outside its authorized root.
static int bv_open_walk(const char *path) {
	if (*path != '/') {
		errno = EINVAL;
		return -1;
	}
	int dir = open("/", O_PATH | O_DIRECTORY | O_CLOEXEC);
	if (dir < 0 || path[1] == '\0')
		return dir;
	char *parts = strdup(path + 1);
	if (!parts) {
		close(dir);
		errno = ENOMEM;
		return -1;
	}
	for (char *part = parts; ; ) {
		char *next = strchr(part, '/');
		if (next)
			*next = '\0';
		if (!*part || !strcmp(part, ".") || !strcmp(part, "..")) {
			errno = EINVAL;
			goto fail;
		}
		int fd = openat(dir, part, O_PATH | O_NOFOLLOW | O_CLOEXEC |
			(next ? O_DIRECTORY : 0));
		if (fd < 0)
			goto fail;
		struct stat st;
		if (fstat(fd, &st) != 0) {
			int err = errno;
			close(fd);
			errno = err;
			goto fail;
		}
		if (S_ISLNK(st.st_mode)) {
			close(fd);
			errno = ELOOP;
			goto fail;
		}
		close(dir);
		dir = fd;
		if (!next)
			break;
		part = next + 1;
	}
	free(parts);
	return dir;
fail:
	{
		int err = errno;
		free(parts);
		close(dir);
		errno = err;
		return -1;
	}
}

static int bv_open_rule(const char *path) {
	if (*path != '/') {
		errno = EINVAL;
		return -1;
	}
	struct bv_open_how how = {
		.flags = O_PATH | O_CLOEXEC,
		.resolve = BV_RESOLVE_NO_SYMLINKS | BV_RESOLVE_NO_MAGICLINKS,
	};
	int fd = (int)syscall(SYS_openat2, AT_FDCWD, path, &how, sizeof how);
	if (fd < 0 && errno == ENOSYS)
		return bv_open_walk(path);
	return fd;
}


// Each installed filter inherits no_new_privs. TSYNC is unnecessary: this
// constructor runs before the Go runtime or libkrun creates any threads.
#if defined(__x86_64__)
#define BV_AUDIT_ARCH AUDIT_ARCH_X86_64
#ifndef __X32_SYSCALL_BIT
#define __X32_SYSCALL_BIT 0x40000000
#endif
#elif defined(__aarch64__)
#define BV_AUDIT_ARCH AUDIT_ARCH_AARCH64
#else
#error "bhatti-vmm seccomp supports x86_64 and aarch64"
#endif

#ifndef CLONE_NEWTIME
#define CLONE_NEWTIME 0x00000080
#endif
#define BV_NS_FLAGS (CLONE_NEWTIME | CLONE_NEWNS | CLONE_NEWCGROUP | CLONE_NEWUTS | \
	CLONE_NEWIPC | CLONE_NEWUSER | CLONE_NEWPID | CLONE_NEWNET)
#define BV_DENY(name) \
	BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, SYS_##name, 0, 1), \
	BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM)

static void bv_install_filter(struct sock_filter *filter, size_t count) {
	struct sock_fprog prog = {(unsigned short)count, filter};
	if (prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, &prog) != 0)
		bv_die("seccomp", NULL, errno);
}

static void bv_seccomp(int no_network) {
	static struct sock_filter filter[] = {
		BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, arch)),
		BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, BV_AUDIT_ARCH, 1, 0),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_KILL_PROCESS),
		BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
#if defined(__x86_64__)
		BPF_JUMP(BPF_JMP | BPF_JSET | BPF_K, __X32_SYSCALL_BIT, 0, 1),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_KILL_PROCESS),
		// Linux before 5.4 dispatched 512..547 as x32 even without the
		// x32 tag (521 was ptrace). Neither Go's amd64 runtime nor libkrun
		// uses the x32 ABI; kill rather than let these bypass BV_DENY.
		BPF_JUMP(BPF_JMP | BPF_JGE | BPF_K, 512, 0, 2),
		BPF_JUMP(BPF_JMP | BPF_JGE | BPF_K, 548, 1, 0),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_KILL_PROCESS),
#endif
		BV_DENY(kexec_load),
		BV_DENY(kexec_file_load),
		BV_DENY(init_module),
		BV_DENY(finit_module),
		BV_DENY(delete_module),
		BV_DENY(bpf),
		BV_DENY(perf_event_open),
		BV_DENY(ptrace),
		BV_DENY(process_vm_readv),
		BV_DENY(process_vm_writev),
		BV_DENY(keyctl),
		BV_DENY(add_key),
		BV_DENY(request_key),
		BV_DENY(mount),
		BV_DENY(umount2),
		BV_DENY(pivot_root),
		BV_DENY(move_mount),
		BV_DENY(open_tree),
		BV_DENY(fsopen),
		BV_DENY(fsconfig),
		BV_DENY(fsmount),
		BV_DENY(fspick),
		BV_DENY(swapon),
		BV_DENY(swapoff),
		BV_DENY(reboot),
		BV_DENY(setns),
		BV_DENY(unshare),
		BV_DENY(userfaultfd),
		BV_DENY(open_by_handle_at),
		BV_DENY(name_to_handle_at),
		BV_DENY(kcmp),
		BV_DENY(acct),
		BV_DENY(quotactl),
#if defined(__x86_64__)
		BV_DENY(iopl),
		BV_DENY(ioperm),
#endif
		BV_DENY(personality),
		// Rust and glibc both fall back to clone when clone3 reports ENOSYS;
		// EPERM instead would break thread creation after this constructor.
		BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, SYS_clone3, 0, 1),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | ENOSYS),
		BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, SYS_clone, 0, 3),
		BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, args[0])),
		BPF_JUMP(BPF_JMP | BPF_JSET | BPF_K, BV_NS_FLAGS, 0, 1),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ERRNO | EPERM),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
	};
	bv_install_filter(filter, sizeof(filter) / sizeof(filter[0]));

	if (no_network) {
		// Unlike Landlock's ABI 4 TCP hooks, this also blocks UDP and
		// socket operations on inherited descriptors. Go's exec child
		// inherits only stdio pipes.
		static struct sock_filter sockets[] = {
			BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
			BV_DENY(socket),
			BV_DENY(socketpair),
			BV_DENY(connect),
			BV_DENY(bind),
			BV_DENY(listen),
			BV_DENY(accept),
			BV_DENY(accept4),
			BV_DENY(sendto),
			BV_DENY(sendmsg),
			BV_DENY(sendmmsg),
			BV_DENY(recvfrom),
			BV_DENY(recvmsg),
			BV_DENY(recvmmsg),
			BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
		};
		bv_install_filter(sockets, sizeof(sockets) / sizeof(sockets[0]));
	}
}

__attribute__((constructor, used)) static void bv_confine(void) {
	const char *path = getenv("BHATTI_VMM_CONFINE");
	if (!path || !*path)
		return;

	size_t len;
	char *policy = bv_read_policy(path, &len);
	unsigned long long uid = 0, gid = 0, keep = 0;
	gid_t groups[BV_MAX_GROUPS];
	int ngroups = 0;
	struct { uint64_t access; const char *path; int mount; } rules[BV_MAX_RULES];
	int nrules = 0, nmounts = 0;
	int no_network = 0;

	// Records are "<key> <value>", each NUL-terminated.
	for (char *rec = policy; rec < policy + len; rec += strlen(rec) + 1) {
		if (!strncmp(rec, "uid ", 4)) {
			uid = bv_number(rec + 4, 10, "uid");
		} else if (!strncmp(rec, "gid ", 4)) {
			gid = bv_number(rec + 4, 10, "gid");
		} else if (!strncmp(rec, "group ", 6)) {
			if (ngroups == BV_MAX_GROUPS)
				bv_die("policy: too many groups", NULL, E2BIG);
			groups[ngroups++] = (gid_t)bv_number(rec + 6, 10, "group");
		} else if (!strncmp(rec, "cap ", 4)) {
			unsigned long long c = bv_number(rec + 4, 10, "cap");
			if (c > 63)
				bv_die("policy: bad cap", rec + 4, EINVAL);
			keep |= 1ull << c;
		} else if (!strcmp(rec, "network deny")) {
			no_network = 1;
		} else if (!strncmp(rec, "path ", 5) || !strncmp(rec, "mount ", 6)) {
			int mount = !strncmp(rec, "mount ", 6);
			char *value = rec + (mount ? 6 : 5);
			char *sp = strchr(value, ' ');
			if (!sp || !sp[1])
				bv_die("policy: bad path record", rec, EINVAL);
			if (nrules == BV_MAX_RULES)
				bv_die("policy: too many paths", NULL, E2BIG);
			rules[nrules].access = bv_number(value, 16, "access");
			rules[nrules].path = sp + 1;
			rules[nrules].mount = mount;
			nmounts += mount;
			nrules++;
		} else if (*rec) {
			bv_die("policy: unknown record", rec, EINVAL);
		}
	}

	if (uid || gid) {
		// A helper started as root that kept root would be the case this
		// exists to prevent: refuse rather than run.
		if (geteuid() != 0)
			bv_die("an identity to run as needs starting as root", NULL, EPERM);
		if (!uid || !gid)
			bv_die("policy names half an identity", NULL, EINVAL);
		if (keep && prctl(PR_SET_SECUREBITS, SECBIT_NO_SETUID_FIXUP | SECBIT_NO_SETUID_FIXUP_LOCKED |
				SECBIT_NOROOT | SECBIT_NOROOT_LOCKED, 0, 0, 0) != 0)
			bv_die("securebits", NULL, errno);
		for (int c = 0; c < 64; c++) {
			if (keep & (1ull << c))
				continue;
			if (prctl(PR_CAPBSET_DROP, c, 0, 0, 0) != 0) {
				if (errno == EINVAL)
					break; // past the kernel's last capability
				bv_die("drop bounding capability", NULL, errno);
			}
		}
		if (setgroups(ngroups, groups) != 0)
			bv_die("setgroups", NULL, errno);
		if (setresgid((gid_t)gid, (gid_t)gid, (gid_t)gid) != 0)
			bv_die("setresgid", NULL, errno);
		if (setresuid((uid_t)uid, (uid_t)uid, (uid_t)uid) != 0)
			bv_die("setresuid", NULL, errno);
		struct __user_cap_header_struct hdr = {_LINUX_CAPABILITY_VERSION_3, 0};
		struct __user_cap_data_struct caps[2];
		memset(caps, 0, sizeof caps);
		caps[0].permitted = caps[0].effective = (uint32_t)keep;
		caps[1].permitted = caps[1].effective = (uint32_t)(keep >> 32);
		if (syscall(SYS_capset, &hdr, caps) != 0)
			bv_die("capset", NULL, errno);
		if (prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0) != 0 && errno != EINVAL)
			bv_die("clear ambient capabilities", NULL, errno);
		if (!(keep & (1ull << CAP_SETUID)) && setresuid(0, 0, 0) == 0)
			bv_die("root could be regained", NULL, 0);
	} else if (geteuid() == 0) {
		bv_die("started as root without an identity to run as", NULL, EPERM);
	}

	if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0)
		bv_die("no_new_privs", NULL, errno);

	long abi = syscall(SYS_landlock_create_ruleset, NULL, 0, BV_LL_VERSION);
	if (abi < 0) {
		if (errno != ENOSYS && errno != EOPNOTSUPP)
			bv_die("landlock", NULL, errno);
		if (nmounts)
			bv_die("Landlock required for virtio-fs mounts", NULL, ENOTSUP);
		fprintf(stderr, "vmm: confine: Landlock unavailable on this kernel (%s); running without filesystem confinement\n",
			strerror(errno));
		goto install_seccomp;
	}
	struct bv_ruleset_attr attr = {BV_FS_ABI1, 0, 0};
	if (abi >= 2)
		attr.handled_access_fs |= BV_FS_REFER;
	if (abi >= 3)
		attr.handled_access_fs |= BV_FS_TRUNCATE;
	if (abi >= 4)
		attr.handled_access_net = BV_NET_BIND_TCP | BV_NET_CONNECT_TCP;
	if (abi >= 5)
		attr.handled_access_fs |= BV_FS_IOCTL_DEV;
	if (abi >= 6)
		attr.scoped = BV_SCOPE_ABSTRACT_UNIX_SOCKET | BV_SCOPE_SIGNAL;
	// The kernel takes the whole struct as long as what it doesn't know is zero.
	int ruleset = (int)syscall(SYS_landlock_create_ruleset, &attr, sizeof attr, 0);
	if (ruleset < 0)
		bv_die("landlock ruleset", NULL, errno);
	for (int i = 0; i < nrules; i++) {
		int fd = bv_open_rule(rules[i].path);
		if (fd < 0)
			bv_die("open", rules[i].path, errno);
		struct stat st;
		if (fstat(fd, &st) != 0)
			bv_die("stat", rules[i].path, errno);
		if (rules[i].mount && !S_ISDIR(st.st_mode))
			bv_die("mount is not a directory", rules[i].path, ENOTDIR);
		uint64_t allowed = rules[i].access & attr.handled_access_fs;
		if (!S_ISDIR(st.st_mode))
			allowed &= BV_FS_FILE;
		if (allowed) {
			struct bv_path_beneath_attr pb = {allowed, fd};
			if (syscall(SYS_landlock_add_rule, ruleset, BV_LL_RULE_PATH_BENEATH, &pb, 0) != 0)
				bv_die("landlock rule", rules[i].path, errno);
		}
		close(fd);
	}
	if (syscall(SYS_landlock_restrict_self, ruleset, 0) != 0)
		bv_die("landlock restrict", NULL, errno);
	close(ruleset);
install_seccomp:
	bv_seccomp(no_network);
	fprintf(stderr, "vmm: confined: uid=%u gid=%u caps=%#llx landlock abi %ld, %d paths, seccomp\n",
		(unsigned)getuid(), (unsigned)getgid(), keep, abi, nrules);
	free(policy);
}
*/
import "C"
