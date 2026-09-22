from tortoise import BaseDBAsyncClient


async def upgrade(db: BaseDBAsyncClient) -> str:
    return """
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
    """


async def downgrade(db: BaseDBAsyncClient) -> str:
    return """
        DROP INDEX IF EXISTS "idx_capture_bookmarks_filename";
        DROP INDEX IF EXISTS "idx_capture_folders_user_id";
        DROP INDEX IF EXISTS "uidx_capture_book_folder__filename";
        DROP TABLE IF EXISTS "capture_bookmarks";
        DROP TABLE IF EXISTS "capture_folders";
    """
