-- 0_20230512060000_init.py

        DROP TABLE IF EXISTS "channels";
        CREATE TABLE "channels" (
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
        DROP TABLE IF EXISTS "programs";
        CREATE TABLE "programs" (
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
        DROP INDEX IF EXISTS "programs_time_index";
        CREATE INDEX "programs_time_index" ON "programs" (
            "channel_id",
            "start_time",
            "end_time"
        );
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
        );
        DROP TABLE IF EXISTS "aerich";
        CREATE TABLE IF NOT EXISTS "aerich" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "version" VARCHAR(255) NOT NULL,
            "app" VARCHAR(100) NOT NULL,
            "content" JSON NOT NULL
        );
    
-- 1_20230621210000_update.py

        DROP TABLE IF EXISTS "recorded_programs";
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
            "secondary_audio_language" TEXT
        );
        DROP TABLE IF EXISTS "recorded_videos";
        CREATE TABLE IF NOT EXISTS "recorded_videos" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "recorded_program_id" INT NOT NULL REFERENCES "recorded_programs" ("id") ON DELETE CASCADE,
            "file_path" TEXT NOT NULL,
            "file_hash" TEXT NOT NULL,
            "file_size" INT NOT NULL,
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
            "cm_sections" JSON NOT NULL
        );
        DROP TABLE IF EXISTS "series";
        CREATE TABLE IF NOT EXISTS "series" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "title" TEXT NOT NULL,
            "description" TEXT NOT NULL,
            "genres" JSON NOT NULL,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );
        DROP TABLE IF EXISTS "series_broadcast_periods";
        CREATE TABLE IF NOT EXISTS "series_broadcast_periods" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "series_id" INT NOT NULL REFERENCES "series" ("id") ON DELETE CASCADE,
            "channel_id" VARCHAR(255) NOT NULL REFERENCES "channels" ("id") ON DELETE CASCADE,
            "start_date" DATE NOT NULL,
            "end_date" DATE NOT NULL
        );
    
-- 2_20250130170000_update.py

        -- Drop existing tables
        DROP TABLE IF EXISTS "recorded_programs";
        DROP TABLE IF EXISTS "recorded_videos";
        DROP TABLE IF EXISTS "series";

        -- Recreate tables with new columns and indexes
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
            "cm_sections" JSON NOT NULL,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

        CREATE TABLE IF NOT EXISTS "series" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "title" TEXT NOT NULL,
            "description" TEXT NOT NULL,
            "genres" JSON NOT NULL,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );

        -- Create indexes for better query performance
        CREATE INDEX "recorded_programs_time_range" ON "recorded_programs" ("start_time", "end_time");
        CREATE INDEX "recorded_programs_channel_time" ON "recorded_programs" ("channel_id", "start_time");
        CREATE INDEX "recorded_videos_file_path" ON "recorded_videos" ("file_path");
        CREATE INDEX "recorded_videos_file_hash" ON "recorded_videos" ("file_hash");
        CREATE INDEX "series_title" ON "series" ("title");
    
-- 3_20250214100000_update.py

        CREATE INDEX "recorded_programs_start_time_id" ON "recorded_programs" ("start_time" DESC, "id" DESC);
        CREATE INDEX "recorded_programs_title" ON "recorded_programs" ("title");
    
