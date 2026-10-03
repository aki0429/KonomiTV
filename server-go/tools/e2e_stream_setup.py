"""ライブストリーミングの E2E 検証環境を構築する。

実際の FFmpeg / tsreadex を使って、ローカルのテストストリーム (MPEG-2 TS) を
IPTV の疑似チャンネルとして取り込み、Go 版サーバーでライブストリーミングを検証するための
一時環境を作る。

使い方:
    python server-go/tools/e2e_stream_setup.py <作業ディレクトリ>

作業ディレクトリには以下が作成される:
    config.yaml     : 検証用のサーバー設定 (リポジトリの config.yaml を元に IPTV の設定だけ変更する)
    local.m3u       : ローカルのテストストリームを指す M3U プレイリスト
    test.ts         : テスト用の MPEG-2 TS ファイル
    server/         : -server-dir に指定するディレクトリ (thirdparty と static は実体へリンクする)
"""

import io
import shutil
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
SERVER_DIR = REPO_ROOT / 'server'
CONFIG_YAML_PATH = REPO_ROOT / 'config.yaml'
FFMPEG_PATH = SERVER_DIR / 'thirdparty' / 'FFmpeg' / 'ffmpeg.exe'
STATIC_DIR = SERVER_DIR / 'static'

# テストストリームの設定
TS_DURATION_SECONDS = 30
STREAM_PORT = 7099


def make_junction(link: Path, target: Path) -> None:
    """ディレクトリのジャンクションを作成する (Windows のみ) 。"""
    if link.exists():
        return
    result = subprocess.run(
        ['cmd', '/c', 'mklink', '/J', str(link), str(target)],
        capture_output=True,
    )
    if result.returncode != 0:
        # ジャンクションが作れない環境ではコピーする
        shutil.copytree(target, link)


def replace_config_value(config_text: str, key: str, value: str) -> str:
    """config.yaml の指定されたキーの値を差し替える (行単位で処理する) 。"""
    lines = config_text.split('\n')
    result: list[str] = []
    for line in lines:
        stripped = line.strip()
        if stripped.startswith(key + ':') and not stripped.startswith('#'):
            indent = line[:len(line) - len(line.lstrip())]
            result.append(f'{indent}{key}: {value}')
        else:
            result.append(line)
    return '\n'.join(result)


def replace_config_sources(config_text: str, source: str) -> str:
    """config.yaml の iptv.sources を指定されたプレイリストのみに差し替える。"""
    lines = config_text.split('\n')
    result: list[str] = []
    index = 0
    while index < len(lines):
        line = lines[index]
        stripped = line.strip()
        if stripped.startswith('sources:') and '[' in stripped:
            indent = line[:len(line) - len(line.lstrip())]
            result.append(f'{indent}sources: [')
            result.append(f"{indent}    '{source}',")
            result.append(f'{indent}]')
            index += 1
            # 元のフローシーケンスの残りを読み飛ばす
            while index < len(lines) and lines[index].strip() != ']':
                index += 1
            index += 1
            continue
        result.append(line)
        index += 1
    return '\n'.join(result)


def main() -> None:
    work_dir = Path(sys.argv[1]).resolve()
    if work_dir.exists():
        shutil.rmtree(work_dir)
    work_dir.mkdir(parents=True)

    # ***** config.yaml を作成する *****
    config_text = io.open(CONFIG_YAML_PATH, encoding='utf-8').read()
    config_text = replace_config_sources(config_text, str(work_dir / 'local.m3u'))
    # 誰も見ていないチャンネルのエンコードタスクを早めに終了させる
    config_text = replace_config_value(config_text, 'max_alive_time', '3')
    # プレイリストのキャッシュを短くする
    config_text = replace_config_value(config_text, 'cache_ttl', '60')
    io.open(work_dir / 'config.yaml', 'w', encoding='utf-8', newline='\n').write(config_text)

    # ***** ローカルのプレイリストを作成する *****
    io.open(work_dir / 'local.m3u', 'w', encoding='utf-8', newline='\n').write(
        '#EXTM3U\n'
        '#EXTINF:-1 tvg-id="LocalTest.jp" tvg-logo="https://example.com/logo.png" group-title="Test",Local Test Channel\n'
        f'http://127.0.0.1:{STREAM_PORT}/test.ts\n'
    )

    # ***** server ディレクトリを作成する *****
    server_dir = work_dir / 'server'
    (server_dir / 'data').mkdir(parents=True)
    (server_dir / 'logs').mkdir(parents=True)
    # thirdparty と static は実体をリンクする
    make_junction(server_dir / 'thirdparty', SERVER_DIR / 'thirdparty')
    make_junction(server_dir / 'static', STATIC_DIR)
    # JWT 秘密鍵とデータベースをコピーする (IPTV の疑似チャンネルしか使わないので中身は空でよい)
    shutil.copy(SERVER_DIR / 'data' / 'jwt_secret.dat', server_dir / 'data' / 'jwt_secret.dat')
    if (SERVER_DIR / 'data' / 'database.sqlite').exists():
        shutil.copy(SERVER_DIR / 'data' / 'database.sqlite', server_dir / 'data' / 'database.sqlite')

    # ***** テスト用の MPEG-2 TS を生成する *****
    print('Generating the test MPEG-2 TS file...')
    result = subprocess.run([
        str(FFMPEG_PATH),
        '-hide_banner', '-loglevel', 'error', '-y',
        '-f', 'lavfi', '-i', 'testsrc2=size=1440x1080:rate=30000/1001',
        '-f', 'lavfi', '-i', 'sine=frequency=440:sample_rate=48000',
        '-t', str(TS_DURATION_SECONDS),
        '-c:v', 'mpeg2video', '-b:v', '8M', '-g', '15',
        '-c:a', 'aac', '-b:a', '192K', '-ac', '2',
        '-f', 'mpegts', str(work_dir / 'test.ts'),
    ], capture_output=True)
    if result.returncode != 0:
        print(result.stderr.decode('utf-8', errors='ignore'))
        raise SystemExit('Failed to generate the test MPEG-2 TS file.')
    print(f'Created {work_dir / "test.ts"} ({(work_dir / "test.ts").stat().st_size} bytes)')
    print(f'Work directory: {work_dir}')


if __name__ == '__main__':
    main()
