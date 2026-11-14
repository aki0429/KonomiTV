from __future__ import annotations

import argparse
import ast
import hashlib
import io
import json
import logging
from pathlib import Path
from types import SimpleNamespace
from typing import Any, cast

import cv2
import numpy as np
from numpy.typing import NDArray


parser = argparse.ArgumentParser(
    description='Run pinned upstream sequential thumbnail extraction with controlled frames'
)
parser.add_argument('reference_source', type=Path)
parser.add_argument('--normalized-media', type=Path)
parser.add_argument('--media-input', type=Path)
args = parser.parse_args()
source = args.reference_source.read_text(encoding='utf-8')
source_sha256 = hashlib.sha256(source.encode()).hexdigest()
assert (
    source_sha256 == 'f5fd79e96a1e3a997fcdd41dfc5409a61afa1de6b5a53ff4259777dd0af203b5'
)
tree = ast.parse(source)
original = next(
    n
    for n in tree.body
    if isinstance(n, ast.ClassDef) and n.name == 'ThumbnailGenerator'
)
method = next(
    n
    for n in original.body
    if isinstance(n, ast.FunctionDef)
    and n.name == '__extractAndScoreFramesWithTSReadEx'
)
selected = ast.ClassDef(
    name='ThumbnailGenerator',
    bases=[],
    keywords=[],
    body=[method],
    decorator_list=[],
    type_params=[],
)
module = ast.Module(
    body=[
        ast.ImportFrom(
            module='__future__', names=[ast.alias(name='annotations')], level=0
        ),
        selected,
    ],
    type_ignores=[],
)


def frame(seconds, label, kind='I', error=False):
    return {'time': seconds, 'label': label, 'kind': kind, 'decode_error': error}


cases = [
    {
        'name': 'exact-and-tail',
        'offsets': [0, 1, 2, 3, 10],
        'frames': [frame(50, 10), frame(51, 11), frame(52, 12), frame(53, 13)],
    },
    {
        'name': 'millisecond-boundaries',
        'offsets': [0, 0.9989, 0.999, 1, 1.0009, 1.0011, 2.0011],
        'frames': [
            frame(10000, 20),
            frame(10001, 21),
            frame(10002, 22),
            frame(10003, 23),
        ],
    },
    {
        'name': 'single-frame-tail',
        'offsets': [0, 1, 10, 600],
        'frames': [frame(100, 30)],
    },
    {'name': 'no-frames', 'offsets': [0, 1, 2], 'frames': []},
    {
        'name': 'missing-time',
        'offsets': [0, 1, 2, 10],
        'frames': [frame(None, 40), frame(80, 41), frame(None, 42), frame(81, 43)],
    },
    {
        'name': 'p-frame-despite-skip',
        'offsets': [0, 1, 2, 10],
        'frames': [
            frame(5, 50, 'P'),
            frame(6, 51),
            frame(7, 52, 'P'),
            frame(8, 53),
            frame(9, 54),
        ],
    },
    {
        'name': 'damaged-packet',
        'offsets': [0, 1, 2, 10],
        'frames': [
            frame(50, 60),
            frame(51, 61, error=True),
            frame(52, 62),
            frame(53, 63),
        ],
    },
    {
        'name': 'duplicate-and-backward-pts',
        'offsets': [0, 1, 2, 3, 10],
        'frames': [
            frame(100, 70),
            frame(101, 71),
            frame(101, 72),
            frame(100.5, 73),
            frame(104, 74),
        ],
    },
    {
        'name': 'nonmonotone-offsets',
        'offsets': [0, 3, 1, 3.5, 20],
        'frames': [frame(100, 80), frame(102, 81), frame(104, 82)],
    },
    {
        'name': 'first-tolerance',
        'offsets': [-1, 0, 0.0005, 0.001, 0.00101],
        'frames': [frame(0, 90), frame(0.5, 91)],
    },
    {
        'name': 'service-zero',
        'service_id': 0,
        'offsets': [0, 1, 10],
        'frames': [frame(100, 100), frame(101, 101)],
    },
    {
        'name': 'service-specific',
        'service_id': 101,
        'offsets': [0, 1, 10],
        'frames': [frame(-3, 110), frame(-2, 111)],
    },
    {'name': 'no-video-stream', 'no_video': True, 'offsets': [0, 1], 'frames': []},
    {'name': 'timeout', 'timeout': True, 'offsets': [0, 1], 'frames': [frame(0, 120)]},
]