-- 4_20250428010000_update.py

        -- Temporarily disable foreign key constraints (may be disabled by default in SQLite, but just in case)
        PRAGMA foreign_keys=off;

        -- Create a temporary table with the new schema (cm_sections allows NULL)
        CREATE TABLE "recorded_videos_new" (
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
        );

        -- Copy data from existing table to temporary table
        INSERT INTO "recorded_videos_new" (
            "id", "recorded_program_id", "status", "file_path", "file_hash", "file_size",
            "file_created_at", "file_modified_at", "recording_start_time", "recording_end_time",
            "duration", "container_format", "video_codec", "video_codec_profile", "video_scan_type",
            "video_frame_rate", "video_resolution_width", "video_resolution_height",
            "primary_audio_codec", "primary_audio_channel", "primary_audio_sampling_rate",
            "secondary_audio_codec", "secondary_audio_channel", "secondary_audio_sampling_rate",
            "key_frames", "cm_sections", "created_at", "updated_at"
        )
        SELECT
            "id", "recorded_program_id", "status", "file_path", "file_hash", "file_size",
            "file_created_at", "file_modified_at", "recording_start_time", "recording_end_time",
            "duration", "container_format", "video_codec", "video_codec_profile", "video_scan_type",
            "video_frame_rate", "video_resolution_width", "video_resolution_height",
            "primary_audio_codec", "primary_audio_channel", "primary_audio_sampling_rate",
            "secondary_audio_codec", "secondary_audio_channel", "secondary_audio_sampling_rate",
            "key_frames", "cm_sections", "created_at", "updated_at"
        FROM "recorded_videos";

        -- Drop existing table
        DROP TABLE "recorded_videos";

        -- Rename temporary table to original table name
        ALTER TABLE "recorded_videos_new" RENAME TO "recorded_videos";

        -- Recreate indexes (copy from original migration file)
        CREATE INDEX "recorded_videos_file_path" ON "recorded_videos" ("file_path");
        CREATE INDEX "recorded_videos_file_hash" ON "recorded_videos" ("file_hash");

        -- Re-enable foreign key constraints
        PRAGMA foreign_keys=on;
    
-- 5_20250429010000_update.py

        CREATE INDEX IF NOT EXISTS "recorded_videos_recorded_program_id" ON "recorded_videos" ("recorded_program_id");
    
-- 6_20260108120000_update.py

        -- Add thumbnail_info as a nullable JSON column
        ALTER TABLE "recorded_videos" ADD COLUMN "thumbnail_info" JSON;
    
-- 7_20260214060000_update.py

        CREATE INDEX IF NOT EXISTS "recorded_videos_status" ON "recorded_videos" ("status");
    
-- 8_20260521010000_update.py

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
        CREATE TABLE IF NOT EXISTS "account_links" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "user_id" INT NOT NULL REFERENCES "users" ("id") ON DELETE CASCADE,
            "twitter_account_id" INT NOT NULL UNIQUE REFERENCES "twitter_accounts" ("id") ON DELETE CASCADE,
            "bluesky_account_id" INT NOT NULL UNIQUE REFERENCES "bluesky_accounts" ("id") ON DELETE CASCADE,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
        );
    
-- 9_20260528010000_update.py

        ALTER TABLE "twitter_accounts" ADD COLUMN "cookie_browser_info" JSON;
    
-- 10_20260530010000_update.py

        ALTER TABLE "recorded_videos" ADD COLUMN "segment_map" JSON NOT NULL DEFAULT '[]';
    
-- 11_20260604000000_update.py

        ALTER TABLE "recorded_videos" ADD COLUMN "has_video_stream_changes" INT NOT NULL DEFAULT 0;
    
-- 12_20260922150000_update.py

        -- キャプチャフォルダテーブルの作成
        CREATE TABLE IF NOT EXISTS "capture_folders" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "name" TEXT NOT NULL,
            "sort_order" INT NOT NULL DEFAULT 0,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "updated_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "user_id" INT NOT NULL REFERENCES "users" ("id") ON DELETE CASCADE
        );
        -- キャプチャブックマーク (中間テーブル) の作成
        CREATE TABLE IF NOT EXISTS "capture_bookmarks" (
            "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
            "filename" TEXT NOT NULL,
            "created_at" TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
            "folder_id" INT NOT NULL REFERENCES "capture_folders" ("id") ON DELETE CASCADE
        );
        -- 同一フォルダ内に同じファイル名の重複紐付けを防止するユニーク制約
        CREATE UNIQUE INDEX IF NOT EXISTS "uidx_capture_book_folder__filename" ON "capture_bookmarks" ("folder_id", "filename");
        -- フォルダ検索用のインデックス (ユーザー ID でのフィルタリング高速化)
        CREATE INDEX IF NOT EXISTS "idx_capture_folders_user_id" ON "capture_folders" ("user_id");
        -- ブックマーク検索用のインデックス (ファイル名での検索高速化)
        CREATE INDEX IF NOT EXISTS "idx_capture_bookmarks_filename" ON "capture_bookmarks" ("filename");
    