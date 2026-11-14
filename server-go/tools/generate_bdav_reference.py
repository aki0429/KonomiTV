from __future__ import annotations

import argparse
import ast
import base64
import copy
import gzip
import hashlib
import importlib
import json
import runpy
import sqlite3
import struct
import sys
import tempfile
from datetime import datetime, timedelta
from pathlib import Path
from types import SimpleNamespace


parser = argparse.ArgumentParser()
parser.add_argument('reference_source', type=Path)
args = parser.parse_args()
source = args.reference_source.read_text(encoding='utf-8')
digest = hashlib.sha256(source.encode()).hexdigest()
assert digest == 'cc31837b5263266901b20e5f4da3f5a39f8e430835a6af0819542e8da99ed15f'
sys.path.insert(0, str(Path.cwd()))
namespace = runpy.run_path(str(args.reference_source))
analyzer_class, schemas = namespace['TSInfoAnalyzer'], namespace['schemas']
metadata_path = args.reference_source.with_name('MetadataAnalyzer.py')
metadata_source = metadata_path.read_text(encoding='utf-8')
assert hashlib.sha256(metadata_source.encode()).hexdigest() == 'd5c742cb7aa69e41daddd7b158d8dbce8dbd190ed7ad97848a6518e2c4d0c47d'
metadata_namespace = runpy.run_path(str(metadata_path))
metadata_class = metadata_namespace['MetadataAnalyzer']
app_config = importlib.import_module('app.config')
app_config._CONFIG = SimpleNamespace(general=SimpleNamespace(debug=False))


def crc(data):
    value = 0xffffffff
    for byte in data:
        value ^= byte << 24
        for _ in range(8):
            value = ((value << 1) ^ (0x04c11db7 if value & 0x80000000 else 0)) & 0xffffffff
    return value


def syntax(table_id, extension, body):
    raw = bytearray([table_id, 0xb0, 0, extension >> 8, extension & 255, 0xc1, 0, 0]) + body
    length = len(raw) - 3 + 4
    raw[1] |= length >> 8
    raw[2] = length & 255
    return bytes(raw) + struct.pack('>I', crc(raw))


def text(value):
    return b'\x0e' + value.encode()


def descriptor(tag, data):
    return bytes([tag, len(data)]) + data