report = {
    'upstream_revision': '687b904bb0a2cbb329596fd7b0368c09afe19a6c',
    'source_sha256': source_sha256,
    'cases': [],
}
for case in cases:
    events = []
    popen_args = []

    class DecodeError(Exception):
        pass

    class Frame:
        def __init__(self, item):
            self.time = item['time']
            self.pict_type = item['kind']
            self.label = item['label']

        def to_ndarray(self, format):
            assert format == 'rgb24'
            return np.full((4, 4, 3), self.label, dtype=np.uint8)

    class Packet:
        def __init__(self, item):
            self.item = item

        def decode(self):
            if self.item['decode_error']:
                raise DecodeError('damaged packet')
            return [Frame(self.item)]

    codec = SimpleNamespace(skip_frame=None)

    class Container:
        streams = SimpleNamespace(
            video=[] if case.get('no_video') else [SimpleNamespace(codec_context=codec)]
        )

        def demux(self, stream):
            return iter(Packet(item) for item in case['frames'])

        def close(self):
            events.append('container_close')

    class Pipe(io.BytesIO):
        def close(self):
            if not self.closed:
                events.append('pipe_close')
            super().close()

    class Process:
        def __init__(self, options, **kwargs):
            popen_args.extend(options)
            self.stdout = Pipe(b'transport stream')
            self.returncode = None

        def kill(self):
            events.append('process_kill')
            self.returncode = -9

        def wait(self, timeout):
            assert timeout == 5
            events.append('process_wait')
            return self.returncode

    ticks = iter([0.0, 601.0] if case.get('timeout') else [0.0] * 100)
    env = {
        'np': np,
        'cv2': cv2,
        'Any': Any,
        'cast': cast,
        'NDArray': NDArray,
        'logging': logging,
        'time': SimpleNamespace(time=lambda: next(ticks)),
        'LIBRARY_PATH': {'tsreadex': 'fake-tsreadex'},
        'av': SimpleNamespace(
            open=lambda *a, **kw: Container(),
            error=SimpleNamespace(
                FFmpegError=DecodeError, InvalidDataError=DecodeError
            ),
            video=SimpleNamespace(
                frame=SimpleNamespace(PictureType=SimpleNamespace(I='I'))
            ),
        ),
        'subprocess': SimpleNamespace(
            Popen=Process, PIPE=-1, DEVNULL=-3, TimeoutExpired=TimeoutError
        ),
    }
    exec(
        compile(ast.fix_missing_locations(module), str(args.reference_source), 'exec'),
        env,
    )
    generator = env['ThumbnailGenerator']()
    generator.file_path = Path('recording.ts')
    generator.service_id = case.get('service_id')
    generator.SCORING_SCALE = (480, 270)
    generator.TSREADEX_FRAME_EXTRACTION_TIMEOUT = 600
    generator._ThumbnailGenerator__scoreFrames = lambda frames: 0 if frames else None
    result = generator._ThumbnailGenerator__extractAndScoreFramesWithTSReadEx(
        case['offsets']
    )
    report['cases'].append(
        {
            **case,
            'expected': None
            if result is None
            else [int(img[0, 0, 0]) for img in result[0]],
            'tsreadex_args': popen_args[1:],
            'cleanup': events,
            'skip_frame': codec.skip_frame,
        }
    )

out = (
    Path(__file__).resolve().parents[1]
    / 'internal/metadata/testdata/thumbnail_tsreadex_reference.json'
)
out.write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
print(f'{len(cases)} source-generated sequential extraction cases: {out}')

if args.normalized_media:
    import time
    from importlib.metadata import version

    import av

    assert version('av') == '19.0.2'
    assert version('opencv-contrib-python-headless') == '5.0.0.93'
    assert version('numpy') == '2.5.4'

    class MediaPipe:
        # 実 tsreadex で作成した同じバイト列を、PyAV に非シーク可能な入力として渡す。
        def __init__(self):
            self.file = args.normalized_media.open('rb')

        def read(self, size):
            return self.file.read(size)

        def close(self):
            self.file.close()

        def seekable(self):
            return False

    class MediaProcess:
        def __init__(self, *a, **kw):
            self.stdout = MediaPipe()
            self.returncode = 0

        def wait(self, timeout):
            return 0

        def kill(self):
            pass

    env['av'] = av
    env['time'] = time
    env['subprocess'] = SimpleNamespace(
        Popen=MediaProcess, PIPE=-1, DEVNULL=-3, TimeoutExpired=TimeoutError
    )
    generator = env['ThumbnailGenerator']()
    generator.file_path = args.media_input or args.normalized_media
    generator.service_id = 101
    generator.SCORING_SCALE = (480, 270)
    generator.TSREADEX_FRAME_EXTRACTION_TIMEOUT = 600
    generator._ThumbnailGenerator__scoreFrames = lambda frames: 0
    offsets = [0, 2, 5, 6, 8, 11, 20]
    result = generator._ThumbnailGenerator__extractAndScoreFramesWithTSReadEx(offsets)
    assert result is not None
    colors = [list(map(int, img[100, 100])) for img in result[0]]
    media = {
        'upstream_revision': report['upstream_revision'],
        'source_sha256': source_sha256,
        'av': av.__version__,
        'opencv': cv2.__version__,
        'numpy': np.__version__,
        'normalized_sha256': hashlib.sha256(
            args.normalized_media.read_bytes()
        ).hexdigest(),
        'input_sha256': hashlib.sha256(args.media_input.read_bytes()).hexdigest()
        if args.media_input
        else None,
        'offsets': offsets,
        'pixel_bgr': colors,
        'expected_colors': ['blue' if b > r else 'red' for b, g, r in colors],
    }
    media_out = out.with_name('thumbnail_tsreadex_media_reference.json')
    media_out.write_text(json.dumps(media, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(media))
