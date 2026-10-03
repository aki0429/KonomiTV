package api

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/auth"
	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// testJWTSecret はテストで使う JWT シークレット。
// python-jose で生成したフィクスチャトークン (pythonJoseAccessToken) と一致させている。
const testJWTSecret = "test-secret-for-server-go-compatibility-0123456789abcdef"

// pythonJoseAccessToken は python-jose (HS256) で生成したアクセストークンのフィクスチャ。
// sub=42 / typ=AccessToken / iss=KonomiTV Server / 有効期限は 2036 年頃まで。
// Go 側が Python 版の発行したトークンを検証できることを確認するために使う。
const pythonJoseAccessToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpc3MiOiJLb25vbWlUViBTZXJ2ZXIiLCJ0eXAiOiJBY2Nlc3NUb2tlbiIsInN1YiI6IjQyIiwiaWF0IjoxNzkxMDkwNjY2LCJleHAiOjIxMDY0NTA2NjYsImp0aSI6InRlc3QtanRpIn0.LPE72YVXU_P7v85PvlDArbCuQn6S125Lly9wI0-mcQM"

// testDatabaseSchema はテスト用の最小スキーマ (server/app/models/ 互換) 。
const testDatabaseSchema = `
CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL,
    password TEXT NOT NULL,
    is_admin INT NOT NULL,
    client_settings JSON NOT NULL,
    niconico_user_id INT,
    niconico_user_name TEXT,
    niconico_user_premium INT,
    niconico_access_token TEXT,
    niconico_refresh_token TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE twitter_accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    user_id INT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    screen_name TEXT NOT NULL,
    icon_url TEXT NOT NULL,
    access_token TEXT NOT NULL,
    access_token_secret TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    cookie_browser_info JSON
);
CREATE TABLE bluesky_accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    user_id INT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    did TEXT NOT NULL,
    handle TEXT NOT NULL,
    name TEXT NOT NULL,
    icon_url TEXT NOT NULL,
    session_string TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT uid_bluesky_ac_user_id_did UNIQUE (user_id, did)
);
CREATE TABLE account_links (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    user_id INT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    twitter_account_id INT NOT NULL UNIQUE REFERENCES twitter_accounts (id) ON DELETE CASCADE,
    bluesky_account_id INT NOT NULL UNIQUE REFERENCES bluesky_accounts (id) ON DELETE CASCADE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE channels (
    id VARCHAR(255) NOT NULL PRIMARY KEY,
    display_channel_id VARCHAR(255) NOT NULL UNIQUE,
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    transport_stream_id INT,
    remocon_id INT NOT NULL,
    channel_number VARCHAR(255) NOT NULL,
    type VARCHAR(255) NOT NULL,
    name TEXT NOT NULL,
    jikkyo_force INT,
    is_subchannel INT NOT NULL,
    is_radiochannel INT NOT NULL,
    is_watchable INT NOT NULL
);
CREATE TABLE series (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    genres JSON NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE series_broadcast_periods (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    series_id INT NOT NULL REFERENCES series (id) ON DELETE CASCADE,
    channel_id VARCHAR(255) NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    start_date DATE NOT NULL,
    end_date DATE NOT NULL
);
CREATE TABLE recorded_programs (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    recording_start_margin REAL NOT NULL,
    recording_end_margin REAL NOT NULL,
    is_partially_recorded INT NOT NULL,
    channel_id VARCHAR(255) REFERENCES channels (id) ON DELETE CASCADE,
    network_id INT,
    service_id INT,
    event_id INT,
    series_id INT REFERENCES series (id) ON DELETE CASCADE,
    series_broadcast_period_id INT REFERENCES series_broadcast_periods (id) ON DELETE CASCADE,
    title TEXT NOT NULL,
    series_title TEXT,
    episode_number VARCHAR(255),
    subtitle TEXT,
    description TEXT NOT NULL,
    detail JSON NOT NULL,
    start_time TIMESTAMP NOT NULL,
    end_time TIMESTAMP NOT NULL,
    duration REAL NOT NULL,
    is_free INT NOT NULL,
    genres JSON NOT NULL,
    primary_audio_type TEXT NOT NULL,
    primary_audio_language TEXT NOT NULL,
    secondary_audio_type TEXT,
    secondary_audio_language TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    is_series_manually_edited INT NOT NULL DEFAULT 0
);
CREATE TABLE capture_folders (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    name TEXT NOT NULL,
    sort_order INT NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    user_id INT NOT NULL REFERENCES users (id) ON DELETE CASCADE
);
CREATE TABLE capture_bookmarks (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    filename TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    folder_id INT NOT NULL REFERENCES capture_folders (id) ON DELETE CASCADE
);
CREATE TABLE recorded_videos (
    id INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
    recorded_program_id INT NOT NULL REFERENCES recorded_programs (id) ON DELETE CASCADE,
    status VARCHAR(255) NOT NULL,
    file_path TEXT NOT NULL,
    file_hash TEXT NOT NULL,
    file_size INT NOT NULL,
    file_created_at TIMESTAMP NOT NULL,
    file_modified_at TIMESTAMP NOT NULL,
    recording_start_time TIMESTAMP,
    recording_end_time TIMESTAMP,
    duration REAL NOT NULL,
    container_format VARCHAR(255) NOT NULL,
    video_codec VARCHAR(255) NOT NULL,
    video_codec_profile VARCHAR(255) NOT NULL,
    video_scan_type VARCHAR(255) NOT NULL,
    video_frame_rate REAL NOT NULL,
    video_resolution_width INT NOT NULL,
    video_resolution_height INT NOT NULL,
    primary_audio_codec VARCHAR(255) NOT NULL,
    primary_audio_channel VARCHAR(255) NOT NULL,
    primary_audio_sampling_rate INT NOT NULL,
    secondary_audio_codec VARCHAR(255),
    secondary_audio_channel VARCHAR(255),
    secondary_audio_sampling_rate INT,
    key_frames JSON NOT NULL,
    cm_sections JSON,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    thumbnail_info JSON,
    segment_map JSON NOT NULL DEFAULT '[]',
    has_video_stream_changes INT NOT NULL DEFAULT 0
);
CREATE TABLE programs (
    id VARCHAR(255) NOT NULL PRIMARY KEY,
    channel_id VARCHAR(255) NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    network_id INT NOT NULL,
    service_id INT NOT NULL,
    event_id INT NOT NULL,
    title TEXT NOT NULL,
    description TEXT NOT NULL,
    detail JSON NOT NULL,
    start_time TIMESTAMP NOT NULL,
    end_time TIMESTAMP NOT NULL,
    duration REAL NOT NULL,
    is_free INT NOT NULL,
    genres JSON NOT NULL,
    video_type TEXT,
    video_codec TEXT,
    video_resolution TEXT,
    primary_audio_type TEXT NOT NULL,
    primary_audio_language TEXT NOT NULL,
    primary_audio_sampling_rate TEXT NOT NULL,
    secondary_audio_type TEXT,
    secondary_audio_language TEXT,
    secondary_audio_sampling_rate TEXT
);
`

// createTestDatabase はテスト用の SQLite データベースを生成する。
func createTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.sqlite")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(testDatabaseSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// createTestUser はテスト用のユーザーを作成し、ID を返す。
func createTestUser(t *testing.T, db *sql.DB, name string, password string, isAdmin bool) int64 {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(constants.JST).Format("2006-01-02 15:04:05.999999-07:00")
	result, err := db.Exec(
		`INSERT INTO users (name, password, is_admin, client_settings, created_at, updated_at) VALUES (?, ?, ?, '{}', ?, ?)`,
		name, hash, isAdmin, now, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// setTestUserID はテスト用ユーザーの ID を明示的に指定する (フィクスチャトークンの sub と合わせるため) 。
func setTestUserID(t *testing.T, db *sql.DB, username string, id int64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE users SET id = ? WHERE name = ?`, id, username); err != nil {
		t.Fatal(err)
	}
}

// issueTestToken はテスト用のアクセストークンを発行する。
func issueTestToken(t *testing.T, userID int64) string {
	t.Helper()
	manager := auth.NewWithSecret(testJWTSecret, nil, true, nil)
	token, err := manager.GenerateAccessToken(userID)
	if err != nil {
		t.Fatalf("failed to issue test token: %v", err)
	}
	return token
}
