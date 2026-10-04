"""録画ストリーミングのキーフレーム探索の期待値を生成する。

使い方: python tools/generate_videostream_fixture.py <TS ファイルのパス> [出力先ディレクトリ]
"""

import json
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))

from biim.mpeg2ts import ts  # noqa: E402
from app.utils.TSKeyFrameSeeker import TSKeyFrameSeeker  # noqa: E402
from app.streams.VideoSegmentPlanner import VideoSegmentPlanner  # noqa: E402


def main() -> int:
    ts_path = Path(sys.argv[1])
    output_dir = Path(sys.argv[2]) if len(sys.argv) > 2 else (
        REPO_ROOT / 'server-go' / 'internal' / 'videostream' / 'testdata'
    )
    output_dir.mkdir(parents=True, exist_ok=True)

    stream_info = TSKeyFrameSeeker.findStreamInfo(ts_path)
    base_dts = TSKeyFrameSeeker.findBaseDTS(ts_path, stream_info)

    # 各プレイリスト時刻でのキーフレーム探索結果 (1 セグメント長を最大の古さとする)
    video_frame_rate = 30000 / 1001
    segment_duration = VideoSegmentPlanner.computeSegmentDurationSeconds(video_frame_rate)
    max_keyframe_age_ticks = round(segment_duration * ts.HZ)
    seeks = []
    for playlist_start_seconds in [0.0, 1.0, 3.0, 6.0, 6.006, 12.0, 18.5, 24.0, 29.0]:
        try:
            position = TSKeyFrameSeeker.seek(
                ts_path,
                stream_info,
                playlist_start_seconds,
                base_dts,
                max_keyframe_age_ticks,
            )
            seeks.append({
                'playlist_start_seconds': playlist_start_seconds,
                'source_file_position': position.source_file_position,
                'source_start_dts': position.source_start_dts,
            })
        except RuntimeError:
            seeks.append({
                'playlist_start_seconds': playlist_start_seconds,
                'source_file_position': None,
                'source_start_dts': None,
            })

    # セグメント長の計算 (代表的なフレームレート)
    segment_durations = {}
    for frame_rate in [29.97, 59.94, 23.976, 25.0, 30.0, 60.0, 15.0, 0.0, -1.0, 12.345]:
        segment_durations[str(frame_rate)] = VideoSegmentPlanner.computeSegmentDurationSeconds(frame_rate)

    result = {
        'file_size': ts_path.stat().st_size,
        'stream_info': {
            'video_pid': stream_info.video_pid,
            'pcr_pid': stream_info.pcr_pid,
            'codec': stream_info.codec,
            'packet_size': stream_info.packet_size,
        },
        'base_dts': base_dts,
        'video_frame_rate': video_frame_rate,
        'segment_duration_seconds': segment_duration,
        'seeks': seeks,
        'segment_durations': segment_durations,
    }
    (output_dir / 'ts_seeker.json').write_text(
        json.dumps(result, ensure_ascii=False, indent=2) + '\n',
        encoding='utf-8',
    )
    print(f'Wrote {output_dir / "ts_seeker.json"}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
