-- Generated from tsukumijima/KonomiTV 687b904bb0a2cbb329596fd7b0368c09afe19a6c

-- Run server-go/tools/generate_schema.py to regenerate.

CREATE TABLE IF NOT EXISTS "account_links" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "user_id" INT NOT NULL REFERENCES "users" ("id") ON DELETE CASCADE,
            "twitter_account_id" INT NOT NULL UNIQUE REFERENCES "twitter_accounts" ("id") ON DELETE CASCADE,
            "bluesky_account_id" INT NOT NULL UNIQUE REFERENCES "bluesky_accounts" ("id") ON DELETE CASCADE,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

CREATE TABLE IF NOT EXISTS "aerich" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "version" VARCHAR(255) NOT NULL,
            "app" VARCHAR(100) NOT NULL,
            "content" JSON NOT NULL
        );

CREATE TABLE IF NOT EXISTS "bluesky_accounts" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "user_id" INT NOT NULL REFERENCES "users" ("id") ON DELETE CASCADE,
            "did" TEXT NOT NULL,
            "handle" TEXT NOT NULL,
            "name" TEXT NOT NULL,
            "icon_url" TEXT NOT NULL,
            "session_string" TEXT NOT NULL,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            CONSTRAINT "uid_bluesky_ac_user_id_did" UNIQUE ("user_id", "did")
        );

CREATE TABLE IF NOT EXISTS "capture_favorites" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "user_id" INT NOT NULL REFERENCES "users" ("id") ON DELETE CASCADE,
            "capture_id" INT NOT NULL REFERENCES "captures" ("id") ON DELETE CASCADE,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            CONSTRAINT "uid_capture_favorites_user_id_capture_id" UNIQUE ("user_id", "capture_id")
        );

CREATE TABLE IF NOT EXISTS "capture_files" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "capture_id" INT REFERENCES "captures" ("id") ON DELETE CASCADE,
            "file_path" VARCHAR(4096) NOT NULL UNIQUE,
            "file_name" VARCHAR(1024) NOT NULL,
            "file_size" BIGINT NOT NULL,
            "file_created_at" TIMESTAMP NOT NULL,
            "file_modified_at" TIMESTAMP NOT NULL,
            "status" VARCHAR(16) NOT NULL,
            "missing_since" TIMESTAMP,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

CREATE TABLE IF NOT EXISTS "capture_programs" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "network_id" INT NOT NULL,
            "service_id" INT NOT NULL,
            "event_id" INT NOT NULL,
            "start_time" TIMESTAMP NOT NULL,
            "end_time" TIMESTAMP,
            "duration" REAL,
            "title" TEXT NOT NULL,
            "description" TEXT NOT NULL,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            CONSTRAINT "uid_capture_programs_program_key" UNIQUE ("network_id", "service_id", "event_id", "start_time")
        );

CREATE TABLE IF NOT EXISTS "capture_tag_items" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "tag_id" INT NOT NULL REFERENCES "capture_tags" ("id") ON DELETE CASCADE,
            "capture_id" INT NOT NULL REFERENCES "captures" ("id") ON DELETE CASCADE,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            CONSTRAINT "uid_capture_tag_items_tag_id_capture_id" UNIQUE ("tag_id", "capture_id")
        );

CREATE TABLE IF NOT EXISTS "capture_tags" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "user_id" INT NOT NULL REFERENCES "users" ("id") ON DELETE CASCADE,
            "name" VARCHAR(255) NOT NULL,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            CONSTRAINT "uid_capture_tags_user_id_name" UNIQUE ("user_id", "name")
        );

CREATE TABLE IF NOT EXISTS "captures" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "file_hash" BLOB NOT NULL,
            "file_size" BIGINT NOT NULL,
            "captured_at" TIMESTAMP NOT NULL,
            "broadcast_at" TIMESTAMP,
            "image_width" INT NOT NULL,
            "image_height" INT NOT NULL,
            "metadata_status" VARCHAR(16) NOT NULL,
            "playback_position" REAL,
            "caption_text" TEXT,
            "is_caption_composited" INT NOT NULL DEFAULT 0,
            "is_comment_composited" INT NOT NULL DEFAULT 0,
            "capture_program_id" INT REFERENCES "capture_programs" ("id") ON DELETE SET NULL,
            "detached_since" TIMESTAMP,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

