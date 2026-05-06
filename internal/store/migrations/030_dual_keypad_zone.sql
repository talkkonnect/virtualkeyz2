CREATE TABLE IF NOT EXISTS dual_keypad_zone_occupancy (
	pin TEXT PRIMARY KEY NOT NULL,
	inside_count INTEGER NOT NULL CHECK (inside_count > 0)
);
