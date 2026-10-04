"""録画ストリーミングの副音声抽出の期待値を生成する。

2 音声の TS ファイルから入力セグメントと Python 版の抽出結果をフィクスチャとして保存する。

使い方: python tools/generate_videostream_audio_fixture.py <2 音声の TS ファイルのパス>
"""

import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))

from app.utils.TSKeyFrameSeeker import TSKeyFrameSeeker  # noqa: E402
from app.utils.TSSecondaryAudioExtractor import TSSecondaryAudioExtractor  # noqa: E402

# フィクスチャとして保存するパケット数
PACKET_COUNT = 500


def main() -> int:
    ts_path = Path(sys.argv[1])
    output_dir = REPO_ROOT / 'server-go' / 'internal' / 'videostream' / 'testdata'
    output_dir.mkdir(parents=True, exist_ok=True)

    data = ts_path.read_bytes()
    segment = data[: 188 * PACKET_COUNT]
    extracted = TSSecondaryAudioExtractor.extract(segment)

    stream_info = TSKeyFrameSeeker.findStreamInfo(ts_path)
    (output_dir / 'secondary_audio_input.ts').write_bytes(segment)
    (output_dir / 'secondary_audio_expected.ts').write_bytes(extracted)
    print(
        f'Wrote {output_dir / "secondary_audio_input.ts"} ({len(segment)} bytes), '
        f'{output_dir / "secondary_audio_expected.ts"} ({len(extracted)} bytes), '
        f'video_pid={stream_info.video_pid}, pcr_pid={stream_info.pcr_pid}',
    )
    return 0


if __name__ == '__main__':
    sys.exit(main())
