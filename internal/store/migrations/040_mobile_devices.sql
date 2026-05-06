CREATE TABLE IF NOT EXISTS access_pin_mobile_devices (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	pin TEXT NOT NULL REFERENCES access_pins(pin) ON DELETE CASCADE,
	device_uuid TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS idx_access_pin_mobile_devices_pin ON access_pin_mobile_devices(pin);
