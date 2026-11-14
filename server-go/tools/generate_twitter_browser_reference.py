"""本家の実メソッドによるフィルター・UA・Cookie 書き出しの比較データを生成する。"""

import ast
import asyncio
import hashlib
import json
import re
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Annotated, Any, ClassVar, Literal

from pydantic import BaseModel, Field


def BuildOracle(source: str) -> type:
    """本家の純粋処理と Cookie 書き出しを変更せず抽出する。

    Args:
        source: 固定した TwitterScrapeBrowser.py の内容。
    Returns:
        ブラウザー依存を持たない比較用クラス。
    """
    tree = ast.parse(source)
    models = [node for node in tree.body if isinstance(node, ast.ClassDef) and node.name in {
        'TwitterAnalyticsBlockFilter', 'TwitterAnalyticsBlockFilterCache',
    }]
    browser = next(node for node in tree.body if isinstance(node, ast.ClassDef) and node.name == 'TwitterScrapeBrowser')
    selected: list[ast.stmt] = []
    for node in browser.body:
        if isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name) and node.target.id == '_UA_OS_INFO_BY_PLATFORM':
            selected.append(node)
        if isinstance(node, ast.FunctionDef | ast.AsyncFunctionDef) and node.name in {
            '_extractAnalyticsBlockFilters', '_buildSpoofedUserAgent', '_buildAcceptLanguage', 'saveTwitterCookiesToNetscapeFormat',
        }:
            selected.append(node)
    browser.body = selected
    namespace = {
        're': re, 'Any': Any, 'Annotated': Annotated, 'ClassVar': ClassVar, 'Literal': Literal,
        'BaseModel': BaseModel, 'Field': Field, 'logging': SimpleNamespace(warning=lambda *_args: None),
    }
    module = ast.fix_missing_locations(ast.Module(body=[*models, browser], type_ignores=[]))
    exec(compile(module, '<pinned TwitterScrapeBrowser methods>', 'exec'), namespace)
    return namespace['TwitterScrapeBrowser']


async def CookieResult(oracle: type, cookies: list[dict[str, Any]]) -> str:
    """実メソッドの CDP 取得境界だけを固定した Cookie で置き換える。

    Args:
        oracle: 抽出した本家クラス。
        cookies: CDP が返す Cookie の辞書。
    Returns:
        本家が生成した Netscape 文字列。
    """
    async def GetAll(**_kwargs: Any) -> list[SimpleNamespace]:
        return [SimpleNamespace(**cookie) for cookie in cookies]
    browser = oracle()
    browser._browser = SimpleNamespace(cookies=SimpleNamespace(get_all=GetAll))
    browser.log_prefix = '[local oracle]'
    return await browser.saveTwitterCookiesToNetscapeFormat()


