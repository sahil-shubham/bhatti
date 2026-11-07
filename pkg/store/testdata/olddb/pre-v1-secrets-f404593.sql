CREATE TABLE templates (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	engine TEXT NOT NULL DEFAULT 'docker',
	image TEXT NOT NULL,
	cpus REAL NOT NULL DEFAULT 1,
	memory_mb INTEGER NOT NULL DEFAULT 512,
	disk_size_mb INTEGER NOT NULL DEFAULT 0,
	userdata TEXT NOT NULL DEFAULT '',
	secrets_json TEXT NOT NULL DEFAULT '[]',
	labels_json TEXT NOT NULL DEFAULT '{}',
	mounts_json TEXT NOT NULL DEFAULT '[]',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE volumes (
	name TEXT PRIMARY KEY,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE sandbox_volumes (
	sandbox_id TEXT NOT NULL,
	volume_name TEXT NOT NULL,
	target TEXT NOT NULL,
	readonly INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (sandbox_id, volume_name)
);
CREATE TABLE sandboxes (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	template_id TEXT NOT NULL DEFAULT '',
	engine_id TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'unknown',
	ip TEXT NOT NULL DEFAULT '',
	engine_meta_json TEXT NOT NULL DEFAULT '{}',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	stopped_at DATETIME
, rootfs_path TEXT DEFAULT '', snap_mem_path TEXT DEFAULT '', snap_vm_path TEXT DEFAULT '', vsock_cid INTEGER DEFAULT 0, tap_device TEXT DEFAULT '', guest_ip TEXT DEFAULT '', guest_mac TEXT DEFAULT '', vcpu_count REAL DEFAULT 1, mem_size_mib INTEGER DEFAULT 512, socket_path TEXT DEFAULT '', vsock_path TEXT DEFAULT '', created_by TEXT NOT NULL DEFAULT '');
CREATE TABLE secrets (
	name TEXT PRIMARY KEY,
	path TEXT NOT NULL,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
, value_encrypted BLOB DEFAULT NULL, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP, user_id TEXT NOT NULL DEFAULT '');
CREATE TABLE users (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	api_key_hash TEXT NOT NULL UNIQUE,
	max_sandboxes INTEGER NOT NULL DEFAULT 5,
	max_cpus_per_sandbox INTEGER NOT NULL DEFAULT 4,
	max_memory_mb_per_sandbox INTEGER NOT NULL DEFAULT 4096,
	subnet_index INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX idx_sandboxes_user_name
		ON sandboxes(created_by, name) WHERE status != 'destroyed';
INSERT INTO "templates" (id, name, engine, image, cpus, memory_mb, disk_size_mb, userdata, secrets_json, labels_json, mounts_json, created_at) VALUES ('tpl-1', 'node', 'docker', 'minimal', 1.0, 512, 0, '', '[]', '{}', '[]', '2026-01-01 00:00:00');
INSERT INTO "volumes" (name, created_at) VALUES ('legacy-vol', '2026-01-01 00:00:00');
INSERT INTO "sandbox_volumes" (sandbox_id, volume_name, target, readonly) VALUES ('sb-0001', 'legacy-vol', '/mnt/legacy', 0);
INSERT INTO "sandboxes" (id, name, template_id, engine_id, status, ip, engine_meta_json, created_at, stopped_at, rootfs_path, snap_mem_path, snap_vm_path, vsock_cid, tap_device, guest_ip, guest_mac, vcpu_count, mem_size_mib, socket_path, vsock_path, created_by) VALUES ('sb-0001', 'dev-box', 'tpl-1', 'fc-0001', 'running', '10.0.1.2', '{}', '2026-01-01 00:00:00', NULL, '/data/sb-0001/rootfs.ext4', '', '', 3, 'tap-sb0001', '10.0.1.2', '', 1.0, 512, '', '', 'u-alice');
INSERT INTO "sandboxes" (id, name, template_id, engine_id, status, ip, engine_meta_json, created_at, stopped_at, rootfs_path, snap_mem_path, snap_vm_path, vsock_cid, tap_device, guest_ip, guest_mac, vcpu_count, mem_size_mib, socket_path, vsock_path, created_by) VALUES ('sb-0002', 'dev-box', '', '', 'destroyed', '', '{}', '2026-01-01 00:00:00', '2026-01-02 03:04:05', '', '', '', 0, '', '', '', 1.0, 512, '', '', 'u-alice');
INSERT INTO "sandboxes" (id, name, template_id, engine_id, status, ip, engine_meta_json, created_at, stopped_at, rootfs_path, snap_mem_path, snap_vm_path, vsock_cid, tap_device, guest_ip, guest_mac, vcpu_count, mem_size_mib, socket_path, vsock_path, created_by) VALUES ('sb-0003', 'dev-box', '', '', 'stopped', '', '{}', '2026-01-01 00:00:00', '2026-01-02 03:04:05', '', '', '', 0, '', '', '', 1.0, 512, '', '', 'u-bob');
INSERT INTO "secrets" (name, path, created_at, value_encrypted, updated_at, user_id) VALUES ('api-key', '/secrets/api-key', '2026-01-01 00:00:00', X'656E632D616C6963652D617069', '2026-01-01 00:00:00', 'u-alice');
INSERT INTO "secrets" (name, path, created_at, value_encrypted, updated_at, user_id) VALUES ('db-pass', '/secrets/db-pass', '2026-01-01 00:00:00', X'656E632D616C6963652D6462', NULL, 'u-alice');
INSERT INTO "users" (id, name, api_key_hash, max_sandboxes, max_cpus_per_sandbox, max_memory_mb_per_sandbox, subnet_index, created_at) VALUES ('u-alice', 'alice', 'hash-alice', 5, 4, 4096, 1, '2026-01-01 00:00:00');
INSERT INTO "users" (id, name, api_key_hash, max_sandboxes, max_cpus_per_sandbox, max_memory_mb_per_sandbox, subnet_index, created_at) VALUES ('u-bob', 'bob', 'hash-bob', 9, 4, 4096, 2, '2026-01-01 00:00:00');
