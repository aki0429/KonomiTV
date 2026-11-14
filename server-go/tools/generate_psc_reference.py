from __future__ import annotations

import argparse
import ast
import base64
import hashlib
import io
import json
import struct
from pathlib import Path
from typing import cast


parser = argparse.ArgumentParser()
parser.add_argument('reference_source', type=Path)
args = parser.parse_args()
source = args.reference_source.read_text(encoding='utf-8')
digest = hashlib.sha256(source.encode()).hexdigest()
assert digest == 'cc31837b5263266901b20e5f4da3f5a39f8e430835a6af0819542e8da99ed15f'
original = next(n for n in ast.parse(source).body if isinstance(n, ast.ClassDef) and n.name == 'TSInfoAnalyzer')
method = next(n for n in original.body if isinstance(n, ast.FunctionDef) and n.name == 'readPSIData')
selected = ast.ClassDef(name='TSInfoAnalyzer', bases=[], keywords=[], body=[method], decorator_list=[], type_params=[])
module = ast.Module(body=[ast.ImportFrom(module='__future__', names=[ast.alias(name='annotations')], level=0), selected], type_ignores=[])
namespace = dict(cast=cast)
exec(compile(ast.fix_missing_locations(module), str(args.reference_source), 'exec'), namespace)
read = namespace['TSInfoAnalyzer'].readPSIData


def block(entries, times, window=None):
    ids, data, codes, clock = bytearray(), bytearray(), bytearray(), bytearray()
    dictionary = bytearray()
    for entry in entries:
        if isinstance(entry, int):
            dictionary.extend(struct.pack('<H', 4096 + entry))
        else:
            pid, section = entry
            dictionary.extend(struct.pack('<H', len(section) - 1))
            ids.extend(struct.pack('<H', pid))
            data.extend(section)
    for item in times:
        if isinstance(item, int):
            clock.extend(struct.pack('<I', item))
        else:
            delta, indexes = item
            clock.extend(struct.pack('<HH', delta, len(indexes) - 1))
            codes.extend(struct.pack('<' + 'H' * len(indexes), *[4096 + n for n in indexes]))
    size = len(ids) + len(data)
    header = bytearray(32)
    header[:8] = b'Pssc\r\n\x9a\n'
    struct.pack_into('<HHHIII', header, 10, len(times), len(entries), window if window is not None else len(entries), size, size, len(codes) // 2)
    trailer = 4 - (len(dictionary) + (size + 1) // 2 * 2 + len(codes)) % 4
    return bytes(header + clock + dictionary + ids + data + bytes(size % 2) + codes + bytes(trailer))


first = block([(0x11, b'SDT'), (0x14, b'TOT'), (0x12, b'EIT'), (0x200, b'ignored')], [0x800003e8, (0, [0, 1]), (11250, [2, 3])])
second = block([1, (0x12, b'EIT2')], [0x80002fda, (10, [0, 1, 2])], window=3)
wrap = block([(0x14, b'WRAP')], [0xbfffff00, (500, [0]), 0xffffffff, (20, [0]), 0x80000100, (0, [0])])
cases = [
    ('dictionary-carry-and-reuse', first + second, [0x11, 0x12, 0x14], 0),
    ('filter-empty', first, [], 0),
    ('clock-wrap-and-reset', wrap, [0x14], 0),
    ('callback-stop', first, [0x11, 0x14], 1),
    ('bad-reference', block([9], [0x80000000, (0, [0])]), [0x14], 0),
    ('duplicate-reference', first + block([0, 0], [0x80000000, (0, [0])]), [0x11], 0),
    ('bad-code', block([(0x14, b'A')], [0x80000000, (0, [7])]), [0x14], 0),
    ('truncated-data', first[:55], [0x11], 0),
    ('truncated-trailer', first[:-1], [0x11, 0x12, 0x14], 0),
    ('header-only', first[:32], [0x11], 0),
    ('empty', b'', [0x11], 0),
    ('short-header-is-end', b'Pssc', [0x11], 0),
    ('unknown-header-is-end', bytes(32), [0x11], 0),
]
for label, offset, value in [('bad-window', 14, 0), ('large-window', 14, 65535), ('bad-buffer-size', 20, 0)]:
    data = bytearray(first)
    struct.pack_into('<H' if offset == 14 else '<I', data, offset, value)
    cases.append((label, bytes(data), [0x11], 0))
result = []
for name, data, pids, stop_after in cases:
    sections = []

    def callback(seconds, pid, section):
        sections.append(dict(time=seconds, pid=pid, data=base64.b64encode(section).decode()))
        return not stop_after or len(sections) < stop_after

    valid = read(io.BytesIO(data), pids, callback)
    result.append(dict(name=name, input=base64.b64encode(data).decode(), pids=pids, stop_after=stop_after, valid=valid, sections=sections))
output = Path(__file__).resolve().parents[1] / 'internal/tsinfo/psc/testdata/reference.json'
output.parent.mkdir(parents=True, exist_ok=True)
output.write_text(json.dumps(dict(source_sha256=digest, cases=result), indent=2) + '\n', encoding='utf-8')
print(output)
