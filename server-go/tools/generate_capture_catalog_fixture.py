"""固定した本家 CaptureScanTask の実装から画像・カーソルの期待値を生成する。"""

import ast
import base64
from copy import deepcopy
from dataclasses import dataclass
from datetime import date, datetime, timedelta, timezone
import hashlib
import io
import json
from pathlib import Path
import re
import struct
import subprocess
from typing import Annotated, Any, Literal, Self

from PIL import Image, UnidentifiedImageError
from pydantic import AliasChoices, BaseModel, Field, TypeAdapter, ValidationError, field_validator, model_validator

ROOT = Path(__file__).resolve().parents[2]
REF = "687b904bb0a2cbb329596fd7b0368c09afe19a6c"
JST = timezone(timedelta(hours=9))
source = subprocess.check_output(["git", "-C", str(ROOT), "show", f"{REF}:server/app/metadata/CaptureScanTask.py"]).decode("utf-8")
tree = ast.parse(source)
nodes = [node for node in tree.body if getattr(node, "name", "") in {"CaptureExif", "CaptureImageInfo", "ReadCaptureImage"}]
exec(compile(ast.Module(body=nodes, type_ignores=[]), "upstream/CaptureScanTask.py", "exec"), globals())

BASE = dict(captured_at="2026-11-14T10:00:10.123456+09:00", captured_playback_position=10.123456,
            network_id=4, service_id=101, event_id=123, start_time="2026-11-14T10:00:00+09:00",
            end_time="2026-11-14T11:00:00+09:00", duration=3600, title="番組100%_", description="日本語", caption_text="字幕")
modified = datetime(2026, 11, 14, 15, 0, tzinfo=JST)
cases = []

def add(name, edits=None, *, metadata=True, filename="image.jpg", fmt="JPEG", comment=None, truncate=0, original=None):
    meta = deepcopy(BASE) if metadata else None
    if edits is not None:
        edits(meta)
    exif = Image.Exif()
    if meta is not None:
        exif[40092] = json.dumps(meta, ensure_ascii=False).encode("utf-16-le") if comment is None else comment
    if original is not None:
        exif[36867] = original
    output = io.BytesIO()
    Image.new("RGB", (16, 16), (20, 60, 100)).save(output, fmt, exif=exif)
    data = output.getvalue()
    if truncate:
        data = data[:-truncate]
    case = dict(name=name, filename=filename, data=base64.b64encode(data).decode(), modified_at=modified.isoformat())
    try:
        value = ReadCaptureImage(data, filename, modified)
        case.update(file_hash=value.file_hash.hex(), image_width=value.image_width, image_height=value.image_height,
                    captured_at=value.captured_at.isoformat(), metadata_status=value.metadata_status,
                    metadata=value.metadata.model_dump(mode="json") if value.metadata else None, invalid=False)
    except (UnidentifiedImageError, SyntaxError, ValueError, Image.DecompressionBombError, OSError):
        case["invalid"] = True
    cases.append(case)

add("current")
add("png", fmt="PNG", filename="image.png")
add("legacy", lambda m: (m.update(capture_time=m.pop("captured_at")), m.pop("captured_playback_position")))
add("legacy_mixed_timezone", lambda m: (m.update(capture_time=m.pop("captured_at"), start_time="2026-11-14T10:00:00"), m.pop("captured_playback_position")))
add("legacy_current_alias", lambda m: (m.update(capture_time="2026-11-14T10:00:05+09:00"), m.pop("captured_playback_position")))
add("unknown_duration", lambda m: m.update(duration=None))
add("unknown_ids", lambda m: m.update(network_id=-1, service_id=-1, event_id=-1))
add("coerced_numbers", lambda m: m.update(network_id="4.0", duration="3600.0", captured_playback_position=True))
add("naive_times", lambda m: m.update(captured_at="2026-11-14T10:00:10", start_time="2026-11-14T10:00:00"))
add("utc_times", lambda m: m.update(captured_at="2026-11-14T01:00:10Z", start_time="2026-11-14T01:00:00Z"))
for name, edit in {
    "invalid_id": lambda m: m.update(network_id=65536),
    "negative_id": lambda m: m.update(event_id=-2),
    "fractional_id": lambda m: m.update(service_id=4.5),
    "missing_position": lambda m: m.pop("captured_playback_position"),
    "missing_end": lambda m: m.pop("end_time"),
    "missing_duration": lambda m: m.pop("duration"),
    "null_title": lambda m: m.update(title=None),
    "nonfinite_position": lambda m: m.update(captured_playback_position="NaN"),
    "overflow_position": lambda m: m.update(captured_playback_position=1e100),
}.items():
    add(name, edit)
add("invalid_utf16", comment=b"\x00\xd8")
add("wrong_comment_type", comment="not bytes")
add("no_metadata", metadata=False)
add("filename", metadata=False, filename="Capture_20261113-235959-copy.jpg")
add("twelve_hour_filename", metadata=False, filename="Capture_20261114-030000.jpg")
add("original_time", metadata=False, original="2026:11:12 20:00:00")
add("truncated_jpeg", truncate=40)
add("truncated_png", fmt="PNG", filename="image.png", truncate=20)

signatures = []
for values in [["Programs", "日本😀\\\"", False, "BroadcastAtDesc", 12], ["CapturedAtAsc", "2026-11-14", 4, {"query": "日本", "tag_ids": [1, 2], "is_favorite": True}, None]]:
    signatures.append(dict(values=values, signature=hashlib.sha256(json.dumps(values, sort_keys=True, default=str).encode()).hexdigest()))
target = ROOT / "server-go/internal/api/testdata/capture_catalog_reference.json"
date_cases = []
for raw in ["2026-11-14", "2026-11-14T00:00:00Z", "2026-11-14T00:00:00+09:00", "2026-11-14T00:00:00-05:00", "2026-11-14T01:00:00Z", "2026-11-14T00:00:00.000001", "1763078400", "1763078400000", "0", "0.0", "2026", "", "bad", "2026-02-30", "0000-01-01", "2026-13-01", "NaN", "Infinity", "999999999999999999999", "2026-11-14 00:00"]:
    row = dict(raw=raw)
    try:
        row["value"] = TypeAdapter(date).validate_python(raw).isoformat()
    except ValidationError as ex:
        error = ex.errors(include_url=False)[0]
        error["loc"] = ["query", "date"]
        row["error"] = error
    date_cases.append(row)
target.write_text(json.dumps(dict(upstream_commit=REF, cases=cases, signatures=signatures, date_cases=date_cases), ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
print(f"Wrote {len(cases)} images and {len(signatures)} cursor signatures to {target.name}")