CREATE TABLE IF NOT EXISTS "channels" (
            "id" VARCHAR(255) NOT NULL PRIMARY KEY,
            "display_channel_id" VARCHAR(255) NOT NULL UNIQUE,
            "network_id" INT NOT NULL,
            "service_id" INT NOT NULL,
            "transport_stream_id" INT,
            "remocon_id" INT NOT NULL,
            "channel_number" VARCHAR(255) NOT NULL,
            "type" VARCHAR(255) NOT NULL,
            "name" TEXT NOT NULL,
            "jikkyo_force" INT,
            "is_subchannel" INT NOT NULL,
            "is_radiochannel" INT NOT NULL,
            "is_watchable" INT NOT NULL
        );

CREATE TABLE IF NOT EXISTS "programs" (
            "id" VARCHAR(255) NOT NULL PRIMARY KEY,
            "channel_id" VARCHAR(255) NOT NULL REFERENCES "channels" ("id") ON DELETE CASCADE,
            "network_id" INT NOT NULL,
            "service_id" INT NOT NULL,
            "event_id" INT NOT NULL,
            "title" TEXT NOT NULL,
            "description" TEXT NOT NULL,
            "detail" JSON NOT NULL,
            "start_time" TIMESTAMP NOT NULL,
            "end_time" TIMESTAMP NOT NULL,
            "duration" REAL NOT NULL,
            "is_free" INT NOT NULL,
            "genres" JSON NOT NULL,
            "video_type" TEXT,
            "video_codec" TEXT,
            "video_resolution" TEXT,
            "primary_audio_type" TEXT NOT NULL,
            "primary_audio_language" TEXT NOT NULL,
            "primary_audio_sampling_rate" TEXT NOT NULL,
            "secondary_audio_type" TEXT,
            "secondary_audio_language" TEXT,
            "secondary_audio_sampling_rate" TEXT
        );

CREATE TABLE IF NOT EXISTS "recorded_programs" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "recording_start_margin" REAL NOT NULL,
            "recording_end_margin" REAL NOT NULL,
            "is_partially_recorded" INT NOT NULL,
            "channel_id" VARCHAR(255) REFERENCES "channels" ("id") ON DELETE CASCADE,
            "network_id" INT,
            "service_id" INT,
            "event_id" INT,
            "series_id" INT REFERENCES "series" ("id") ON DELETE CASCADE,
            "series_broadcast_period_id" INT REFERENCES "series_broadcast_periods" ("id") ON DELETE CASCADE,
            "title" TEXT NOT NULL,
            "series_title" TEXT,
            "episode_number" VARCHAR(255),
            "subtitle" TEXT,
            "description" TEXT NOT NULL,
            "detail" JSON NOT NULL,
            "start_time" TIMESTAMP NOT NULL,
            "end_time" TIMESTAMP NOT NULL,
            "duration" REAL NOT NULL,
            "is_free" INT NOT NULL,
            "genres" JSON NOT NULL,
            "primary_audio_type" TEXT NOT NULL,
            "primary_audio_language" TEXT NOT NULL,
            "secondary_audio_type" TEXT,
            "secondary_audio_language" TEXT,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

CREATE TABLE IF NOT EXISTS "recorded_videos" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "recorded_program_id" INT NOT NULL REFERENCES "recorded_programs" ("id") ON DELETE CASCADE,
            "status" VARCHAR(255) NOT NULL,
            "file_path" TEXT NOT NULL,
            "file_hash" TEXT NOT NULL,
            "file_size" INT NOT NULL,
            "file_created_at" TIMESTAMP NOT NULL,
            "file_modified_at" TIMESTAMP NOT NULL,
            "recording_start_time" TIMESTAMP,
            "recording_end_time" TIMESTAMP,
            "duration" REAL NOT NULL,
            "container_format" VARCHAR(255) NOT NULL,
            "video_codec" VARCHAR(255) NOT NULL,
            "video_codec_profile" VARCHAR(255) NOT NULL,
            "video_scan_type" VARCHAR(255) NOT NULL,
            "video_frame_rate" REAL NOT NULL,
            "video_resolution_width" INT NOT NULL,
            "video_resolution_height" INT NOT NULL,
            "primary_audio_codec" VARCHAR(255) NOT NULL,
            "primary_audio_channel" VARCHAR(255) NOT NULL,
            "primary_audio_sampling_rate" INT NOT NULL,
            "secondary_audio_codec" VARCHAR(255),
            "secondary_audio_channel" VARCHAR(255),
            "secondary_audio_sampling_rate" INT,
            "key_frames" JSON NOT NULL,
            "cm_sections" JSON, -- Removed NOT NULL constraint
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        , "thumbnail_info" JSON, "segment_map" JSON NOT NULL DEFAULT '[]', "has_video_stream_changes" INT NOT NULL DEFAULT 0, "missing_since" TIMESTAMP);