def clock(value):
    mjd = (value.date() - datetime(1858, 11, 17).date()).days
    return struct.pack('>H', mjd) + bytes([n // 10 * 16 + n % 10 for n in (value.hour, value.minute, value.second)])


def duration(seconds):
    if seconds is None:
        return b'\xff' * 3
    return bytes([n // 10 * 16 + n % 10 for n in (seconds // 3600, seconds // 60 % 60, seconds % 60)])


start = datetime(2026, 10, 7, 10)


def partial_time(event_start, event_duration, current, adjustment=0, subtract=False):
    flags = 0xf8 | (4 if subtract else 0) | (1 if current else 0)
    raw = b'\x00' + (clock(event_start) if event_start else b'\xff' * 5) + duration(event_duration) + duration(adjustment) + bytes([flags])
    if current:
        raw += clock(current)
    return descriptor(0xc3, raw)


def sit(case, second, sid, onid):
    adjustment = 10 if case in ('offset-add', 'offset-subtract') else 0
    subtract = case == 'offset-subtract'
    encoded_delta = adjustment if subtract else -adjustment
    transmission = partial_time(None, None, start + timedelta(seconds=second + encoded_delta), adjustment, subtract)
    if case in ('service-clock', 'no-clock'):
        transmission = b''
    if onid != 4:
        transmission += descriptor(0xcd, b'\x08\x00')
    event_id, event_start, event_duration = 10, start, 60
    if case.startswith('unknown-duration'):
        event_duration = None
        if case == 'unknown-duration-next' and second >= 61:
            event_id, event_start, event_duration = 12, start + timedelta(seconds=61), 60
        if case == 'unknown-duration-last-valid' and second >= 61:
            event_duration = 60
    if case == 'missing-start':
        event_start = None
    ids = descriptor(0x85, struct.pack('>HHHB', onid, 1, event_id, 0))
    service_name = text('BDAV CHANNEL')
    service = descriptor(0x48, bytes([1, 0, len(service_name)]) + service_name)
    if case == 'missing-ids':
        ids = b''
    if case == 'missing-service-name':
        service = b''
    title, description = text('BDAV PROGRAM' if event_id == 10 else 'NEXT PROGRAM'), text('BDAV DESCRIPTION')
    if case == 'fragmented':
        title = text('BDAV ' + 'PROGRAM' * 20)
    short = descriptor(0x4d, b'jpn' + bytes([len(title)]) + title + bytes([len(description)]) + description)
    event_clock = start + timedelta(seconds=second + encoded_delta) if case == 'service-clock' else None
    if case == 'clock-priority':
        event_clock = start + timedelta(seconds=second + 3600)
    event_time = partial_time(event_start + timedelta(seconds=encoded_delta) if event_start else None, event_duration, event_clock, adjustment, subtract)
    descriptors = ids + service + short + event_time
    if case == 'paid':
        descriptors += descriptor(0x53, b'\x00\x05')
    body = struct.pack('>H', 0xf000 | len(transmission)) + transmission
    body += struct.pack('>HH', sid, 0x8000 | (4 << 12) | len(descriptors)) + descriptors
    return syntax(0x7f, 0xffff, body)


def packets(pid, section, counters):
    output = bytearray()
    position = 0
    while position < len(section):
        cc = counters.get(pid, 0)
        counters[pid] = (cc + 1) & 15
        packet = bytes([0x47, (0x40 if position == 0 else 0) | (pid >> 8), pid & 255, 0x10 | cc])
        if position == 0:
            packet += b'\x00'
        count = min(188 - len(packet), len(section) - position)
        packet += section[position:position + count]
        position += count
        output += packet + b'\xff' * (188 - len(packet))
    return output


def pcr(ticks):
    ticks %= 1 << 33
    raw = bytes([ticks >> 25, ticks >> 17 & 255, ticks >> 9 & 255, ticks >> 1 & 255, (ticks & 1) << 7 | 0x7e, 0])
    return b'\x47\x02\x00\x20\xb7\x10' + raw + b'\xff' * 176


root = Path(__file__).resolve().parents[1] / 'internal/metadata/testdata/bdav'
root.mkdir(parents=True, exist_ok=True)
cases = []
with tempfile.TemporaryDirectory() as temporary:
    # 地デジの枝番検索も本家の処理を使い、接続先だけを空の専用DBへ置き換える。
    import app.constants
    database = Path(temporary) / 'empty.sqlite'
    with sqlite3.connect(database) as connection:
        connection.execute('CREATE TABLE channels (network_id INT, service_id INT, channel_number TEXT, type TEXT)')
    connection.close()
    app.constants.DATABASE_CONFIG = copy.deepcopy(app.constants.DATABASE_CONFIG)
    app.constants.DATABASE_CONFIG['connections']['default'] = f'sqlite://{database.as_posix()}'
    for case in ['sit188', 'sit192', 'offset-add', 'offset-subtract', 'service-clock', 'no-clock', 'no-pcr', 'paid', 'ground-remocon', 'unknown-duration-next', 'unknown-duration-last-valid', 'unknown-duration-no-next', 'missing-start', 'missing-ids', 'missing-service-name', 'fragmented', 'clock-priority', 'masked-bs', 'tot-no-eit']:
        size = 188 if case in ('sit188', 'tot-no-eit') else 192
        clip_duration = 60 if case == 'tot-no-eit' else 65
        sid, onid = (1024, 0x7880) if case == 'ground-remocon' else (101, 4)
        if case == 'masked-bs':
            sid |= 0x180
        pat = syntax(0, 999, struct.pack('>HH', sid, 0xe100))
        pmt = syntax(2, sid, b'\xe2\x00\xf0\x00\x02\xe2\x00\xf0\x00\x0f\xe2\x01\xf0\x00')
        counters = {}
        data = bytearray(pcr(100000) if case != 'no-pcr' else b'')
        if case == 'tot-no-eit':
            raw = b'\x73\x70\x0b' + clock(datetime(2020, 1, 2, 3, 4, 5)) + b'\xf0\x00'
            data += packets(0x14, raw + struct.pack('>I', crc(raw)), counters)
        for second in range(5, 66):
            data += packets(0, pat, counters) + packets(0x100, pmt, counters)
            if case != 'no-pcr':
                data += pcr(100000 + second * 90000)
            sit_sid = sid & ~0x180 if case == 'masked-bs' and 10 <= second <= 25 else sid
            if case == 'tot-no-eit':
                service_name = text('BDAV CHANNEL')
                service = descriptor(0x48, bytes([1, 0, len(service_name)]) + service_name)
                data += packets(0x11, syntax(0x42, 999, struct.pack('>H', onid) + b'\xff' + struct.pack('>HBH', sid, 0xff, 0x8000 | len(service)) + service), counters)
            else:
                data += packets(0x1f, sit(case, second, sit_sid, onid), counters)
            # 本家の物理TSは10000パケット単位で読むため、最小ハッシュサイズ以上の録画にする。
            for _ in range(370):
                cc = counters.get(0x1fff, 0)
                counters[0x1fff] = (cc + 1) & 15
                data += b'\x47\x1f\xff' + bytes([0x10 | cc]) + b'\xff' * 184
        if size == 192:
            data = b''.join(struct.pack('>I', n // 188) + data[n:n + 188] for n in range(0, len(data), 188))
        media = Path(temporary) / f'{case}.mpegts'
        media.write_bytes(data)
        (root / f'{case}.mpegts.gz').write_bytes(gzip.compress(data, mtime=0))
        video = schemas.RecordedVideo.model_construct(file_path=str(media), file_size=len(data), duration=clip_duration, container_format='MPEG-TS', recording_start_time=None, recording_end_time=None)
        analyzer = analyzer_class(video)
        recording_time = analyzer.analyzeRecordingTime()
        if recording_time is not None:
            video.recording_start_time, video.recording_end_time = recording_time
        program = analyzer.analyze()
        meta = metadata_class(media)
        pcr_duration = meta._MetadataAnalyzer__calculateTSFileDuration()
        fallback_start = None
        if case == 'tot-no-eit':
            # FFprobe境界だけを制御し、本家MetadataAnalyzerのファイル名フォールバックまで実行する。
            probe_data = dict(format=dict(format_name='mpegts', duration='60'), programs=[], streams=[
                dict(index=0, codec_type='video', codec_name='h264', profile='High', width=640, height=360, avg_frame_rate='30/1', r_frame_rate='30/1', field_order='progressive', ts_packetsize='188'),
                dict(index=1, codec_type='audio', codec_name='aac', profile='LC', channels=2, sample_rate='48000'),
            ])
            full_probe = metadata_namespace['FFprobeResult'].model_validate(probe_data)
            sample_probe = metadata_namespace['FFprobeSampleResult'].model_validate(probe_data)
            setattr(meta, '_MetadataAnalyzer__analyzeFFprobe', lambda: (full_probe, sample_probe, None))
            metadata_class.analyze.__globals__['TSInfoAnalyzer'] = analyzer_class
            import os
            timestamp = datetime(2026, 10, 7, 11).timestamp()
            os.utime(media, (timestamp, timestamp))
            fallback = meta.analyze()
            assert fallback is not None
            fallback_start = fallback.start_time.isoformat()
        cases.append(dict(name=case, packet_size=size, duration=clip_duration, channel_database='empty',
                          recording_start=recording_time[0].isoformat() if recording_time else None,
                          recording_end=recording_time[1].isoformat() if recording_time else None,
                          title=program.title if program else None, start=program.start_time.isoformat() if program else None,
                          end=program.end_time.isoformat() if program else None, program_duration=program.duration if program else None,
                          event_id=program.event_id if program else None, channel=program.channel.id if program else None,
                          channel_number=program.channel.channel_number if program else None, channel_tsid=program.channel.transport_stream_id if program else None,
                          is_free=program.is_free if program else None,
                          pcr_duration=pcr_duration[0] if pcr_duration else None, valid_end=pcr_duration[1] if pcr_duration else None,
                          filename_fallback_start=fallback_start))
        analyzer.ts.close()
(root / 'reference.json').write_text(json.dumps(dict(source_sha256=digest, cases=cases), indent=2) + '\n', encoding='utf-8')

# 本家の部分AAC除去を独立した入力で実行する。アプリの設定やFFprobeは使わない。
metadata_tree = ast.parse(metadata_source)
clear_method = next(n for n in ast.walk(metadata_tree) if isinstance(n, ast.FunctionDef) and n.name == 'ClearLeadingPartialAACFrames')
module = ast.Module(body=[ast.ImportFrom(module='__future__', names=[ast.alias(name='annotations')], level=0), clear_method], type_ignores=[])
clear_namespace = dict(ts=metadata_namespace['ts'])
exec(compile(ast.fix_missing_locations(module), str(metadata_path), 'exec'), clear_namespace)
clear_aac = clear_namespace['ClearLeadingPartialAACFrames']


def adts(length=20, frequency=3, channels=2, profile=1):
    return bytes([0xff, 0xf1, profile << 6 | frequency << 2 | channels >> 2,
                  (channels & 3) << 6 | length >> 11, length >> 3 & 255,
                  (length & 7) << 5 | 0x1f, 0xfc]) + b'\x55' * (length - 7)


def pes(pid, data, stream_id=0xc0, adaptation=False, prefix_continuation=False):
    data = b'\x00\x00\x01' + bytes([stream_id, 0, 0, 0x80, 0, 0]) + data
    result, position, cc = bytearray(), 0, 0
    if prefix_continuation:
        result += bytes([0x47, pid >> 8, pid & 255, 0x10]) + b'\xaa' * 184
        cc = 1
    while position < len(data):
        header = bytes([0x47, (0x40 if position == 0 else 0) | pid >> 8, pid & 255, (0x30 if adaptation else 0x10) | cc])
        if adaptation:
            header += b'\x0a\x00' + b'\xff' * 9
        used = min(188 - len(header), len(data) - position)
        result += header + data[position:position + used] + b'\xff' * (188 - len(header) - used)
        position += used
        cc = (cc + 1) & 15
    return bytes(result)


samples = [
    ('partial', pes(0x200, b'partial' + adts() * 3)),
    ('cross-packet', pes(0x200, b'\x55' * 190 + adts(37) * 3)),
    ('two-audio-and-video', pes(0x200, b'left' + adts() * 3) + pes(0x201, b'other' + adts(channels=6) * 3) + pes(0x300, b'video' + adts() * 3, stream_id=0xe0)),
    ('leading-continuation', pes(0x200, b'left' + adts() * 3, prefix_continuation=True)),
    ('adaptation', pes(0x200, b'left' + adts() * 3, adaptation=True)),
    ('reserved-frequency', pes(0x200, b'left' + adts(frequency=15) * 3)),
    ('inconsistent-headers', pes(0x200, b'left' + adts() * 2 + adts(profile=0))),
    ('two-headers-only', pes(0x200, b'left' + adts() * 2)),
    ('latm-kept', pes(0x200, b'\x56\xe0\x20' * 20)),
]
aac_cases = []
for name, data in samples:
    for size in (188, 192):
        sample = data if size == 188 else b''.join(b'\x80\x00\x00\x01' + data[n:n + 188] for n in range(0, len(data), 188))
        aac_cases.append(dict(name=f'{name}-{size}', packet_size=size, input=base64.b64encode(sample).decode(), expected=base64.b64encode(clear_aac(sample, size)).decode()))
(root / 'aac_reference.json').write_text(json.dumps(dict(cases=aac_cases), indent=2) + '\n', encoding='utf-8')
print(root)
