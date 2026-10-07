"""録画ストリーミングのエンコードタスク (VideoEncodingTask) の期待値を Python 版の実コードから生成する。

使い方 (server/ の uv 環境で、PYTHONPATH を外して実行する):
    cd server && env -u PYTHONPATH .venv/Scripts/python.exe ../server-go/tools/generate_videostream_encoding_fixture.py

生成先 (server-go/internal/videostream/testdata/):
    - encoding_synth.ts           : 合成した MPEG-TS (H.264 + AAC + その他 PID, 33bit DTS ラップを跨ぐ)
    - encoding_task_fixture.json  : 下記の期待値

期待値の作り方:
    - buildFFmpegOptions() / buildHWEncCOptions() を全組み合わせで呼び、引数列の SHA-256 (先頭 16 文字) を保存する。
      代表ケースは引数列そのものも保存する。
    - run() 本体は asyncio.subprocess.create_subprocess_exec() を偽物に差し替えて実際に走らせる。
      偽の tsreadex / psisimux / エンコーダーが受け取った argv と、tsreadex の stdin に流れたバイト列、
      偽のエンコーダー stdout (= 合成 TS) から切り出された HLS セグメント、収集された入力キーフレームを記録する。
    - 実エンコーダー・tsreadex・psisimux・FFmpeg は一切起動しない。
"""

import asyncio
import hashlib
import json
import os
import sys
import threading
from pathlib import Path
from types import SimpleNamespace

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))

from biim.mpeg2ts import ts  # noqa: E402
from biim.mpeg2ts.packetize import packetize_pes, packetize_section  # noqa: E402
from biim.mpeg2ts.pes import PES  # noqa: E402

import app.streams.VideoEncodingTask as VET  # noqa: E402
from app.constants import QUALITY, QUALITY_TYPES  # noqa: E402
from app.streams.StreamEncodingOptions import StreamEncodingOptions  # noqa: E402
from app.utils.TSKeyFrameSeeker import TSKeyFrameCollector, TSStreamInfo  # noqa: E402

OUT_DIR = REPO_ROOT / 'server-go' / 'internal' / 'videostream' / 'testdata'

ENCODERS = ['FFmpeg', 'QSVEncC', 'NVEncC', 'VCEEncC', 'rkmppenc']


def sha16(args: list[str]) -> str:
    return hashlib.sha256('\n'.join(args).encode('utf-8')).hexdigest()[:16]


