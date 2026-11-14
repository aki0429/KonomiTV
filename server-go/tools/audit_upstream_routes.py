"""本家の FastAPI 宣言と Go の登録ルートを静的に比較する。

ルートの存在は機能互換性の証明ではない。実装と実機の検証は別に記録する。
サーバーの Python モジュール・設定・データベースは読み込まない。
"""

import argparse
import ast
import json
from pathlib import Path
import re
import subprocess


def git(root: Path, *args: str) -> str:
    return subprocess.check_output(["git", "-C", str(root), *args]).decode("utf-8")


def normalized(path: str) -> str:
    return re.sub(r"\{([^}:]+):[^}]+\}", r"{\1}", path).rstrip("/")


def key(method: str, path: str) -> tuple[str, str]:
    return method, re.sub(r"\{[^}]+\}", "{}", normalized(path))


def audit(root: Path, ref: str) -> dict:
    upstream = []
    paths = git(root, "ls-tree", "-r", "--name-only", ref, "server/app/routers").splitlines()
    for path in paths:
        if not path.endswith("Router.py"):
            continue
        tree = ast.parse(git(root, "show", f"{ref}:{path}"))
        prefix = ""
        for node in ast.walk(tree):
            if isinstance(node, ast.Call) and ((isinstance(node.func, ast.Attribute) and node.func.attr == "APIRouter") or (isinstance(node.func, ast.Name) and node.func.id == "APIRouter")):
                prefix = next((ast.literal_eval(k.value) for k in node.keywords if k.arg == "prefix"), "")
        for node in ast.walk(tree):
            if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue
            for decorator in node.decorator_list:
                if not isinstance(decorator, ast.Call) or not isinstance(decorator.func, ast.Attribute):
                    continue
                if not isinstance(decorator.func.value, ast.Name) or decorator.func.value.id != "router":
                    continue
                method = decorator.func.attr.upper()
                if method not in {"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"}:
                    raise ValueError(f"Unsupported route declaration: {path}:{node.lineno}: {method}")
                route = normalized(prefix + ast.literal_eval(decorator.args[0]))
                upstream.append({"method": method, "path": route, "source": path, "function": node.name})
    go_routes = set()
    for path in (root / "server-go/internal/api").glob("*.go"):
        if path.name.endswith("_test.go"):
            continue
        for method, route in re.findall(r'\.Handle(?:Func)?\("(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS) ([^"]+)"', path.read_text(encoding="utf-8")):
            go_routes.add(key(method, route))
    # URL 全体をパスに含む web-bml は ServeMux による // 正規化を避けて直接ディスパッチする。
    for method in ("GET", "POST"):
        go_routes.add(key(method, "/api/data-broadcasting/request/{url}"))
    # AT URI のスラッシュを受ける Bluesky 専用 dispatcher の実装済み分岐。
    for method in ("PUT", "DELETE"):
        for action in ("like", "repost"):
            go_routes.add(key(method, f"/api/bluesky/accounts/{{handle}}/posts/{{post_id}}/{action}"))
    missing = [r for r in upstream if key(r["method"], r["path"]) not in go_routes]
    return {
        "upstream_ref": ref,
        "upstream_commit": git(root, "rev-parse", ref).strip(),
        "upstream_routes": len(upstream),
        "go_registered_routes": len(go_routes),
        "missing_routes": missing,
        "routes": upstream,
        "limitation": "Static route registration only; does not prove implementation, response, UI or runtime parity.",
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ref", default="upstream/master")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    result = audit(Path(__file__).resolve().parents[2], args.ref)
    if args.output:
        args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({k: v for k, v in result.items() if k != "routes"}, ensure_ascii=False, indent=2))
