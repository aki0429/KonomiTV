from __future__ import annotations

import argparse
import ast
import base64
import hashlib
import json
import logging
import time
from pathlib import Path
from typing import cast

import av
import cv2
import numpy as np
from numpy.typing import NDArray


parser = argparse.ArgumentParser()
parser.add_argument('reference_source', type=Path)
args = parser.parse_args()
source = args.reference_source.read_text(encoding='utf-8')
source_sha256 = hashlib.sha256(source.encode()).hexdigest()
assert source_sha256 == 'f5fd79e96a1e3a997fcdd41dfc5409a61afa1de6b5a53ff4259777dd0af203b5'
original = next(n for n in ast.parse(source).body if isinstance(n, ast.ClassDef) and n.name == 'ThumbnailGenerator')
method = next(n for n in original.body if isinstance(n, ast.FunctionDef) and n.name == '__extractAndScoreFrames')
selected = ast.ClassDef(name='ThumbnailGenerator', bases=[], keywords=[], body=[method], decorator_list=[], type_params=[])
module = ast.Module(body=[ast.ImportFrom(module='__future__', names=[ast.alias(name='annotations')], level=0), selected], type_ignores=[])
namespace = dict(av=av, cv2=cv2, np=np, logging=logging, time=time, cast=cast, NDArray=NDArray)
exec(compile(ast.fix_missing_locations(module), str(args.reference_source), 'exec'), namespace)
generator_class = namespace['ThumbnailGenerator']
setattr(generator_class, '_ThumbnailGenerator__scoreFrames', lambda self, frames: None)
root = Path(__file__).resolve().parents[1]
fixture = root / 'internal/metadata/testdata/thumbnail_tsreadex_input.mpegts'
offsets = [0, 2, 5, 6, 8, 11, 20]
selected_pts = []
open_count = 0
original_open = av.open


class FrameProxy:
    def __init__(self, frame):
        self.frame = frame
        self.pict_type = frame.pict_type

    def to_ndarray(self, **kwargs):
        selected_pts.append(self.frame.pts)
        return self.frame.to_ndarray(**kwargs)


class PacketProxy:
    def __init__(self, packet):
        self.packet = packet

    def decode(self):
        return [FrameProxy(f) for f in self.packet.decode()]


class ContainerProxy:
    def __init__(self, container):
        self.container = container
        self.streams = container.streams

    def seek(self, *args, **kwargs):
        return self.container.seek(*args, **kwargs)

    def demux(self, *args):
        return (PacketProxy(p) for p in self.container.demux(*args))

    def close(self):
        self.container.close()


def observed_open(*args, **kwargs):
    global open_count
    open_count += 1
    return ContainerProxy(original_open(*args, **kwargs))


av.open = observed_open
generator = generator_class()
generator.file_path = fixture
generator.container_format = 'MPEG-TS'
generator.SCORING_SCALE = (480, 270)
generator.FRAME_EXTRACTION_MAX_DEMUX_PACKETS = 20000
generator.FRAME_EXTRACTION_MAX_CONSECUTIVE_FAILURES = 10
result = generator._ThumbnailGenerator__extractAndScoreFrames(offsets)
av.open = original_open
assert result is not None
frames, _ = result
media = dict(offsets=offsets, selected_pts=selected_pts, open_count=open_count,
             rgb_sha256=[hashlib.sha256(cv2.cvtColor(f, cv2.COLOR_BGR2RGB).tobytes()).hexdigest() for f in frames])
selected_pts.clear()
open_count = 0
av.open = observed_open
generator.file_path = root / 'internal/frameextract/testdata/seek_input.mp4'
generator.container_format = 'MPEG-4'
mp4_offsets = [0, 0.01, 0.9, 1, 1.01, 2.9, 3.9, 4, 10]
result = generator._ThumbnailGenerator__extractAndScoreFrames(mp4_offsets)
av.open = original_open
assert result is not None
mp4 = dict(offsets=mp4_offsets, selected_pts=list(selected_pts), open_count=open_count,
           rgb_sha256=[hashlib.sha256(cv2.cvtColor(f, cv2.COLOR_BGR2RGB).tobytes()).hexdigest() for f in result[0]])
resize = []
for w, h, out_w, out_h in [(8, 6, 4, 3), (9, 6, 3, 2), (13, 7, 8, 5), (8, 5, 13, 7), (13, 5, 8, 7), (1, 1, 3, 4), (17, 9, 8, 4)]:
    pixels = ((np.arange(w * h * 3, dtype=np.uint32) * 73 + 19) % 256).astype(np.uint8).reshape(h, w, 3)
    output = cv2.resize(pixels, (out_w, out_h), interpolation=cv2.INTER_AREA)
    resize.append(dict(width=w, height=h, output_width=out_w, output_height=out_h,
                       pixels=base64.b64encode(pixels.tobytes()).decode(), expected=base64.b64encode(output.tobytes()).decode()))
output = root / 'internal/frameextract/testdata/seek_reference.json'
output.parent.mkdir(parents=True, exist_ok=True)
output.write_text(json.dumps(dict(source_sha256=source_sha256, av=av.__version__, cv2=cv2.__version__, media=media, mp4=mp4, resize=resize), indent=2) + '\n', encoding='utf-8')
print(output)
