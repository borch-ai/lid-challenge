CREATE TABLE IF NOT EXISTS user_profile (
	id VARCHAR(64) PRIMARY KEY,
	name VARCHAR(255) NOT NULL,
	phone VARCHAR(64) NOT NULL,
	street_address TEXT NOT NULL,
	locality VARCHAR(128) NOT NULL,
	region VARCHAR(128) NOT NULL,
	postal_code VARCHAR(32) NOT NULL,
	country VARCHAR(64) NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_user_profile_phone ON user_profile(phone);
CREATE INDEX IF NOT EXISTS idx_user_profile_name ON user_profile(name);
CREATE INDEX IF NOT EXISTS idx_user_profile_name_lower ON user_profile((LOWER(name)));
CREATE INDEX IF NOT EXISTS idx_user_profile_locality_lower ON user_profile((LOWER(locality)));
CREATE INDEX IF NOT EXISTS idx_user_profile_region_lower ON user_profile((LOWER(region)));
CREATE INDEX IF NOT EXISTS idx_user_profile_country_lower ON user_profile((LOWER(country)));

CREATE TABLE IF NOT EXISTS user_credential (
	user_id VARCHAR(64) PRIMARY KEY REFERENCES user_profile(id) ON DELETE CASCADE,
	username VARCHAR(128) UNIQUE NOT NULL,
	method VARCHAR(32) NOT NULL,
	password_hash TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL
);
