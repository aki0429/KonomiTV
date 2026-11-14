"""固定した本家ソースからブラウザー内で実行する JS 式を抽出する開発用ツール。"""

import ast
import hashlib
import json
import sys
from pathlib import Path


def ExtractExpression(node: ast.expr) -> dict[str, object]:
    """JS のリテラルと f-string の JSON 挿入位置を保持する。

    Args:
        node: 本家 runtime.evaluate() の expression。
    Returns:
        スクリプトのテンプレートと挿入する Python 式。
    """
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return {'template': node.value, 'expressions': []}
    if not isinstance(node, ast.JoinedStr):
        raise ValueError(f'Unsupported expression at line {node.lineno}')
    parts: list[str] = []
    expressions: list[str] = []
    for value in node.values:
        if isinstance(value, ast.Constant) and isinstance(value.value, str):
            parts.append(value.value)
        elif isinstance(value, ast.FormattedValue) and value.conversion == -1 and value.format_spec is None:
            parts.append(f'__KONOMITV_JSON_{len(expressions)}__')
            expressions.append(ast.unparse(value.value))
        else:
            raise ValueError(f'Unsupported f-string at line {node.lineno}')
    return {'template': ''.join(parts), 'expressions': expressions}


def Main() -> None:
    """本家の固定行位置とソース SHA を含む JSON を保存する。

    Args:
        なし。コマンド引数に TwitterScrapeBrowser.py のパスを取る。
    Returns:
        なし。
    """
    source_path = Path(sys.argv[1])
    source = source_path.read_text(encoding='utf-8')
    tree = ast.parse(source)
    wanted = {
        894: 'following',
        1287: 'discard',
        1402: 'reply',
        1423: 'modal_ready',
        1473: 'paste',
        1527: 'preload',
        1546: 'attach',
        1626: 'button_ready',
        1662: 'focus',
    }
    scripts: dict[str, dict[str, object]] = {}
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call) or node.lineno not in wanted:
            continue
        if not isinstance(node.func, ast.Attribute) or node.func.attr != 'evaluate':
            continue
        expression = next(keyword.value for keyword in node.keywords if keyword.arg == 'expression')
        scripts[wanted[node.lineno]] = ExtractExpression(expression)
    if set(scripts) != set(wanted.values()):
        raise ValueError('Pinned source expressions changed')
    destination = Path(__file__).resolve().parents[1] / 'internal/twitter/browser_assets/scripts.json'
    destination.write_text(json.dumps({
        'source_sha256': hashlib.sha256(source.encode()).hexdigest(),
        'scripts': scripts,
    }, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(f'Extracted {len(scripts)} browser scripts')


if __name__ == '__main__':
    Main()