def sha(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


# ---------------------------------------------------------------------------
# 合成 TS の生成
# ---------------------------------------------------------------------------

def mpeg_crc32(data: bytes) -> int:
    crc = 0xFFFFFFFF
    for byte in data:
        crc ^= byte << 24
        for _ in range(8):
            crc = ((crc << 1) ^ 0x04C11DB7) & 0xFFFFFFFF if crc & 0x80000000 else (crc << 1) & 0xFFFFFFFF
    return crc


def build_section(body_after_length: bytes, table_id: int) -> bytes:
    section_length = len(body_after_length) + 4
    head = bytes([table_id, 0xB0 | ((section_length >> 8) & 0x0F), section_length & 0xFF])
    without_crc = head + body_after_length
    return without_crc + mpeg_crc32(without_crc).to_bytes(4, 'big')


def build_pat(pmt_pid: int) -> bytes:
    body = bytes([0x00, 0x01, 0xC1, 0x00, 0x00, 0x00, 0x01, 0xE0 | (pmt_pid >> 8), pmt_pid & 0xFF])
    return build_section(body, 0x00)


def build_pmt(pcr_pid: int, streams: list[tuple[int, int]]) -> bytes:
    body = bytes([0x00, 0x01, 0xC1, 0x00, 0x00, 0xE0 | (pcr_pid >> 8), pcr_pid & 0xFF, 0xF0, 0x00])
    for stream_type, pid in streams:
        body += bytes([stream_type, 0xE0 | (pid >> 8), pid & 0xFF, 0xF0, 0x00])
    return build_section(body, 0x02)


def encode_timestamp(prefix: int, value: int) -> bytes:
    value &= (1 << 33) - 1
    return bytes([
        (prefix << 4) | (((value >> 30) & 0x07) << 1) | 1,
        (value >> 22) & 0xFF,
        (((value >> 15) & 0x7F) << 1) | 1,
        (value >> 7) & 0xFF,
        ((value & 0x7F) << 1) | 1,
    ])


def build_video_pes(pts: int, dts: int, es: bytes) -> bytes:
    # PES_packet_length = 0 (無制限) は FFmpeg の出力する映像 PES と同じ
    return bytes([0x00, 0x00, 0x01, 0xE0, 0x00, 0x00, 0x80, 0xC0, 0x0A]) + encode_timestamp(3, pts) + encode_timestamp(1, dts) + es


def build_audio_pes(pts: int, es: bytes) -> bytes:
    length = 3 + 5 + len(es)
    return bytes([0x00, 0x00, 0x01, 0xC0, length >> 8, length & 0xFF, 0x80, 0x80, 0x05]) + encode_timestamp(2, pts) + es


def generate_synth_ts() -> tuple[bytes, dict]:
    """33bit ラップを跨ぐ 300 フレーム (約 10 秒) の合成 TS を生成する"""

    video_pid, audio_pid, other_pid, pmt_pid = 0x0100, 0x0101, 0x0102, 0x1000
    base_dts = (1 << 33) - 450000  # ラップの 5 秒前から始める
    frame_ticks = 3003
    gop = 15
    frame_count = 300

    pat = build_pat(pmt_pid)
    pmt = build_pmt(video_pid, [(0x1B, video_pid), (0x0F, audio_pid)])

    out = bytearray()
    cc = {0: 0, pmt_pid: 0, video_pid: 0, audio_pid: 0, other_pid: 0}
    frames = []

    def write_psi() -> None:
        for packet in packetize_section(_Sec(pat), False, False, 0, 0, cc[0]):
            out.extend(packet)
            cc[0] = (cc[0] + 1) & 0xF
        for packet in packetize_section(_Sec(pmt), False, False, pmt_pid, 0, cc[pmt_pid]):
            out.extend(packet)
            cc[pmt_pid] = (cc[pmt_pid] + 1) & 0xF

    audio_next = 0  # 音声 PES を 1 フレームあたり 1 個入れる
    for index in range(frame_count):
        if index % 30 == 0:
            write_psi()
        dts = base_dts + index * frame_ticks
        pts = dts + 2 * frame_ticks
        is_idr = index % gop == 0
        if is_idr:
            # SPS(7) PPS(8) IDR(5)。0xAA 埋めで開始コードを含まないようにする
            es = (b'\x00\x00\x00\x01\x67' + b'\xAA' * 20 + b'\x00\x00\x00\x01\x68' + b'\xAA' * 8 +
                  b'\x00\x00\x00\x01\x65' + b'\xAA' * (900 + (index % 7) * 13))
        else:
            es = b'\x00\x00\x00\x01\x41' + b'\xAA' * (150 + (index % 11) * 29)
        offset = len(out)
        frames.append({'index': index, 'offset': offset, 'dts': dts, 'is_idr': is_idr})
        for packet in packetize_pes(PES(build_video_pes(pts & ((1 << 33) - 1), dts & ((1 << 33) - 1), es)), False, False, video_pid, 0, cc[video_pid]):
            out.extend(packet)
            cc[video_pid] = (cc[video_pid] + 1) & 0xF
        audio_pes = build_audio_pes((dts + frame_ticks) & ((1 << 33) - 1), b'\xBB' * (100 + (index % 5) * 17))
        for packet in packetize_pes(PES(audio_pes), False, False, audio_pid, 0, cc[audio_pid]):
            out.extend(packet)
            cc[audio_pid] = (cc[audio_pid] + 1) & 0xF
        if index % 9 == 0:
            # その他の PID (データ放送等) は素通しされる
            packet = bytes([0x47, 0x40 | (other_pid >> 8), other_pid & 0xFF, 0x10 | cc[other_pid]]) + bytes([0xCC]) * 184
            out.extend(packet)
            cc[other_pid] = (cc[other_pid] + 1) & 0xF
    _ = audio_next
    return bytes(out), {
        'video_pid': video_pid,
        'audio_pid': audio_pid,
        'pcr_pid': video_pid,
        'base_dts': base_dts,
        'frame_ticks': frame_ticks,
        'frames': frames,
    }


class _Sec:
    """packetize_section() に渡すためのセクション (len() とスライスだけ使われる)"""

    def __init__(self, data: bytes) -> None:
        self.data = data

    def __len__(self) -> int:
        return len(self.data)

    def __getitem__(self, item):  # type: ignore[no-untyped-def]
        return self.data[item]


# ---------------------------------------------------------------------------
# run() を偽プロセスで実行する
# ---------------------------------------------------------------------------

class FakeProcess:
    def __init__(self, stdout_bytes: bytes | None, spawn_record: dict) -> None:
        self.returncode: int | None = None
        self.stdout = asyncio.StreamReader()
        if stdout_bytes is not None:
            self.stdout.feed_data(stdout_bytes)
        self.stdout.feed_eof()
        self.stderr = asyncio.StreamReader()
        self.stderr.feed_eof()
        self.spawn_record = spawn_record

    def kill(self) -> None:
        self.returncode = -9

    async def wait(self) -> int:
        await asyncio.sleep(0)
        if self.returncode is None:
            self.returncode = 0
        return self.returncode


class FakeSegment:
    def __init__(self, sequence_index: int, playlist_start_seconds: float, duration_seconds: float,
                 source_file_position: int | None, source_start_dts: int | None) -> None:
        self.sequence_index = sequence_index
        self.playlist_start_seconds = playlist_start_seconds
        self.duration_seconds = duration_seconds
        self.source_file_position = source_file_position
        self.source_start_dts = source_start_dts
        self.encode_status = 'Pending'
        self.encoded_segment_ts_future = asyncio.get_running_loop().create_future()
        self.is_encoded_segment_ts_future_readed = False

    async def resetState(self) -> None:
        if not self.encoded_segment_ts_future.done():
            self.encoded_segment_ts_future.set_result(b'')
        self.encode_status = 'Pending'
        self.encoded_segment_ts_future = asyncio.get_running_loop().create_future()


class NoopLogging:
    def __getattr__(self, name):  # type: ignore[no-untyped-def]
        return lambda *args, **kwargs: None


async def run_scenario(scenario: dict, synth: bytes, synth_info: dict, ts_path: Path) -> dict:
    spawns: list[dict] = []
    captured_threads: list[threading.Thread] = []
    captured_stdin: dict[int, bytearray] = {}

    async def fake_create_subprocess_exec(program, *args, **kwargs):  # type: ignore[no-untyped-def]
        name = Path(program).stem.lower()
        record = {'name': name, 'args': list(args)}
        spawns.append(record)
        stdin = kwargs.get('stdin')
        if name == 'tsreadex':
            # tsreadex の stdin に流れたバイト列を取得する (親が閉じる前に dup する)
            data = bytearray()
            captured_stdin[len(spawns) - 1] = data
            if hasattr(stdin, 'read'):
                data.extend(stdin.read())
            elif isinstance(stdin, int):
                dup_fd = os.dup(stdin)

                def reader() -> None:
                    try:
                        while True:
                            chunk = os.read(dup_fd, 65536)
                            if not chunk:
                                break
                            data.extend(chunk)
                    except OSError:
                        pass
                    finally:
                        try:
                            os.close(dup_fd)
                        except OSError:
                            pass

                thread = threading.Thread(target=reader, daemon=True)
                thread.start()
                captured_threads.append(thread)
            return FakeProcess(None, record)
        if name == 'psisimux':
            return FakeProcess(None, record)
        # エンコーダー: 合成 TS をそのまま出力する (scenario['encoder_outputs_ts'] が False なら空出力)
        return FakeProcess(synth if scenario['encoder_outputs_ts'] else b'', record)

    saved_calls: list[list[dict]] = []

    class FakeVideoStream:
        log_prefix = '[fake]'
        SEGMENT_MAP_SAVE_BATCH_SIZE = 16

        def __init__(self) -> None:
            video = scenario['video']
            self.recorded_program = SimpleNamespace(
                recorded_video=SimpleNamespace(
                    file_path=str(ts_path) if video['container_format'] == 'MPEG-TS' else video['file_path'],
                    container_format=video['container_format'],
                    video_codec=video['video_codec'],
                    video_scan_type=video['video_scan_type'],
                    video_frame_rate=video['video_frame_rate'],
                    video_resolution_width=video['video_resolution_width'],
                    video_resolution_height=video['video_resolution_height'],
                ),
                channel=SimpleNamespace(**scenario['channel']) if scenario['channel'] is not None else None,
            )
            self.quality = scenario['quality']
            self.encoding_options = StreamEncodingOptions(
                is_hevc_10bit_enabled=scenario['is_hevc_10bit_enabled'],
                is_24fps_mode_enabled=scenario['is_24fps_mode_enabled'],
            )
            self.segments = [
                FakeSegment(
                    sequence_index=i,
                    playlist_start_seconds=i * scenario['segment_duration'],
                    duration_seconds=scenario['segment_duration'],
                    source_file_position=scenario['segment_positions'].get(i),
                    source_start_dts=scenario['segment_dts'].get(i),
                )
                for i in range(scenario['segment_count'])
            ]
            self.ts_stream_info = TSStreamInfo(
                video_pid=synth_info['video_pid'], pcr_pid=synth_info['pcr_pid'], codec='H.264', packet_size=188,
            ) if scenario['use_stream_info'] else None

        async def ensureTSKeyFrameContext(self) -> None:
            return None

        def createSegmentMapEntriesFromKeyFrames(self, key_frames):  # type: ignore[no-untyped-def]
            saved_calls.append([dict(k) for k in key_frames])
            return []

        async def saveSegmentMapEntries(self, entries):  # type: ignore[no-untyped-def]
            return None

    VET.Config = lambda: SimpleNamespace(general=SimpleNamespace(encoder=scenario['encoder'], debug_encoder=False))  # type: ignore[assignment]
    VET.IsHWEncCOptionAvailable = lambda encoder_type, option: scenario['adapt_resolution_available']  # type: ignore[assignment]
    VET.logging = NoopLogging()  # type: ignore[assignment]
    VET.LIBRARY_PATH = {name: name for name in ['FFmpeg', 'QSVEncC', 'NVEncC', 'VCEEncC', 'rkmppenc', 'tsreadex', 'psisimux']}
    original_create = asyncio.subprocess.create_subprocess_exec
    asyncio.subprocess.create_subprocess_exec = fake_create_subprocess_exec  # type: ignore[assignment]
    try:
        video_stream = FakeVideoStream()
        task = VET.VideoEncodingTask(video_stream)  # type: ignore[arg-type]
        await task.run(scenario['start_sequence'])
    finally:
        asyncio.subprocess.create_subprocess_exec = original_create  # type: ignore[assignment]
    for thread in captured_threads:
        thread.join(timeout=5)

    segments = []
    for segment in video_stream.segments:
        data = segment.encoded_segment_ts_future.result() if segment.encoded_segment_ts_future.done() else None
        segments.append({
            'status': segment.encode_status,
            'length': None if data is None else len(data),
            'sha256': None if data is None else sha(data),
        })
    last_keyframes = saved_calls[-1] if saved_calls else []
    return {
        'spawns': spawns,
        'tsreadex_stdin': {str(k): {'length': len(v), 'sha256': sha(bytes(v))} for k, v in captured_stdin.items()},
        'segments': segments,
        'retry_count': task._retry_count,
        'is_finished': task._is_finished,
        'keyframes': last_keyframes,
        'keyframe_flush_call_count': len(saved_calls),
    }


def main() -> int:
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    synth, info = generate_synth_ts()
    ts_path = OUT_DIR / 'encoding_synth.ts'
    ts_path.write_bytes(synth)
    frames = info['frames']
    idr_frames = [f for f in frames if f['is_idr']]

    # ---- (c) コマンドライン組み立て (全組み合わせ) ----
    option_hashes: dict[str, str] = {}
    option_samples: dict[str, list[str]] = {}
    for encoder in ENCODERS:
        for quality in QUALITY.keys():
            for scan_type in ['Interlaced', 'Progressive', 'Unknown']:
                for codec in ['MPEG-2', 'H.264']:
                    for width, height in [(1920, 1080), (1440, 1080), (3840, 2160)]:
                        for hevc10 in [False, True]:
                            for fps24 in [False, True]:
                                for retry in [0, 2]:
                                    for adapt in [True, False]:
                                        if encoder == 'FFmpeg' and adapt is False:
                                            continue
                                        frame_rate = 29.97 if scan_type != 'Progressive' else 23.976
                                        video_stream = SimpleNamespace(
                                            recorded_program=SimpleNamespace(recorded_video=SimpleNamespace(
                                                video_codec=codec,
                                                video_scan_type=scan_type,
                                                video_frame_rate=frame_rate,
                                                video_resolution_width=width,
                                                video_resolution_height=height,
                                            )),
                                            encoding_options=StreamEncodingOptions(
                                                is_hevc_10bit_enabled=hevc10, is_24fps_mode_enabled=fps24,
                                            ),
                                        )
                                        task = VET.VideoEncodingTask(video_stream)  # type: ignore[arg-type]
                                        task._retry_count = retry
                                        VET.IsHWEncCOptionAvailable = lambda encoder_type, option, adapt=adapt: adapt  # type: ignore[assignment]
                                        offset = 12.345 if retry == 0 else 0.0
                                        if encoder == 'FFmpeg':
                                            args = task.buildFFmpegOptions(quality, offset)  # type: ignore[arg-type]
                                        else:
                                            args = task.buildHWEncCOptions(quality, encoder, offset)  # type: ignore[arg-type]
                                        key = f'{encoder}|{quality}|{scan_type}|{codec}|{width}x{height}|{int(hevc10)}{int(fps24)}|{retry}|{int(adapt)}'
                                        option_hashes[key] = sha16(args)
                                        if (
                                            quality in ('1080p-60fps', '1080p-hevc', '720p') and
                                            codec == 'H.264' and (width, height) in ((1920, 1080), (3840, 2160)) and
                                            retry in (0, 2) and adapt is True and
                                            (hevc10, fps24) in ((False, False), (True, True))
                                        ):
                                            option_samples[key] = args

    # output_ts_offset の float 表記 (Python の repr) の検証用
    float_reprs = {str(v): repr(v / 90000) for v in [0, 1, 3003, 450000, 8589934592, 123456789, 8589934592 + 5, 90000 * 3600 * 5 + 17]}

    # ---- (a)(b)(d)(e)(f) run() の偽プロセス実行 ----
    base_video = {
        'container_format': 'MPEG-TS',
        'file_path': 'ignored',
        'video_codec': 'H.264',
        'video_scan_type': 'Interlaced',
        'video_frame_rate': 29.97,
        'video_resolution_width': 1920,
        'video_resolution_height': 1080,
    }
    unwrapped = {f['index']: f['dts'] for f in frames}
    scenarios = {
        # TS 入力 / FFmpeg / 最終セグメントまで到達 (is_split_pending 経路を含む)
        'ts_ffmpeg_final': {
            'video': base_video, 'channel': {'network_id': 32736, 'transport_stream_id': 32736, 'service_id': 1024},
            'quality': '720p', 'is_hevc_10bit_enabled': False, 'is_24fps_mode_enabled': False,
            'encoder': 'FFmpeg', 'adapt_resolution_available': True, 'encoder_outputs_ts': True,
            'segment_count': 4, 'segment_duration': 2.2, 'start_sequence': 0, 'use_stream_info': True,
            'segment_positions': {0: idr_frames[3]['offset']}, 'segment_dts': {0: idr_frames[3]['dts']},
        },
        # TS 入力 / NVEncC / 途中のセグメントから開始し、EOF で最終セグメントが部分的に確定する経路
        'ts_nvencc_partial': {
            'video': base_video, 'channel': None,
            'quality': '1080p-hevc', 'is_hevc_10bit_enabled': True, 'is_24fps_mode_enabled': True,
            'encoder': 'NVEncC', 'adapt_resolution_available': True, 'encoder_outputs_ts': True,
            'segment_count': 6, 'segment_duration': 2.2, 'start_sequence': 2, 'use_stream_info': True,
            'segment_positions': {2: idr_frames[6]['offset']}, 'segment_dts': {2: idr_frames[6]['dts']},
        },
        # TS 入力 / PAT/PMT が見つからない (ファイル先頭) 経路 + ストリーム情報なし (キーフレーム収集なし)
        'ts_no_patpmt': {
            'video': base_video, 'channel': {'network_id': 1, 'transport_stream_id': 2, 'service_id': 3},
            'quality': '480p', 'is_hevc_10bit_enabled': False, 'is_24fps_mode_enabled': False,
            'encoder': 'VCEEncC', 'adapt_resolution_available': False, 'encoder_outputs_ts': True,
            'segment_count': 3, 'segment_duration': 2.2, 'start_sequence': 0, 'use_stream_info': False,
            'segment_positions': {0: 0}, 'segment_dts': {0: info['base_dts']},
        },
        # MP4 入力 (psisimux 経路) / チャンネルあり・TSID なし / エンコーダーが PID を出力せず 10 回リトライして諦める
        'mp4_qsvencc_retry': {
            'video': {**base_video, 'container_format': 'MPEG-4', 'file_path': 'C:/rec/sample.mp4'},
            'channel': {'network_id': 32736, 'transport_stream_id': None, 'service_id': 1024},
            'quality': '1080p-60fps-hevc', 'is_hevc_10bit_enabled': True, 'is_24fps_mode_enabled': False,
            'encoder': 'QSVEncC', 'adapt_resolution_available': True, 'encoder_outputs_ts': False,
            'segment_count': 3, 'segment_duration': 2.2, 'start_sequence': 1, 'use_stream_info': False,
            'segment_positions': {}, 'segment_dts': {1: 123456},
        },
        # MP4 入力 / チャンネルなし
        'mp4_no_channel_ffmpeg': {
            'video': {**base_video, 'container_format': 'MPEG-4', 'file_path': 'C:/rec/sample.mp4', 'video_scan_type': 'Progressive', 'video_frame_rate': 23.976},
            'channel': None,
            'quality': '360p', 'is_hevc_10bit_enabled': False, 'is_24fps_mode_enabled': False,
            'encoder': 'FFmpeg', 'adapt_resolution_available': True, 'encoder_outputs_ts': True,
            'segment_count': 2, 'segment_duration': 2.2, 'start_sequence': 0, 'use_stream_info': False,
            'segment_positions': {}, 'segment_dts': {0: 0},
        },
    }

    scenario_results = {}
    for name, scenario in scenarios.items():
        result = asyncio.run(run_scenario(scenario, synth, info, ts_path))
        scenario_results[name] = {'input': scenario, 'result': result}

    # ---- (a) TSKeyFrameCollector 単体 (チャンクサイズを変えても同じ結果になること) ----
    collector_cases = {}
    stream_info = TSStreamInfo(video_pid=info['video_pid'], pcr_pid=info['pcr_pid'], codec='H.264', packet_size=188)
    for label, start_offset, initial_dts in [
        ('from_start', 0, info['base_dts']),
        ('from_idr3', idr_frames[3]['offset'], idr_frames[3]['dts']),
        ('from_idr3_far_target', idr_frames[3]['offset'], idr_frames[3]['dts'] + (1 << 33) * 3),
    ]:
        for chunk_packets in [10000, 1000, 7, 1]:
            collector = TSKeyFrameCollector(stream_info, initial_dts)
            found = []
            position = start_offset
            while position < len(synth):
                chunk = synth[position:position + 188 * chunk_packets]
                for key_frame in collector.push(chunk, position):
                    found.append({'offset': key_frame.source_file_position, 'dts': key_frame.source_start_dts})
                position += len(chunk)
            collector_cases[f'{label}|{chunk_packets}'] = {
                'start_offset': start_offset, 'initial_dts': initial_dts, 'chunk_packets': chunk_packets, 'key_frames': found,
            }

    # ---- (c) リトライ回数 0..9 の掃引 (analyzeduration / probesize / input_analyze の浮動小数点表記の検証) ----
    retry_sweep = {}
    for encoder in ENCODERS:
        for codec in ['MPEG-2', 'H.264']:
            for retry in range(10):
                video_stream = SimpleNamespace(
                    recorded_program=SimpleNamespace(recorded_video=SimpleNamespace(
                        video_codec=codec, video_scan_type='Interlaced', video_frame_rate=29.97,
                        video_resolution_width=1920, video_resolution_height=1080,
                    )),
                    encoding_options=StreamEncodingOptions(is_hevc_10bit_enabled=False, is_24fps_mode_enabled=False),
                )
                task = VET.VideoEncodingTask(video_stream)  # type: ignore[arg-type]
                task._retry_count = retry
                VET.IsHWEncCOptionAvailable = lambda encoder_type, option: True  # type: ignore[assignment]
                offset = retry * 1.1
                if encoder == 'FFmpeg':
                    args = task.buildFFmpegOptions('720p', offset)  # type: ignore[arg-type]
                else:
                    args = task.buildHWEncCOptions('720p', encoder, offset)  # type: ignore[arg-type]
                retry_sweep[f'{encoder}|{codec}|{retry}'] = args

    fixture = {
        'synth': {
            'file_size': len(synth),
            'sha256': sha(synth),
            'video_pid': info['video_pid'],
            'audio_pid': info['audio_pid'],
            'pcr_pid': info['pcr_pid'],
            'base_dts': info['base_dts'],
            'idr_frames': [{'index': f['index'], 'offset': f['offset'], 'dts': f['dts']} for f in idr_frames],
        },
        'option_hashes': option_hashes,
        'option_samples': option_samples,
        'float_reprs': float_reprs,
        'collector_cases': collector_cases,
        'retry_sweep': retry_sweep,
        'scenarios': scenario_results,
        'constants': {'gop_length_second': VET.VideoEncodingTask.GOP_LENGTH_SECOND, 'max_retry_count': VET.VideoEncodingTask.MAX_RETRY_COUNT},
    }
    (OUT_DIR / 'encoding_task_fixture.json').write_text(
        json.dumps(fixture, ensure_ascii=False, indent=1, sort_keys=True) + '\n', encoding='utf-8',
    )
    print(f'Wrote {OUT_DIR / "encoding_task_fixture.json"}: {len(option_hashes)} option cases, {len(scenario_results)} scenarios, ts={len(synth)} bytes')
    _ = (ts, unwrapped, QUALITY)
    return 0


if __name__ == '__main__':
    sys.exit(main())
