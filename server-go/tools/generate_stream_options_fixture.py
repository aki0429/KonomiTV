"""ライブストリーミングの画質・エンコードオプション・エンコーダー引数のパリティ検証用フィクスチャを生成する。

server/ で `uv run python ../server-go/tools/generate_stream_options_fixture.py` として実行する。
生成先: server-go/internal/stream/testdata/stream_options.json
"""

import hashlib
import json
import sys
from pathlib import Path
from types import SimpleNamespace

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))

from app.constants import QUALITY, QUALITY_TYPES  # noqa: E402
from app.streams import LiveEncodingTask  # noqa: E402
from app.streams import StreamEncodingOptions as StreamEncodingOptionsModule  # noqa: E402
from app.streams.StreamEncodingOptions import (  # noqa: E402
    StreamEncodingOptions,
    SplitQualityAndEncodingOptions,
)

ENCODERS = ['FFmpeg', 'QSVEncC', 'NVEncC', 'VCEEncC', 'rkmppenc']
CHANNEL_TYPES = ['GR', 'BS', 'CS', 'CATV', 'SKY', 'BS4K']

# SplitQualityAndEncodingOptions() の検証に使う品質指定
QUALITY_REQUESTS = [
    'original',
    '1080p-60fps',
    '1080p-60fps-hevc',
    '1080p-60fps-10bit',
    '1080p-60fps-24fps',
    '1080p-60fps-hevc-10bit',
    '1080p-60fps-hevc-24fps',
    '1080p-60fps-hevc-10bit-24fps',
    '1080p-hevc-10bit',
    '1080p-hevc-24fps',
    '1080p-hevc-10bit-24fps',
    '810p-10bit',
    '720p-24fps',
    '720p-hevc-10bit-24fps',
    '540p-10bit',
    '240p-24fps',
    '240p-hevc-10bit-24fps',
    # 不正な指定 (オプションの順序が逆・存在しない画質・余分なオプション)
    '720p-hevc-24fps-10bit',
    '10bit',
    '24fps',
    'invalid',
    '1080p-60fps-hevc-24fps-10bit',
    'original-10bit',
]


# 引数の完全一致を検証する代表的なケース (それ以外はハッシュのみを保存してフィクスチャを小さく保つ)
REPRESENTATIVE_CASES = {
    ('ffmpeg', '1080p-60fps', 'GR', False, 0, False),
    ('ffmpeg', '1080p-60fps-hevc', 'GR', False, 0, False),
    ('ffmpeg', '1080p-hevc', 'BS', True, 0, False),
    ('ffmpeg', '720p', 'GR', False, 0, False),
    ('ffmpeg', '720p', 'GR', True, 0, True),
    ('ffmpeg', '720p-hevc', 'CATV', False, 1, False),
    ('ffmpeg', '540p', 'CS', False, 3, True),
    ('ffmpeg', '240p-hevc', 'SKY', False, 0, False),
    ('ffmpeg', '480p', 'BS4K', False, 0, False),
    ('hwenc', '1080p-hevc', 'QSVEncC', 'GR', 0, False, False, True),
    ('hwenc', '720p', 'QSVEncC', 'GR', 0, False, False, True),
    ('hwenc', '720p-hevc', 'NVEncC', 'GR', 0, True, False, True),
    ('hwenc', '720p-hevc', 'NVEncC', 'GR', 2, False, True, True),
    ('hwenc', '1080p', 'VCEEncC', 'GR', 0, False, False, True),
    ('hwenc', '720p-hevc', 'rkmppenc', 'GR', 0, False, True, True),
    ('hwenc', '1080p-60fps', 'QSVEncC', 'SKY', 0, False, False, True),
    ('hwenc', '240p-hevc', 'NVEncC', 'BS4K', 0, False, False, False),
}


def args_hash(args: list[str]) -> str:
    """引数の配列からハッシュを計算する。"""
    joined = chr(10).join(args)
    return hashlib.sha256(joined.encode('utf-8')).hexdigest()


def build_fake_task(retry_count: int, encoding_options: StreamEncodingOptions) -> SimpleNamespace:
    """LiveEncodingTask のメソッドを呼び出すための最小限のダミーインスタンスを作る。"""
    return SimpleNamespace(
        _retry_count=retry_count,
        live_stream=SimpleNamespace(encoding_options=encoding_options),
        GOP_LENGTH_SECONDS_H264=LiveEncodingTask.LiveEncodingTask.GOP_LENGTH_SECONDS_H264,
        GOP_LENGTH_SECONDS_H265=LiveEncodingTask.LiveEncodingTask.GOP_LENGTH_SECONDS_H265,
    )


