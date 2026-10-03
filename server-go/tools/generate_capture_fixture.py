"""キャプチャ API の互換性テスト用フィクスチャを生成する。

EXIF XPComment にキャプチャメタデータ (UTF-16LE の JSON) を格納した JPEG と、
EXIF を持たない PNG を生成する。

使い方: python tools/generate_capture_fixture.py
"""

import json
import sys
from pathlib import Path

from PIL import Image

REPO_ROOT = Path(__file__).resolve().parents[2]
OUTPUT_DIR = REPO_ROOT / 'server-go' / 'internal' / 'captures' / 'testdata'

# キャプチャメタデータ (クライアント側の ICaptureExifData 相当)
METADATA = {
    'captured_at': '2026-09-22T15:11:58.874290+09:00',
    'captured_playback_position': 123.456,
    'network_id': 32736,
    'service_id': 1024,
    'event_id': 12345,
    'title': 'テスト番組 第1話「日本語タイトル」',
    'description': 'テスト番組の概要です。',
    'start_time': '2026-09-22T15:00:00+09:00',
    'end_time': '2026-09-22T15:30:00+09:00',
    'duration': 1800.0,
    'caption_text': '字幕テスト',
    'is_caption_composited': True,
    'is_comment_composited': False,
}


def main() -> int:
    OUTPUT_DIR.mkdir(parents=True, exist_ok=True)

    # EXIF XPComment 付きの JPEG を生成する (画像サイズは幅 1200 x 高さ 800)
    image = Image.new('RGB', (1200, 800), (32, 64, 128))
    exif = image.getexif()
    # XPComment (タグ番号 0x9C9C) に UTF-16LE の JSON を格納する (CaptureCompositor と同じ形式)
    exif[0x9C9C] = json.dumps(METADATA, ensure_ascii=False).encode('utf-16-le')
    # 回転情報 (Orientation) も付与しておく
    exif[0x0112] = 6
    image.save(OUTPUT_DIR / 'capture_exif.jpg', format='JPEG', quality=90, exif=exif)

    # EXIF を持たない PNG を生成する
    Image.new('RGB', (640, 480), (128, 32, 32)).save(OUTPUT_DIR / 'capture_no_exif.png', format='PNG')

    # 期待値 (Go 版のテストで使用する)
    expectations = {
        'capture_exif.jpg': {
            'mime_type': 'image/jpeg',
            'image_width': 1200,
            'image_height': 800,
            'orientation': 6,
            'capture_metadata': METADATA,
        },
        'capture_no_exif.png': {
            'mime_type': 'image/png',
            'image_width': 640,
            'image_height': 480,
            'orientation': 1,
            'capture_metadata': None,
        },
    }
    (OUTPUT_DIR / 'expected.json').write_text(
        json.dumps(expectations, ensure_ascii=False, indent=2) + '\n',
        encoding='utf-8',
    )
    print(f'Generated fixtures in {OUTPUT_DIR}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