def Main() -> None:
    """生成した比較データを Go の testdata へ保存する。

    Args:
        なし。コマンド引数に本家ソースのパスを取る。
    Returns:
        なし。
    """
    source_path = Path(sys.argv[1])
    source = source_path.read_text(encoding='utf-8')
    oracle = BuildOracle(source)
    browser = oracle()
    filter_inputs: list[list[str]] = []
    for host in ['x.com', 'twitter.com', 'analytics.x.com', 'a.twitter.com', 'X.Com', 'x.com.evil.com', 'evilx.com']:
        for path in ['/i/jot', '^*/log.json', '/api*^', '/a|', '/a|b', '^', '/測定 #~+-']:
            for options in ['', '$xhr', '$ping', '$xmlhttprequest,3p', '$1p', '$third-party', '$badfilter', '$domain=x.com',
                            '$important,redirect=nooptext', '$xhr,ping', '$~xhr', '$']:
                filter_inputs.append([f'||{host}{path}{options}'])
    filter_inputs.extend([
        ['||x.com/a$xhr\n||x.com/a$ping\n||x.com/a\n||x.com/a$xhr', '||twitter.com/b^$3p,xhr'],
        ['! comment\n@@||x.com/jot$xhr\n##cosmetic\n  ||x.com/z$ping  '],
        ['||x.com/' + 'x' * 600],
    ])
    filter_cases = []
    for contents in filter_inputs:
        case: dict[str, Any] = {'input': contents}
        try:
            case['expected'] = [item.model_dump(mode='json') for item in oracle._extractAnalyticsBlockFilters(contents)]
        except ValueError:
            case['error'] = True
        filter_cases.append(case)
    user_agents = ['Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/140.0.0.0',
                   'Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/140.0.0.0', 'Unexpected agent', 'Mozilla/5.0 () Chrome/1']
    ua_cases = [{'user_agent': agent, 'platform': platform, 'expected': browser._buildSpoofedUserAgent(agent, platform)}
                for agent in user_agents for platform in ['Windows', 'macOS', 'Linux', 'Android', '']]
    language_inputs = [None, '', [], ['ja', 'en'], [' ja ', '', None, 3, 'en;q=0.9'], ['\u3000ja\u00a0', '\t'],
                       {'language': 'ja'}, ['ja', 'ja']]
    languages = [{'input': value, 'expected': browser._buildAcceptLanguage(value)} for value in language_inputs]
    cookie_inputs = [[], [dict(name='auth_token', value='local', domain='.x.com', path='/', secure=True, expires=-1.0)],
                     [dict(name='session', value='a', domain='x.com', path='/a', secure=False, expires=None),
                      dict(name='other', value='b', domain='example.com', path='/', secure=True, expires=12.0),
                      dict(name='unicode', value='日本語', domain='a.x.com', path='/', secure=True, expires=1234.99)]]
    cookies = [{'input': value, 'expected': asyncio.run(CookieResult(oracle, value))} for value in cookie_inputs]
    sample = {
        'reply_state': {'state': {'inReplyToStatusId': '123450'}, 'key': 'abc123'},
        'validated_reply_to_status_id': '123450',
        'tweet_text': '日本語\n"<>__KONOMITV_JSON_0__',
        'images_json_array': [{'base64': 'QQ==', 'mimeType': 'image/png', 'fileName': '画像.png'}],
    }
    script_lines = {894: 'following', 1287: 'discard', 1402: 'reply', 1423: 'modal_ready', 1473: 'paste',
                    1527: 'preload', 1546: 'attach', 1626: 'button_ready', 1662: 'focus'}
    scripts = []
    for node in ast.walk(ast.parse(source)):
        if not isinstance(node, ast.Call) or node.lineno not in script_lines:
            continue
        if not isinstance(node.func, ast.Attribute) or node.func.attr != 'evaluate':
            continue
        expression = next(keyword.value for keyword in node.keywords if keyword.arg == 'expression')
        arguments = []
        if isinstance(expression, ast.JoinedStr):
            for value in expression.values:
                if isinstance(value, ast.FormattedValue):
                    argument = value.value
                    assert isinstance(argument, ast.Call) and isinstance(argument.args[0], ast.Name)
                    arguments.append(sample[argument.args[0].id])
        expected = eval(compile(ast.Expression(expression), '<pinned JS expression>', 'eval'), {'json': json}, sample)
        scripts.append({'name': script_lines[node.lineno], 'arguments': arguments, 'expected': expected})
    setup_script = (source_path.parents[2] / 'static/zendriver_setup.js').read_text(encoding='utf-8')
    fixture = {'source_sha256': hashlib.sha256(source.encode()).hexdigest(), 'filters': filter_cases, 'user_agents': ua_cases,
               'languages': languages, 'cookies': cookies, 'scripts': scripts,
               'setup_script_sha256': hashlib.sha256(setup_script.encode()).hexdigest()}
    destination = Path(__file__).resolve().parents[1] / 'internal/twitter/testdata/browser_reference.json'
    destination.write_text(json.dumps(fixture, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(f'Generated filters={len(filter_cases)}, user_agents={len(ua_cases)}, languages={len(languages)}, cookies={len(cookies)}')


if __name__ == '__main__':
    Main()
