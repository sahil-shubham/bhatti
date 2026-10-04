//go:build linux

package confine

// Why a C constructor, not Go: no_new_privs, credentials, capabilities and
// Landlock domains all belong to a thread, and a thread only inherits them
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
// no_new_privs and Landlock are set in one place, in this order:
//
//   - capability bounding set reduced to what the policy keeps;
//   - supplementary groups, gid and uid set to the policy's (all three ids);
//   - capabilities reduced to what it keeps (normally none; a sandbox with a
//     virtio-fs mount keeps the few its file server needs, SECBIT_NO_SETUID_FIXUP
//     holding them across the per-request uid switches it makes);
//   - no_new_privs;
//   - Landlock: every filesystem right this kernel knows is denied except on
//     the policy's paths; TCP bind/connect denied (ABI >= 4); signals and
//     abstract unix sockets scoped to the helper itself (ABI >= 6).
//
// Any failure exits before the VM exists. A kernel without Landlock only loses
// that layer, with a warning, as bhatti-netd does.
//
// seccomp is not applied: libkrun's syscall surface (KVM ioctls, eventfd,
// epoll, vectored I/O, mmap of guest RAM and checkpoints) hasn't been profiled,
// and an enforcing filter that misses one kills a guest at a random moment.
// The next layer is an allowlist built from that profile, logged before it is
// enforced.

/*
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <linux/capability.h>
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
	fprintf(stderr, "vmm: confine: %s%s%s: %s\n", what, arg ? " " : "", arg ? arg : "",
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

__attribute__((constructor, used)) static void bv_confine(void) {
	const char *path = getenv("BHATTI_VMM_CONFINE");
	if (!path || !*path)
		return;

	size_t len;
	char *policy = bv_read_policy(path, &len);
	unsigned long long uid = 0, gid = 0, keep = 0;
	gid_t groups[BV_MAX_GROUPS];
	int ngroups = 0;
	struct { uint64_t access; const char *path; } rules[BV_MAX_RULES];
	int nrules = 0;

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
		} else if (!strncmp(rec, "path ", 5)) {
			char *sp = strchr(rec + 5, ' ');
			if (!sp || !sp[1])
				bv_die("policy: bad path record", rec, EINVAL);
			if (nrules == BV_MAX_RULES)
				bv_die("policy: too many paths", NULL, E2BIG);
			rules[nrules].access = bv_number(rec + 5, 16, "access");
			rules[nrules].path = sp + 1;
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
		fprintf(stderr, "vmm: confine: Landlock unavailable on this kernel (%s); running without filesystem confinement\n",
			strerror(errno));
		free(policy);
		return;
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
		int fd = open(rules[i].path, O_PATH | O_CLOEXEC);
		if (fd < 0)
			bv_die("open", rules[i].path, errno);
		struct stat st;
		if (fstat(fd, &st) != 0)
			bv_die("stat", rules[i].path, errno);
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
	fprintf(stderr, "vmm: confined: uid=%u gid=%u caps=%#llx landlock abi %ld, %d paths\n",
		(unsigned)getuid(), (unsigned)getgid(), keep, abi, nrules);
	free(policy);
}
*/
import "C"