CREATE TABLE IF NOT EXISTS "series" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "title" TEXT NOT NULL,
            "description" TEXT NOT NULL,
            "genres" JSON NOT NULL,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

CREATE TABLE IF NOT EXISTS "series_broadcast_periods" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "series_id" INT NOT NULL REFERENCES "series" ("id") ON DELETE CASCADE,
            "channel_id" VARCHAR(255) NOT NULL REFERENCES "channels" ("id") ON DELETE CASCADE,
            "start_date" DATE NOT NULL,
            "end_date" DATE NOT NULL
        );

CREATE TABLE IF NOT EXISTS "twitter_accounts" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "user_id" INT NOT NULL REFERENCES "users" ("id") ON DELETE CASCADE,
            "name" TEXT NOT NULL,
            "screen_name" TEXT NOT NULL,
            "icon_url" TEXT NOT NULL,
            "access_token" TEXT NOT NULL,
            "access_token_secret" TEXT NOT NULL,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        , "cookie_browser_info" JSON);

CREATE TABLE IF NOT EXISTS "users" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "name" TEXT NOT NULL,
            "password" TEXT NOT NULL,
            "is_admin" INT NOT NULL,
            "client_settings" JSON NOT NULL,
            "niconico_user_id" INT,
            "niconico_user_name" TEXT,
            "niconico_user_premium" INT,
            "niconico_access_token" TEXT,
            "niconico_refresh_token" TEXT,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

CREATE INDEX IF NOT EXISTS "capture_favorites_capture_id" ON "capture_favorites" ("capture_id");

CREATE INDEX IF NOT EXISTS "capture_files_capture_id_status" ON "capture_files" ("capture_id", "status");

CREATE INDEX IF NOT EXISTS "capture_files_name_size_modified_at" ON "capture_files" ("file_name", "file_size", "file_modified_at");

CREATE INDEX IF NOT EXISTS "capture_files_status_missing_since" ON "capture_files" ("status", "missing_since");

CREATE INDEX IF NOT EXISTS "capture_programs_start_time_id" ON "capture_programs" ("start_time", "id");

CREATE INDEX IF NOT EXISTS "capture_tag_items_capture_id" ON "capture_tag_items" ("capture_id");

CREATE INDEX IF NOT EXISTS "captures_broadcast_at_id" ON "captures" ("broadcast_at", "id");

CREATE INDEX IF NOT EXISTS "captures_captured_at_id" ON "captures" ("captured_at", "id");

CREATE INDEX IF NOT EXISTS "captures_program_position" ON "captures" ("capture_program_id", "playback_position", "id");

CREATE INDEX IF NOT EXISTS "programs_time_index" ON "programs" (
            "channel_id",
            "start_time",
            "end_time"
        );

CREATE INDEX IF NOT EXISTS "recorded_programs_channel_time" ON "recorded_programs" ("channel_id", "start_time");

CREATE INDEX IF NOT EXISTS "recorded_programs_program_key" ON "recorded_programs" ("network_id", "service_id", "event_id", "start_time");

CREATE INDEX IF NOT EXISTS "recorded_programs_start_time_id" ON "recorded_programs" ("start_time" DESC, "id" DESC);

CREATE INDEX IF NOT EXISTS "recorded_programs_time_range" ON "recorded_programs" ("start_time", "end_time");

CREATE INDEX IF NOT EXISTS "recorded_programs_title" ON "recorded_programs" ("title");

CREATE INDEX IF NOT EXISTS "recorded_videos_file_hash" ON "recorded_videos" ("file_hash");

CREATE INDEX IF NOT EXISTS "recorded_videos_file_path" ON "recorded_videos" ("file_path");

CREATE INDEX IF NOT EXISTS "recorded_videos_recorded_program_id" ON "recorded_videos" ("recorded_program_id");

CREATE INDEX IF NOT EXISTS "recorded_videos_status" ON "recorded_videos" ("status");

CREATE INDEX IF NOT EXISTS "series_title" ON "series" ("title");

CREATE UNIQUE INDEX IF NOT EXISTS "uid_captures_file_hash" ON "captures" ("file_hash");
