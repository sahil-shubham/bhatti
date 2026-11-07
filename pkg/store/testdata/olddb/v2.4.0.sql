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
, rootfs_path TEXT DEFAULT '', snap_mem_path TEXT DEFAULT '', snap_vm_path TEXT DEFAULT '', vsock_cid INTEGER DEFAULT 0, tap_device TEXT DEFAULT '', guest_ip TEXT DEFAULT '', guest_mac TEXT DEFAULT '', vcpu_count REAL DEFAULT 1, mem_size_mib INTEGER DEFAULT 512, socket_path TEXT DEFAULT '', vsock_path TEXT DEFAULT '', created_by TEXT NOT NULL DEFAULT '', agent_token TEXT DEFAULT '', has_base_snapshot INTEGER DEFAULT 0, keep_hot INTEGER NOT NULL DEFAULT 0, fc_path_origin TEXT DEFAULT '', shell_token_hash TEXT DEFAULT '', cpus REAL NOT NULL DEFAULT 1, memory_mb INTEGER NOT NULL DEFAULT 1024, disk_size_mb INTEGER NOT NULL DEFAULT 0, image TEXT NOT NULL DEFAULT 'minimal', labels TEXT NOT NULL DEFAULT '{}', net_policy TEXT NOT NULL DEFAULT '', has_init INTEGER NOT NULL DEFAULT 0);
CREATE TABLE users (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	api_key_hash TEXT NOT NULL UNIQUE,
	max_sandboxes INTEGER NOT NULL DEFAULT 5,
	max_cpus_per_sandbox INTEGER NOT NULL DEFAULT 4,
	max_memory_mb_per_sandbox INTEGER NOT NULL DEFAULT 4096,
	subnet_index INTEGER NOT NULL DEFAULT 0,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
, max_volume_storage_mb INTEGER NOT NULL DEFAULT 20480, max_images INTEGER NOT NULL DEFAULT 10, max_snapshots INTEGER NOT NULL DEFAULT 5);
CREATE TABLE volumes_v2 (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL,
		size_mb INTEGER NOT NULL,
		file_path TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'ready',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(user_id, name)
	);
CREATE TABLE volume_attachments (
		volume_id TEXT NOT NULL,
		sandbox_id TEXT NOT NULL,
		mount TEXT NOT NULL,
		read_only INTEGER NOT NULL DEFAULT 0,
		attached_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (volume_id, sandbox_id)
	);
CREATE TABLE images (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT '',
		file_path TEXT NOT NULL,
		size_mb INTEGER NOT NULL DEFAULT 0,
		oci_digest TEXT NOT NULL DEFAULT '',
		oci_config_json TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(user_id, name)
	);
CREATE TABLE snapshots (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		name TEXT NOT NULL,
		source_sandbox TEXT NOT NULL,
		mem_path TEXT NOT NULL,
		vm_path TEXT NOT NULL,
		rootfs_path TEXT NOT NULL,
		config_path TEXT NOT NULL,
		manifest_json TEXT NOT NULL,
		size_mb INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(user_id, name)
	);
CREATE TABLE tasks (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		type TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'running',
		progress TEXT NOT NULL DEFAULT '',
		result_json TEXT NOT NULL DEFAULT '{}',
		error TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		completed_at DATETIME
	);
CREATE INDEX idx_tasks_created_at ON tasks(created_at);
CREATE TABLE publish_rules (
		id TEXT PRIMARY KEY,
		sandbox_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		port INTEGER NOT NULL,
		alias TEXT NOT NULL UNIQUE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(sandbox_id, port)
	);
CREATE INDEX idx_publish_rules_sandbox ON publish_rules(sandbox_id);
CREATE TABLE volume_backups (
		id TEXT PRIMARY KEY,
		volume_name TEXT NOT NULL,
		user_id TEXT NOT NULL,
		s3_key TEXT NOT NULL,
		size_bytes INTEGER NOT NULL,
		sha256 TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
CREATE INDEX idx_volume_backups_name ON volume_backups(user_id, volume_name, created_at DESC);
CREATE UNIQUE INDEX idx_sandboxes_user_name
		ON sandboxes(created_by, name) WHERE status != 'destroyed';
CREATE TABLE "secrets" (
		user_id TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL,
		path TEXT NOT NULL DEFAULT '',
		value_encrypted BLOB DEFAULT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (user_id, name)
	);
CREATE TABLE image_shares (
		image_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		PRIMARY KEY (image_id, user_id)
	);
CREATE TABLE events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		type TEXT NOT NULL,
		user_id TEXT NOT NULL DEFAULT '',
		sandbox_id TEXT NOT NULL DEFAULT '',
		meta TEXT NOT NULL DEFAULT '{}'
	);
