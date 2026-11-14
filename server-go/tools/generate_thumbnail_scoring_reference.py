from __future__ import annotations

import argparse
import ast
import hashlib
import json
import logging
import math
import pathlib
import random
import time
from importlib.metadata import version

import cv2
import numpy as np


ROOT = pathlib.Path(__file__).resolve().parents[2]
parser = argparse.ArgumentParser(
    description='Generate upstream thumbnail scoring reference fixtures'
)
parser.add_argument('reference_source', type=pathlib.Path)
parser.add_argument('astronaut_image', type=pathlib.Path)
args = parser.parse_args()
SOURCE = args.reference_source
OUT = ROOT / 'server-go/internal/metadata/testdata/thumbnail_scoring'
DATA = ROOT / 'server-go/internal/metadata/data'
assert cv2.__version__ == '5.0.0'
assert version('opencv-contrib-python-headless') == '5.0.0.93'
assert np.__version__ == '2.5.4'
source_text = SOURCE.read_text(encoding='utf-8')
source_sha256 = hashlib.sha256(source_text.encode('utf-8')).hexdigest()
assert source_sha256 == 'f5fd79e96a1e3a997fcdd41dfc5409a61afa1de6b5a53ff4259777dd0af203b5'
cv2.setNumThreads(1)
tree = ast.parse(source_text)
cls = next(
    n
    for n in tree.body
    if isinstance(n, ast.ClassDef) and n.name == 'ThumbnailGenerator'
)
env = {
    'cv2': cv2,
    'np': np,
    'math': math,
    'pathlib': pathlib,
    'STATIC_DIR': DATA,
    'logging': logging,
    'time': time,
    'random': random,
}
module = ast.Module(
    body=[
        ast.ImportFrom(
            module='__future__', names=[ast.alias(name='annotations')], level=0
        ),
        cls,
    ],
    type_ignores=[],
)
exec(compile(ast.fix_missing_locations(module), str(SOURCE), 'exec'), env)
generator = env['ThumbnailGenerator'].__new__(env['ThumbnailGenerator'])
human = cv2.CascadeClassifier(str(DATA / 'haarcascade_frontalface_default.xml'))
anime = cv2.CascadeClassifier(str(DATA / 'lbpcascade_animeface.xml'))
assert not human.empty() and not anime.empty()
OUT.mkdir(parents=True, exist_ok=True)

images = {}


def add(name, value):
    images[name] = np.ascontiguousarray(value, dtype=np.uint8)


h, w = 270, 480
yy, xx = np.mgrid[:h, :w]
for value in [0, 20, 45, 127, 225, 255]:
    add(f'solid-{value}', np.full((h, w, 3), value, np.uint8))
