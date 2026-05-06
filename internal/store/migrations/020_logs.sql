CREATE TABLE IF NOT EXISTS logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at TEXT NOT NULL,
	event_type TEXT NOT NULL DEFAULT 'event',
	event_name TEXT NOT NULL,
	device_client_id TEXT,
	detail_json TEXT NOT NULL DEFAULT '{}'
);
