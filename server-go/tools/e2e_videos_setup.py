"""録画番組 API の E2E 検証用の環境を構築する。

使い方: python tools/e2e_videos_setup.py <作業ディレクトリ>

- リポジトリの config.yaml と server/data/database.sqlite をコピーする
- 録画番組・録画ファイル・サムネイル画像・補助ファイルを一時的に作成する
  (リポジトリの database.sqlite は変更しない)
"""

import json
import shutil
import sqlite3
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
JST = timezone(timedelta(hours=9))


def main() -> None:
    workdir = Path(sys.argv[1])
    server_dir = workdir / 'server'
    data_dir = server_dir / 'data'
    data_dir.mkdir(parents=True, exist_ok=True)
    (data_dir / 'thumbnails').mkdir(exist_ok=True)
    recorded_dir = workdir / 'recorded'
    recorded_dir.mkdir(exist_ok=True)

    # config.yaml とデータベースをコピーする
    shutil.copy(REPO_ROOT / 'config.yaml', workdir / 'config.yaml')
    shutil.copy(REPO_ROOT / 'server' / 'data' / 'database.sqlite', data_dir / 'database.sqlite')
    shutil.copy(REPO_ROOT / 'server' / 'data' / 'jwt_secret.dat', data_dir / 'jwt_secret.dat')

    connection = sqlite3.connect(data_dir / 'database.sqlite')
    connection.execute('PRAGMA foreign_keys = ON')

    # 既存のテストデータを削除する
    connection.execute('DELETE FROM recorded_programs')
    connection.execute('DELETE FROM channels')
    connection.execute('DELETE FROM series')
    connection.commit()

    # チャンネルを作成する
    connection.execute(
        'INSERT INTO channels (id, display_channel_id, network_id, service_id, transport_stream_id,'
        ' remocon_id, channel_number, type, name, jikkyo_force, is_subchannel, is_radiochannel, is_watchable)'
        " VALUES ('NID32736-SID1024', 'gr011', 32736, 1024, 32736, 1, '011', 'GR', 'NHK総合1・東京', NULL, 0, 0, 1)",
    )

    now = datetime.now(JST)
    timestamp = now.strftime('%Y-%m-%d %H:%M:%S.%f') + '+09:00'
    programs = [
        # (タイトル, シリーズタイトル, 開始時刻 (時間前), ファイル名, ファイルハッシュ, ステータス)
        ('テストドラマ #1', 'テストドラマ', 48, 'drama1.ts', 'hash_drama1', 'Recorded'),
        ('テストドラマ #2', 'テストドラマ', 24, 'drama2.ts', 'hash_drama2', 'Recorded'),
        ('ABCニュース', None, 12, 'news.ts', 'hash_news', 'Recorded'),
    ]
    created_ids: list[int] = []
    for title, series_title, hours_ago, file_name, file_hash, status in programs:
        start_time = now - timedelta(hours=hours_ago)
        end_time = start_time + timedelta(hours=1)
        cursor = connection.execute(
            'INSERT INTO recorded_programs ('
            ' recording_start_margin, recording_end_margin, is_partially_recorded,'
            ' channel_id, network_id, service_id, event_id, series_id, series_broadcast_period_id,'
            ' title, series_title, episode_number, subtitle, description, detail,'
            ' start_time, end_time, duration, is_free, genres,'
            ' primary_audio_type, primary_audio_language, secondary_audio_type, secondary_audio_language,'
            ' created_at, updated_at'
            ") VALUES (1.0, 1.0, 0, 'NID32736-SID1024', 32736, 1024, 1, NULL, NULL,"
            " ?, ?, '#1', 'サブタイトル', '録画番組の説明', '{\"字幕\": \"あり\"}',"
            " ?, ?, 3600.0, 1, '[{\"major\": \"ドラマ\", \"middle\": \"国内ドラマ\"}]',"
            " '2/0モード(ステレオ)', '日本語', NULL, NULL, ?, ?)",
            (title, series_title,
             start_time.strftime('%Y-%m-%d %H:%M:%S.%f') + '+09:00',
             end_time.strftime('%Y-%m-%d %H:%M:%S.%f') + '+09:00',
             timestamp, timestamp),
        )
        program_id = cursor.lastrowid
        created_ids.append(program_id)

        # 録画ファイル本体と補助ファイルを作成する
        file_path = recorded_dir / file_name
        file_path.write_bytes(b'\x47' * 1024)
        (recorded_dir / (file_name + '.program.txt')).write_text('program info', encoding='utf-8')
        (recorded_dir / (file_name + '.err')).write_text('error log', encoding='utf-8')

        # サムネイル情報 (schemas.ThumbnailInfo 互換)
        thumbnail_info = json.dumps({
            'version': 1,
            'representative': {'format': 'WebP', 'width': 480, 'height': 270},
            'tile': {
                'format': 'WebP', 'image_width': 1920, 'image_height': 1080,
                'tile_width': 240, 'tile_height': 135, 'total_tiles': 100,
                'column_count': 10, 'row_count': 10, 'interval_sec': 36.0,
            },
        }, ensure_ascii=False, separators=(',', ':'))

        connection.execute(
            'INSERT INTO recorded_videos ('
            ' recorded_program_id, status, file_path, file_hash, file_size,'
            ' file_created_at, file_modified_at, recording_start_time, recording_end_time, duration,'
            ' container_format, video_codec, video_codec_profile, video_scan_type,'
            ' video_frame_rate, video_resolution_width, video_resolution_height, has_video_stream_changes,'
            ' primary_audio_codec, primary_audio_channel, primary_audio_sampling_rate,'
            ' secondary_audio_codec, secondary_audio_channel, secondary_audio_sampling_rate,'
            ' key_frames, cm_sections, thumbnail_info, created_at, updated_at'
            ') VALUES (?, ?, ?, ?, 1024, ?, ?, ?, ?, 3600.0,'
            " 'MPEG-TS', 'H.264', 'High', 'Progressive', 29.97, 1920, 1080, 0,"
            " 'AAC-LC', 'Stereo', 48000, NULL, NULL, NULL,"
            " '[]', '[{\"start_time\": 10.0, \"end_time\": 20.0}]', ?, ?, ?)",
            (program_id, status, str(file_path), file_hash, timestamp, timestamp,
             start_time.strftime('%Y-%m-%d %H:%M:%S.%f') + '+09:00',
             end_time.strftime('%Y-%m-%d %H:%M:%S.%f') + '+09:00',
             thumbnail_info, timestamp, timestamp),
        )

        # 1 件目のみサムネイル画像を作成する (2 件目以降はデフォルトのサムネイル画像が返る)
        if file_name == 'drama1.ts':
            (data_dir / 'thumbnails' / f'{file_hash}.webp').write_bytes(b'webp-thumbnail')
            (data_dir / 'thumbnails' / f'{file_hash}_tile.webp').write_bytes(b'webp-tile-thumbnail')

    # デフォルトのサムネイル画像 (リポジトリの static/thumbnails/default.webp) をコピーする
    static_dir = server_dir / 'static'
    static_dir.mkdir(exist_ok=True)
    shutil.copytree(REPO_ROOT / 'server' / 'static', static_dir, dirs_exist_ok=True)

    connection.commit()
    connection.close()

    # 検証スクリプトで使う ID を保存する
    (workdir / 'ids.json').write_text(json.dumps({'program_ids': created_ids}), encoding='utf-8')
    print(f'Setup complete: {workdir} (program_ids = {created_ids})')


if __name__ == '__main__':
    main()
