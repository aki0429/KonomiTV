from __future__ import annotations

import argparse
import hashlib
import json
import runpy
import struct
import sys
import tempfile
from datetime import datetime, timedelta
from pathlib import Path


parser = argparse.ArgumentParser()
parser.add_argument('reference_source', type=Path)
args = parser.parse_args()
digest = hashlib.sha256(args.reference_source.read_text(encoding='utf-8').encode()).hexdigest()
assert digest == 'cc31837b5263266901b20e5f4da3f5a39f8e430835a6af0819542e8da99ed15f'
sys.path.insert(0, str(Path.cwd()))
namespace = runpy.run_path(str(args.reference_source))
analyzer_class, schemas = namespace['TSInfoAnalyzer'], namespace['schemas']
original_tsopen = namespace['ariblib'].tsopen
virtual_packets = []


def observed_tsopen(data, *args, **kwargs):
    if hasattr(data, 'getvalue'):
        virtual_packets.append(data.getvalue())
    return original_tsopen(data, *args, **kwargs)


namespace['ariblib'].tsopen = observed_tsopen


def crc(data):
    value = 0xffffffff
    for byte in data:
        value ^= byte << 24
        for _ in range(8):
            value = ((value << 1) ^ (0x04c11db7 if value & 0x80000000 else 0)) & 0xffffffff
    return value


def syntax(table_id, extension, number, body):
    raw = bytearray([table_id, 0xb0, 0, extension >> 8, extension & 255, 0xc1, number, number]) + body
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


def tot(value):
    raw = b'\x73\x70\x0b' + clock(value) + b'\xf0\x00'
    return raw + struct.pack('>I', crc(raw))


def eit(number, event_id, start, title, unknown=False, complete=False):
    name, description = text(title), text('DESCRIPTION')
    descriptors = descriptor(0x4d, b'jpn' + bytes([len(name)]) + name + bytes([len(description)]) + description)
    if complete:
        head, value = text('HEAD'), text('DETAIL')
        items = bytes([len(head)]) + head + bytes([len(value)]) + value
        descriptors += descriptor(0x4e, b'\x00jpn' + bytes([len(items)]) + items + b'\x00')
        descriptors += descriptor(0x54, b'\x00\x00')
        descriptors += descriptor(0xc4, b'\xf2\x03\x10\x0f\x00\x40jpn')
    body = struct.pack('>HHBBH', 1, 4, number, 0x4e, event_id) + clock(start)
    body += (b'\xff\xff\xff' if unknown else b'\x00\x01\x00') + struct.pack('>H', 0x8000 | len(descriptors)) + descriptors
    return syntax(0x4e, 101, number, body)