def main() -> None:
    fixture: dict = {
        'qualities': {
            quality: {
                'is_hevc': info.is_hevc,
                'is_60fps': info.is_60fps,
                'width': info.width,
                'height': info.height,
                'video_bitrate': info.video_bitrate,
                'video_bitrate_max': info.video_bitrate_max,
                'audio_bitrate': info.audio_bitrate,
            }
            for quality, info in QUALITY.items()
        },
        'quality_types': list(QUALITY_TYPES.__args__),
        'split': [],
        'ffmpeg': [],
        'hwenc': [],
    }

    # SplitQualityAndEncodingOptions() の検証
    for quality_request in QUALITY_REQUESTS:
        for encoder in ENCODERS:
            # Config().general.encoder を差し替えるために、モジュール内の Config をモンキーパッチする
            StreamEncodingOptionsModule.Config = lambda encoder=encoder: SimpleNamespace(
                general=SimpleNamespace(encoder=encoder),
            )
            result = SplitQualityAndEncodingOptions(quality_request)
            fixture['split'].append({
                'request': quality_request,
                'encoder': encoder,
                'result': None if result is None else {
                    'quality': result.quality,
                    'suffix': result.encoding_options.buildSuffix(),
                    'is_hevc_10bit_enabled': result.encoding_options.is_hevc_10bit_enabled,
                    'is_24fps_mode_enabled': result.encoding_options.is_24fps_mode_enabled,
                },
            })

    # FFmpeg の引数の検証 (画質 × チャンネル種別 × フル HD × リトライ回数 × 24fps モード)
    for quality in list(QUALITY.keys()):
        for channel_type in CHANNEL_TYPES:
            for is_fullhd in (False, True):
                for retry_count in (0, 1, 3):
                    for is_24fps in (False, True):
                        encoding_options = StreamEncodingOptions(is_24fps_mode_enabled=is_24fps)
                        task = build_fake_task(retry_count, encoding_options)
                        args = LiveEncodingTask.LiveEncodingTask.buildFFmpegOptions(
                            task, quality, channel_type, is_fullhd,
                        )
                        case = {
                            'q': quality,
                            'ct': channel_type,
                            'fh': is_fullhd,
                            'r': retry_count,
                            'fps': is_24fps,
                            'h': args_hash(args),
                        }
                        if ('ffmpeg', quality, channel_type, is_fullhd, retry_count, is_24fps) in REPRESENTATIVE_CASES:
                            case['args'] = args
                        fixture['ffmpeg'].append(case)

    # ラジオチャンネル向け FFmpeg の引数の検証
    for retry_count in (0, 1, 3):
        task = build_fake_task(retry_count, StreamEncodingOptions())
        fixture['ffmpeg_radio'] = fixture.get('ffmpeg_radio', [])
        radio_args = LiveEncodingTask.LiveEncodingTask.buildFFmpegOptionsForRadio(task)
        fixture['ffmpeg_radio'].append({
            'r': retry_count,
            'h': args_hash(radio_args),
            'args': radio_args,
        })

    # HWEncC の引数の検証 (--adapt-resolution の対応状況ごとに生成する)
    for is_adapt_resolution_available in (True, False):
        LiveEncodingTask.IsHWEncCOptionAvailable = lambda *_: is_adapt_resolution_available
        for quality in list(QUALITY.keys()):
            for encoder in ['QSVEncC', 'NVEncC', 'VCEEncC', 'rkmppenc']:
                for channel_type in ['GR', 'SKY', 'BS4K']:
                    for retry_count in (0, 2):
                        for is_hevc_10bit, is_24fps in ((False, False), (True, False), (False, True), (True, True)):
                            encoding_options = StreamEncodingOptions(
                                is_hevc_10bit_enabled=is_hevc_10bit,
                                is_24fps_mode_enabled=is_24fps,
                            )
                            task = build_fake_task(retry_count, encoding_options)
                            args = LiveEncodingTask.LiveEncodingTask.buildHWEncCOptions(
                                task, quality, encoder, channel_type, False,
                            )
                            case = {
                                'q': quality,
                                'e': encoder,
                                'ct': channel_type,
                                'fh': False,
                                'r': retry_count,
                                'b10': is_hevc_10bit,
                                'fps': is_24fps,
                                'ar': is_adapt_resolution_available,
                                'h': args_hash(args),
                            }
                            if ('hwenc', quality, encoder, channel_type, retry_count, is_hevc_10bit, is_24fps, is_adapt_resolution_available) in REPRESENTATIVE_CASES:
                                case['args'] = args
                            fixture['hwenc'].append(case)

    output_path = REPO_ROOT / 'server-go' / 'internal' / 'stream' / 'testdata' / 'stream_options.json'
    output_path.parent.mkdir(parents=True, exist_ok=True)
    with open(output_path, mode='w', encoding='utf-8') as file:
        json.dump(fixture, file, ensure_ascii=False, separators=(',', ':'))
    print(f'Wrote {output_path}')


if __name__ == '__main__':
    main()