CREATE INDEX idx_events_ts ON events(ts);
CREATE INDEX idx_events_type ON events(type, ts);
CREATE INDEX idx_events_user ON events(user_id, ts);
CREATE TABLE metrics_snapshots (
		ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		api_requests INTEGER NOT NULL DEFAULT 0,
		api_errors INTEGER NOT NULL DEFAULT 0,
		api_auth_failures INTEGER NOT NULL DEFAULT 0,
		api_rate_limited INTEGER NOT NULL DEFAULT 0,
		proxy_requests INTEGER NOT NULL DEFAULT 0,
		proxy_errors INTEGER NOT NULL DEFAULT 0,
		proxy_cold_wakes INTEGER NOT NULL DEFAULT 0,
		proxy_rate_limited INTEGER NOT NULL DEFAULT 0,
		events_dropped INTEGER NOT NULL DEFAULT 0,
		sandboxes_total INTEGER NOT NULL DEFAULT 0,
		sandboxes_hot INTEGER NOT NULL DEFAULT 0,
		sandboxes_warm INTEGER NOT NULL DEFAULT 0,
		sandboxes_cold INTEGER NOT NULL DEFAULT 0,
		users_total INTEGER NOT NULL DEFAULT 0,
		users_active INTEGER NOT NULL DEFAULT 0,
		websockets_active INTEGER NOT NULL DEFAULT 0,
		host_load_1m REAL NOT NULL DEFAULT 0,
		host_mem_total_mb INTEGER NOT NULL DEFAULT 0,
		host_mem_avail_mb INTEGER NOT NULL DEFAULT 0
	);
CREATE INDEX idx_ms_ts ON metrics_snapshots(ts);
CREATE TABLE secret_grants (
		id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		secret_name TEXT NOT NULL,
		sandbox_id TEXT NOT NULL,
		hosts_json TEXT NOT NULL DEFAULT '[]',
		placeholder TEXT NOT NULL UNIQUE,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		expires_at DATETIME,
		revoked_at DATETIME
	);
CREATE INDEX idx_secret_grants_sandbox ON secret_grants(sandbox_id);
CREATE INDEX idx_secret_grants_user_created ON secret_grants(user_id, created_at);
CREATE TABLE sandbox_cas (
		sandbox_id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		cert_pem TEXT NOT NULL,
		key_enc BLOB NOT NULL,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	);