add('solid-color', np.full((h, w, 3), [30, 150, 200], np.uint8))
add('gradient', np.repeat((xx * 255 // (w - 1)).astype(np.uint8)[..., None], 3, axis=2))
add(
    'color-gradient',
    np.stack([xx * 255 // (w - 1), yy * 255 // (h - 1), (xx + yy) % 256], axis=2),
)
add(
    'checkerboard',
    np.repeat(
        (((xx // 12 + yy // 12) % 2) * 255).astype(np.uint8)[..., None], 3, axis=2
    ),
)
add('noise', np.random.default_rng(7281).integers(0, 256, (h, w, 3), dtype=np.uint8))
add('dim-noise', np.random.default_rng(7282).integers(0, 46, (h, w, 3), dtype=np.uint8))
add(
    'three-colors',
    np.array(
        [
            [
                (
                    [10, 40, 220]
                    if x < 160
                    else [210, 170, 10]
                    if x < 320
                    else [90, 200, 70]
                )
                for x in range(w)
            ]
            for _ in range(h)
        ],
        np.uint8,
    ),
)
for thickness in [8, 16, 32, 50, 67]:
    value = images['color-gradient'].copy()
    value[:thickness] = 0
    value[-thickness:] = 0
    add(f'letterbox-{thickness}', value)
for edges in [1, 2, 3, 4]:
    value = images['noise'].copy()
    value[:22] = 255
    if edges >= 2:
        value[-22:] = 255
    if edges >= 3:
        value[:, :39] = 255
    if edges >= 4:
        value[:, -39:] = 255
    add(f'white-border-{edges}', value)
astronaut = cv2.imread(str(args.astronaut_image))
assert astronaut is not None
add('astronaut', cv2.resize(astronaut, (w, h)))
face = astronaut[50:195, 160:310]
for size in [60, 100, 160, 235]:
    value = np.full((h, w, 3), 120, np.uint8)
    scaled = cv2.resize(face, (size, size))
    value[10 : 10 + size, 100 : 100 + size] = scaled
    add(f'face-{size}', value)
    add(f'posterized-face-{size}', (value // 40) * 40)

report = {
    'upstream_revision': '687b904bb0a2cbb329596fd7b0368c09afe19a6c',
    'source_sha256': source_sha256,
    'opencv': cv2.__version__,
    'numpy': np.__version__,
    'models': {
        p.name: hashlib.sha256(p.read_bytes()).hexdigest()
        for p in sorted(DATA.glob('*.xml'))
    },
    'images': [],
}
for name, bgr in images.items():
    cv2.imwrite(str(OUT / f'{name}.png'), bgr)
    gray = cv2.cvtColor(bgr, cv2.COLOR_BGR2GRAY)
    letterbox = generator._ThumbnailGenerator__detectLetterbox(bgr)
    score_by_mode = {}
    for mode in [None, 'Human', 'Anime']:
        generator.face_detection_mode = mode
        scores = []
        for seed in [1, 2, 3]:
            cv2.setRNGSeed(seed)
            score, found = generator._ThumbnailGenerator__computeImageScore(
                bgr,
                human if mode == 'Human' else anime if mode == 'Anime' else None,
                human if mode == 'Anime' else None,
                1,
                1,
            )
            scores.append(float(score))
        score_by_mode[mode or 'None'] = {
            'min': min(scores),
            'max': max(scores),
            'found_face': bool(found),
        }
    faces = {}
    for mode, detector, factor, neighbors in [
        ('Human', human, 1.05, 3),
        ('Anime', anime, 1.01, 2),
        ('Auxiliary', human, 1.10, 5),
    ]:
        result = detector.detectMultiScale(
            gray, scaleFactor=factor, minNeighbors=neighbors
        )
        faces[mode] = sorted([list(map(int, r)) for r in result])
    report['images'].append(
        {
            'name': name,
            'gray_sha256': hashlib.sha256(gray.tobytes()).hexdigest(),
            'canny50_sha256': hashlib.sha256(
                cv2.Canny(gray, 50, 150).tobytes()
            ).hexdigest(),
            'canny100_sha256': hashlib.sha256(
                cv2.Canny(gray, 100, 200).tobytes()
            ).hexdigest(),
            'letterbox': None
            if letterbox is None
            else [
                letterbox[1].start,
                letterbox[0].start,
                letterbox[1].stop,
                letterbox[0].stop,
            ],
            'scores': score_by_mode,
            'faces': faces,
        }
    )
    print(name, score_by_mode, faces)


class FixedFaces:
    def __init__(self, faces):
        self.faces = np.array(faces, dtype=np.int32).reshape(-1, 4)

    def detectMultiScale(self, *args, **kwargs):
        return self.faces


report['face_boost_cases'] = []
bgr = images['noise']
for mode in ['Human', 'Anime']:
    generator.face_detection_mode = mode
    cv2.setRNGSeed(1)
    base, _ = generator._ThumbnailGenerator__computeImageScore(
        bgr, FixedFaces([]), FixedFaces([]), 1, 1
    )
    for size in [24, 100, 172, 173, 200, 269]:
        for suppress in [False, True]:
            faces = [[1, 1, size, size], [20, 20, 20, 20]]
            auxiliary = [[0, 0, 24, 24]] if suppress else []
            cv2.setRNGSeed(1)
            score, found = generator._ThumbnailGenerator__computeImageScore(
                bgr, FixedFaces(faces), FixedFaces(auxiliary), 1, 1
            )
            report['face_boost_cases'].append(
                {
                    'mode': mode,
                    'image_area': h * w,
                    'faces': faces,
                    'auxiliary': auxiliary,
                    'boost': float(score - base),
                    'found_face': bool(found),
                }
            )
(OUT / 'reference.json').write_text(
    json.dumps(report, indent=2, ensure_ascii=False) + '\n', encoding='utf-8'
)
