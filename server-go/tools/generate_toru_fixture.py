"""本家 TorneClient を合成 HTTP/DB に接続し、Go との互換性フィクスチャを生成する。

本家クラスだけを AST で取り出す。実サーバーのモジュールや認証情報は読み込まない。
"""

import ast
import asyncio
from dataclasses import asdict, dataclass
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import random
import statistics
import struct
import subprocess
from types import SimpleNamespace
from typing import ClassVar

ROOT = Path(__file__).resolve().parents[2]
REF = "upstream/master"
source = subprocess.check_output(["git", "-C", str(ROOT), "show", f"{REF}:server/app/utils/TorneClient.py"]).decode("utf-8")
tree = ast.parse(source)
node = next(node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == "TorneClient")


@dataclass
class Count:
    count: int
    highlight_base: int | None


def payload(groups):
    headers = b"\x00" + bytes([len(groups)]) + b"\x00" * 6
    records = b""
    for group, rows in groups:
        headers += struct.pack(">Q", group << 60 | len(rows))
        for sid, eid, count in rows:
            records += struct.pack(">HHI", sid, eid, count)
    return headers + records


async def expected(channels, responses):
    class Query:
        async def all(self):
            return [SimpleNamespace(type=row["Type"], network_id=row["NetworkID"], service_id=row["ServiceID"]) for row in channels]

    class Client:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *_):
            pass

        async def post(self, *, data, **_):
            return SimpleNamespace(content=responses[int(data["regionid"])], raise_for_status=lambda: None)

    env = {
        "ClassVar": ClassVar, "statistics": statistics, "struct": struct, "datetime": datetime,
        "JST": timezone(timedelta(hours=9)), "schemas": SimpleNamespace(ToruCount=Count),
        "Channel": SimpleNamespace(filter=lambda **_: Query()), "HTTPX_CLIENT": Client,
        "Interlaced": lambda _: "synthetic", "logging": SimpleNamespace(info=lambda *_: None, warning=lambda *_a, **_k: None),
        "httpx": SimpleNamespace(NetworkError=OSError, TimeoutException=TimeoutError, HTTPStatusError=ValueError),
    }
    future = ast.ImportFrom(module="__future__", names=[ast.alias(name="annotations")], level=0)
    module = ast.fix_missing_locations(ast.Module(body=[future, node], type_ignores=[]))
    exec(compile(module, "upstream/TorneClient.py", "exec"), env)
    client = env["TorneClient"]
    await client.updateToruCounts()
    return {key: asdict(value) for key, value in client.getToruCounts().items()}


async def main():
    randomizer = random.Random(20261114)
    cases = [
        ("regions-and-duplicates", [{"Type": "GR", "NetworkID": 10, "ServiceID": 31 << 10}, {"Type": "GR", "NetworkID": 11, "ServiceID": 32 << 10}, {"Type": "BS", "NetworkID": 4, "ServiceID": 101}], {
            31: payload([(1, [(31 << 10, 1, 10), (31 << 10, 0, 90), (31 << 10, 0, 999), (32 << 10, 1, 2)]), (2, [(101, 1, 100), (101, 2, 1000)])]),
            32: payload([(1, [(31 << 10, 1, 3), (32 << 10, 1, 20), (32 << 10, 2, 60)]), (2, [(101, 1, 100), (101, 2, 1000)])]),
        }),
        ("population-deviation", [{"Type": "BS", "NetworkID": 4, "ServiceID": 101}], {23: payload([(2, [(101, i + 1, randomizer.randrange(0, 100000)) for i in range(150)])])}),
        ("low-counts", [{"Type": "CS", "NetworkID": 6, "ServiceID": 333}], {23: payload([(5, [(333, 1, 0), (333, 2, 1), (333, 3, 1)])])}),
        ("unsupported", [{"Type": "CATV", "NetworkID": 20, "ServiceID": 1}], {}),
    ]
    result = {"upstream_commit": subprocess.check_output(["git", "-C", str(ROOT), "rev-parse", REF]).decode().strip(), "cases": []}
    for name, channels, responses in cases:
        result["cases"].append({"name": name, "channels": channels, "responses": {key: value.hex() for key, value in responses.items()}, "expected": await expected(channels, responses)})
    target = ROOT / "server-go/internal/torne/testdata/upstream.json"
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(f"Generated {len(cases)} synthetic upstream cases")


asyncio.run(main())