def archive(rows):
    dictionary, ids, data, times, codes = [bytearray() for _ in range(5)]
    for index, (seconds, pid, section) in enumerate(rows):
        dictionary.extend(struct.pack('<H', len(section) - 1))
        ids.extend(struct.pack('<H', pid))
        data.extend(section)
        times.extend(struct.pack('<IHH', 0x80000000 + round(seconds * 11250), 0, 0))
        codes.extend(struct.pack('<H', 4096 + index))
    size = len(ids) + len(data)
    header = bytearray(32)
    header[:8] = b'Pssc\r\n\x9a\n'
    struct.pack_into('<HHHIII', header, 10, len(rows) * 2, len(rows), len(rows), size, size, len(rows))
    trailer = 4 - (len(dictionary) + (size + 1) // 2 * 2 + len(codes)) % 4
    return bytes(header + times + dictionary + ids + data + bytes(size % 2) + codes + bytes(trailer))


start = datetime(2026, 10, 7, 10)
pat = syntax(0, 1, 0, struct.pack('>HH', 101, 0xe100))
name = text('ARCHIVE')
service = descriptor(0x48, bytes([1, 0, len(name)]) + name)
sdt = syntax(0x42, 1, 0, b'\x00\x04\xff' + struct.pack('>HBH', 101, 0xff, 0x8000 | len(service)) + service)
root = Path(__file__).resolve().parents[1] / 'internal/metadata/testdata/psc'
root.mkdir(parents=True, exist_ok=True)
cases = []
for case in ['present', 'following-margin', 'unknown-duration-next', 'no-eit', 'late-sdt-only', 'partial-trailer', 'complete-stops-before-next', 'unknown-next-after-initial-stop', 'unknown-last-valid', 'no-tot', 'round-tot-ticks', 'empty']:
    rows = [(0, 0, pat), (0, 0x11, sdt), (0, 0x12, eit(0, 9, start - timedelta(minutes=1), 'EARLY-IGNORE')),
            (5, 0x14, tot(start + timedelta(seconds=5)))]
    if case == 'late-sdt-only':
        rows = [row for row in rows if row[1] != 0x11]
    current_start = start - timedelta(minutes=1) if case == 'following-margin' else start
    following_start = start if case == 'following-margin' else start + timedelta(minutes=2)
    complete = case in ('complete-stops-before-next', 'unknown-next-after-initial-stop', 'unknown-last-valid')
    unknown = case in ('unknown-duration-next', 'unknown-next-after-initial-stop', 'unknown-last-valid')
    if case != 'no-eit':
        rows += [(14, 0x12, eit(0, 10, current_start, 'PRESENT', unknown, complete)),
                 (14, 0x12, eit(1, 11, following_start, 'FOLLOWING', complete=complete))]
    if case in ('unknown-next-after-initial-stop', 'unknown-last-valid'):
        for n in range(110):
            rows += [(15 + n / 3, 0x12, eit(0, 10, current_start, 'PRESENT', True, True)),
                     (15 + n / 3, 0x12, eit(1, 11, following_start, 'FOLLOWING', complete=True))]
    rows += [(60, 0x14, tot(start + timedelta(seconds=60)))]
    if case in ('unknown-duration-next', 'complete-stops-before-next', 'unknown-next-after-initial-stop'):
        rows += [(63, 0x12, eit(0, 12, start + timedelta(seconds=61), 'NEXT-PRESENT', complete=complete))]
    if case == 'unknown-last-valid':
        rows += [(63, 0x12, eit(0, 10, current_start, 'PRESENT', complete=True))]
    rows += [(65, 0x11, sdt)]
    if case == 'no-tot':
        rows = [row for row in rows if row[1] != 0x14]
    if case == 'round-tot-ticks':
        rows = [(seconds + (1 / 11250 if pid == 0x14 else 0), pid, section) for seconds, pid, section in rows]
    if case == 'empty':
        rows = []
    data = archive(rows)
    if case == 'empty':
        data = b''
    if case == 'partial-trailer':
        data = data[:-1]
    (root / f'{case}.psc').write_bytes(data)
    with tempfile.TemporaryDirectory() as temporary:
        media = Path(temporary) / 'movie.mp4'
        media.with_suffix('.psc').write_bytes(data)
        video = schemas.RecordedVideo.model_construct(file_path=str(media), file_size=9999999, duration=65, container_format='MPEG-4', recording_start_time=None, recording_end_time=None)
        virtual_packets.clear()
        analyzer = analyzer_class(video)
        recording_time = analyzer.analyzeRecordingTime()
        if recording_time is not None:
            video.recording_start_time, video.recording_end_time = recording_time
        program = analyzer.analyze()
        cases.append(dict(name=case, duration=65, virtual_ts_sha256=hashlib.sha256(virtual_packets[0]).hexdigest(),
                          recording_start=recording_time[0].isoformat() if recording_time else None,
                          recording_end=recording_time[1].isoformat() if recording_time else None,
                          title=program.title if program else None, start=program.start_time.isoformat() if program else None,
                          end=program.end_time.isoformat() if program else None, program_duration=program.duration if program else None,
                          event_id=program.event_id if program else None,
                          channel=program.channel.id if program else None))
(root / 'reference.json').write_text(json.dumps(dict(source_sha256=digest, cases=cases), indent=2) + '\n', encoding='utf-8')
print(root)
