CREATE TABLE IF NOT EXISTS user_profile (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	phone TEXT NOT NULL,
	street_address TEXT NOT NULL,
	locality TEXT NOT NULL,
	region TEXT NOT NULL,
	postal_code TEXT NOT NULL,
	country TEXT NOT NULL,
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_user_profile_phone ON user_profile(phone);
CREATE INDEX IF NOT EXISTS idx_user_profile_name ON user_profile(name);
CREATE INDEX IF NOT EXISTS idx_user_profile_name_lower ON user_profile(LOWER(name));
CREATE INDEX IF NOT EXISTS idx_user_profile_locality_lower ON user_profile(LOWER(locality));
CREATE INDEX IF NOT EXISTS idx_user_profile_region_lower ON user_profile(LOWER(region));
CREATE INDEX IF NOT EXISTS idx_user_profile_country_lower ON user_profile(LOWER(country));

CREATE TABLE IF NOT EXISTS user_credential (
	user_id TEXT PRIMARY KEY,
	username TEXT UNIQUE NOT NULL,
	method TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL,
	FOREIGN KEY (user_id) REFERENCES user_profile(id) ON DELETE CASCADE
);
