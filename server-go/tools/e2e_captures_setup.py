"""キャプチャ API の E2E 検証用の環境を構築する。

使い方: python tools/e2e_captures_setup.py <作業ディレクトリ>

- リポジトリの config.yaml と server/data/database.sqlite をコピーする
- capture.upload_folders を作業ディレクトリの captures フォルダに書き換える
- テスト用のキャプチャ画像 (EXIF メタデータ付き) を配置する
- EXIF メタデータの NID/SID と一致するチャンネルを DB に登録する (チャンネル名検索用)
"""

import shutil
import sqlite3
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
FIXTURE_DIR = REPO_ROOT / 'server-go' / 'internal' / 'captures' / 'testdata'


def main() -> None:
    workdir = Path(sys.argv[1])
    server_dir = workdir / 'server'
    data_dir = server_dir / 'data'
    data_dir.mkdir(parents=True, exist_ok=True)
    capture_dir = workdir / 'captures'
    capture_dir.mkdir(exist_ok=True)
    # 前回の実行で残ったファイルを削除する
    for path in capture_dir.iterdir():
        if path.is_file():
            path.unlink()

    # config.yaml とデータベースをコピーする
    config_text = (REPO_ROOT / 'config.yaml').read_text(encoding='utf-8')
    # capture.upload_folders をテスト用のフォルダに書き換える
    config_lines = config_text.splitlines(keepends=True)
    output_lines: list[str] = []
    in_capture_section = False
    in_upload_folders = False
    for line in config_lines:
        if in_upload_folders:
            # 元の upload_folders の要素行と閉じ括弧をスキップする
            if line.strip().startswith(']'):
                in_upload_folders = False
            continue
        if line.startswith('capture:'):
            in_capture_section = True
            output_lines.append(line)
            continue
        if in_capture_section and line and not line[0].isspace():
            in_capture_section = False
        if in_capture_section and line.strip().startswith('upload_folders:'):
            output_lines.append(f"    upload_folders: [\n        '{capture_dir.as_posix()}',\n    ]\n")
            in_upload_folders = True
            continue
        output_lines.append(line)
    (workdir / 'config.yaml').write_text(''.join(output_lines), encoding='utf-8')

    shutil.copy(REPO_ROOT / 'server' / 'data' / 'database.sqlite', data_dir / 'database.sqlite')
    shutil.copy(REPO_ROOT / 'server' / 'data' / 'jwt_secret.dat', data_dir / 'jwt_secret.dat')

    # テスト用のキャプチャ画像を配置する (更新日時を制御して並び順を検証できるようにする)
    first = capture_dir / 'e2e_capture1.jpg'
    shutil.copy(FIXTURE_DIR / 'capture_exif.jpg', first)
    second = capture_dir / 'e2e_capture2.png'
    shutil.copy(FIXTURE_DIR / 'capture_no_exif.png', second)
    # e2e_capture2.png の方が新しくなるように更新日時を設定する
    import os

    os.utime(first, (1750000000, 1750000000))
    os.utime(second, (1750000100, 1750000100))
    # キャプチャ画像以外のファイルは一覧に含まれない
    (capture_dir / 'note.txt').write_text('not an image', encoding='utf-8')

    # チャンネル名での検索用に、EXIF メタデータの NID/SID と一致するチャンネルを登録する
    connection = sqlite3.connect(data_dir / 'database.sqlite')
    connection.execute('DELETE FROM channels')
    connection.execute(
        'INSERT INTO channels (id, display_channel_id, network_id, service_id, transport_stream_id,'
        ' remocon_id, channel_number, type, name, jikkyo_force, is_subchannel, is_radiochannel, is_watchable)'
        " VALUES ('NID32736-SID1024', 'gr011', 32736, 1024, 32736, 1, '011', 'GR', 'NHK総合1・東京', NULL, 0, 0, 1)",
    )
    # キャプチャフォルダのテストデータを削除する
    connection.execute('DELETE FROM capture_bookmarks')
    connection.execute('DELETE FROM capture_folders')
    connection.commit()
    connection.close()
    print(f'Prepared {workdir}')


if __name__ == '__main__':
    main()
