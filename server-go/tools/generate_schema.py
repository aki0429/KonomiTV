"""本家のマイグレーションを空の一時 DB だけで実行し、Go 用の現行スキーマを生成する。

既存 DB・実サーバーのモジュール・設定は一切読み込まない。
"""

import ast
import json
from pathlib import Path
import re
import sqlite3
import subprocess

root = Path(__file__).resolve().parents[2]
ref = "upstream/master"
commit = subprocess.check_output(["git", "-C", str(root), "rev-parse", ref]).decode().strip()
paths = subprocess.check_output(["git", "-C", str(root), "ls-tree", "-r", "--name-only", ref, "server/app/migrations/models"]).decode().splitlines()
paths = sorted((p for p in paths if re.search(r"/\d+_.*\.py$", p)), key=lambda p: int(Path(p).name.split("_")[0]))
db = sqlite3.connect(":memory:")
for path in paths:
    source = subprocess.check_output(["git", "-C", str(root), "show", f"{ref}:{path}"]).decode("utf-8")
    function = next(n for n in ast.parse(source).body if isinstance(n, ast.AsyncFunctionDef) and n.name == "upgrade")
    returns = [n for n in ast.walk(function) if isinstance(n, ast.Return)]
    if len(returns) != 1:
        raise ValueError(f"Unsupported migration: {path}")
    db.executescript(ast.literal_eval(returns[0].value))
rows = db.execute("SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END, name").fetchall()
result = [f"-- Generated from tsukumijima/KonomiTV {commit}", "-- Run server-go/tools/generate_schema.py to regenerate."]
for kind, name, sql in rows:
    if kind == "table":
        sql = re.sub(r"^CREATE TABLE(?: IF NOT EXISTS)?", "CREATE TABLE IF NOT EXISTS", sql)
    elif kind == "index":
        sql = re.sub(r"^(CREATE(?: UNIQUE)? INDEX)(?: IF NOT EXISTS)?", r"\1 IF NOT EXISTS", sql)
    else:
        raise ValueError(f"Unsupported schema object: {kind} {name}")
    result.append(sql + ";")
(root / "server-go/internal/database/schema.sql").write_text("\n\n".join(result) + "\n", encoding="utf-8")
columns = {name: [row[1] for row in db.execute(f'PRAGMA table_info("{name}")')] for kind, name, _ in rows if kind == "table"}
(root / "server-go/internal/database/schema_columns.json").write_text(json.dumps(columns, indent=2) + "\n", encoding="utf-8")
print(f"Generated {sum(kind == 'table' for kind, _, _ in rows)} tables and {sum(kind == 'index' for kind, _, _ in rows)} indexes from empty upstream migrations")