INSERT INTO "templates" (id, name, engine, image, cpus, memory_mb, disk_size_mb, userdata, secrets_json, labels_json, mounts_json, created_at) VALUES ('tpl-1', 'node', 'docker', 'minimal', 1.0, 512, 0, '', '[]', '{}', '[]', '2026-01-01 00:00:00');
INSERT INTO "volumes" (name, created_at) VALUES ('legacy-vol', '2026-01-01 00:00:00');
INSERT INTO "sandbox_volumes" (sandbox_id, volume_name, target, readonly) VALUES ('sb-0001', 'legacy-vol', '/mnt/legacy', 0);
INSERT INTO "sandboxes" (id, name, template_id, engine_id, status, ip, engine_meta_json, created_at, stopped_at, rootfs_path, snap_mem_path, snap_vm_path, vsock_cid, tap_device, guest_ip, guest_mac, vcpu_count, mem_size_mib, socket_path, vsock_path, created_by, agent_token, has_base_snapshot, keep_hot, fc_path_origin, shell_token_hash, cpus, memory_mb, disk_size_mb, image, labels, net_policy, has_init) VALUES ('sb-0001', 'dev-box', 'tpl-1', 'fc-0001', 'running', '10.0.1.2', '{}', '2026-01-01 00:00:00', NULL, '/data/sb-0001/rootfs.ext4', '', '', 3, 'tap-sb0001', '10.0.1.2', '', 1.0, 512, '', '', 'u-alice', 'tok-0001', 0, 1, '', '', 2.0, 2048, 4096, 'minimal', '{"env":"dev"}', 'open', 1);
INSERT INTO "sandboxes" (id, name, template_id, engine_id, status, ip, engine_meta_json, created_at, stopped_at, rootfs_path, snap_mem_path, snap_vm_path, vsock_cid, tap_device, guest_ip, guest_mac, vcpu_count, mem_size_mib, socket_path, vsock_path, created_by, agent_token, has_base_snapshot, keep_hot, fc_path_origin, shell_token_hash, cpus, memory_mb, disk_size_mb, image, labels, net_policy, has_init) VALUES ('sb-0002', 'dev-box', '', '', 'destroyed', '', '{}', '2026-01-01 00:00:00', '2026-01-02 03:04:05', '', '', '', 0, '', '', '', 1.0, 512, '', '', 'u-alice', '', 0, 0, '', '', 1.0, 1024, 0, 'minimal', '{}', '', 0);
INSERT INTO "sandboxes" (id, name, template_id, engine_id, status, ip, engine_meta_json, created_at, stopped_at, rootfs_path, snap_mem_path, snap_vm_path, vsock_cid, tap_device, guest_ip, guest_mac, vcpu_count, mem_size_mib, socket_path, vsock_path, created_by, agent_token, has_base_snapshot, keep_hot, fc_path_origin, shell_token_hash, cpus, memory_mb, disk_size_mb, image, labels, net_policy, has_init) VALUES ('sb-0003', 'dev-box', '', '', 'stopped', '', '{}', '2026-01-01 00:00:00', '2026-01-02 03:04:05', '', '', '', 0, '', '', '', 1.0, 512, '', '', 'u-bob', '', 0, 0, '', '', 1.0, 1024, 0, 'minimal', '{"team":"b"}', '', 0);
INSERT INTO "users" (id, name, api_key_hash, max_sandboxes, max_cpus_per_sandbox, max_memory_mb_per_sandbox, subnet_index, created_at, max_volume_storage_mb, max_images, max_snapshots) VALUES ('u-alice', 'alice', 'hash-alice', 5, 4, 4096, 1, '2026-01-01 00:00:00', 20480, 10, 5);
INSERT INTO "users" (id, name, api_key_hash, max_sandboxes, max_cpus_per_sandbox, max_memory_mb_per_sandbox, subnet_index, created_at, max_volume_storage_mb, max_images, max_snapshots) VALUES ('u-bob', 'bob', 'hash-bob', 9, 4, 4096, 2, '2026-01-01 00:00:00', 20480, 10, 5);
INSERT INTO "volumes_v2" (id, user_id, name, size_mb, file_path, status, created_at) VALUES ('vol-1', 'u-alice', 'data', 1024, '/data/vol-1.ext4', 'ready', '2026-01-01 00:00:00');
INSERT INTO "volumes_v2" (id, user_id, name, size_mb, file_path, status, created_at) VALUES ('vol-2', 'u-bob', 'data', 512, '/data/vol-2.ext4', 'ready', '2026-01-01 00:00:00');
INSERT INTO "volume_attachments" (volume_id, sandbox_id, mount, read_only, attached_at) VALUES ('vol-1', 'sb-0001', '/mnt/data', 0, '2026-01-01 00:00:00');
INSERT INTO "images" (id, user_id, name, source, file_path, size_mb, oci_digest, oci_config_json, created_at) VALUES ('img-1', 'u-alice', 'my-image', 'docker:alpine', '/data/img-1.ext4', 300, '', '{}', '2026-01-01 00:00:00');
INSERT INTO "snapshots" (id, user_id, name, source_sandbox, mem_path, vm_path, rootfs_path, config_path, manifest_json, size_mb, created_at) VALUES ('snap-1', 'u-alice', 'before-upgrade', 'sb-0001', '/data/s1/mem', '/data/s1/vm', '/data/s1/rootfs', '/data/s1/cfg', '{}', 128, '2026-01-01 00:00:00');
INSERT INTO "tasks" (id, user_id, type, status, progress, result_json, error, created_at, completed_at) VALUES ('task-1', 'u-alice', 'snapshot', 'done', '100%', '{}', '', '2026-01-01 00:00:00', '2026-01-02 03:04:05');
INSERT INTO "publish_rules" (id, sandbox_id, user_id, port, alias, created_at) VALUES ('pub-1', 'sb-0001', 'u-alice', 8080, 'dev-box-app', '2026-01-01 00:00:00');
INSERT INTO "volume_backups" (id, volume_name, user_id, s3_key, size_bytes, sha256, created_at) VALUES ('bk-1', 'data', 'u-alice', 'backups/data/1', 1048576, 'abc123', '2026-01-01 00:00:00');
INSERT INTO "secrets" (user_id, name, path, value_encrypted, created_at, updated_at) VALUES ('u-alice', 'api-key', '', X'656E632D616C6963652D617069', '2026-01-01 00:00:00', '2026-01-02 03:04:05');
INSERT INTO "secrets" (user_id, name, path, value_encrypted, created_at, updated_at) VALUES ('u-bob', 'api-key', '', X'656E632D626F622D617069', '2026-01-01 00:00:00', '2026-01-02 03:04:05');
INSERT INTO "secrets" (user_id, name, path, value_encrypted, created_at, updated_at) VALUES ('u-alice', 'db-pass', '', X'656E632D616C6963652D6462', '2026-01-01 00:00:00', '2026-01-01 00:00:00');
INSERT INTO "image_shares" (image_id, user_id) VALUES ('img-1', 'u-bob');
INSERT INTO "events" (id, ts, type, user_id, sandbox_id, meta) VALUES (1, '2026-01-01T00:00:00.000Z', 'sandbox.create', 'u-alice', 'sb-0001', '{"name":"dev-box"}');
INSERT INTO "events" (id, ts, type, user_id, sandbox_id, meta) VALUES (2, '2026-01-01T00:00:01.000Z', 'sandbox.destroy', 'u-alice', 'sb-0002', '{}');
INSERT INTO "metrics_snapshots" (ts, api_requests, api_errors, api_auth_failures, api_rate_limited, proxy_requests, proxy_errors, proxy_cold_wakes, proxy_rate_limited, events_dropped, sandboxes_total, sandboxes_hot, sandboxes_warm, sandboxes_cold, users_total, users_active, websockets_active, host_load_1m, host_mem_total_mb, host_mem_avail_mb) VALUES ('2026-01-01T00:00:00.000Z', 42, 0, 0, 0, 0, 0, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0.5, 0, 0);
INSERT INTO "secret_grants" (id, user_id, secret_name, sandbox_id, hosts_json, placeholder, created_at, expires_at, revoked_at) VALUES ('grant-1', 'u-alice', 'api-key', 'sb-0001', '["api.example.com"]', 'bhatti-ph-aaaa', '2026-01-01 00:00:00', NULL, NULL);
INSERT INTO "secret_grants" (id, user_id, secret_name, sandbox_id, hosts_json, placeholder, created_at, expires_at, revoked_at) VALUES ('grant-2', 'u-alice', 'db-pass', 'sb-0001', '["db.example.com"]', 'bhatti-ph-bbbb', '2026-01-01 00:00:00', '2027-01-01 00:00:00', NULL);
INSERT INTO "secret_grants" (id, user_id, secret_name, sandbox_id, hosts_json, placeholder, created_at, expires_at, revoked_at) VALUES ('grant-3', 'u-bob', 'api-key', 'sb-0003', '[]', 'bhatti-ph-cccc', '2026-01-01 00:00:00', NULL, '2026-01-02 03:04:05');
INSERT INTO "sandbox_cas" (sandbox_id, user_id, cert_pem, key_enc, created_at) VALUES ('sb-0001', 'u-alice', '-----BEGIN CERTIFICATE-----
sb1
-----END CERTIFICATE-----
', X'656E632D63612D6B65792D31', '2026-01-01 00:00:00');
