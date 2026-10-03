"""サーバー設定 API の E2E 検証スクリプト。

実サーバー (Go 版) を一時ディレクトリで起動し、設定 API の応答が Python 版と一致することを検証する。
PUT は一時ディレクトリの config.yaml に対してのみ実行する (リポジトリの config.yaml は変更しない) 。

使い方: python tools/e2e_settings_verify.py <base_url> <temp_config_yaml_path> <user_id>
"""

import hashlib
import json
import sys
import urllib.error
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))


def request(method: str, url: str, body: bytes | None = None, token: str | None = None) -> tuple[int, str]:
    req = urllib.request.Request(url, data=body, method=method)
    if body is not None:
        req.add_header('Content-Type', 'application/json')
    if token is not None:
        req.add_header('Authorization', f'Bearer {token}')
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            return response.status, response.read().decode('utf-8')
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode('utf-8')


def fileHash(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main() -> int:
    base_url = sys.argv[1]
    config_path = Path(sys.argv[2])
    user_id = int(sys.argv[3])
    failures: list[str] = []

    # リポジトリの config.yaml が変更されていないことを確認するためのハッシュ
    repo_config_hash = fileHash(REPO_ROOT / 'config.yaml')

    # ***** GET /api/settings/server *****
    status, body = request('GET', f'{base_url}/api/settings/server')
    if status != 200:
        failures.append(f'GET /api/settings/server: status = {status}, body = {body}')
        return report(failures)
    actual = json.loads(body)

    # Python 版 LoadConfig() の結果と比較する (一時ディレクトリの config.yaml を使う)
    import app.config  # noqa: E402
    app.config._CONFIG_YAML_PATH = config_path
    from app.config import LoadConfig  # noqa: E402
    expected_json = LoadConfig(bypass_validation=True).model_dump_json()
    expected = json.loads(expected_json)
    if body == expected_json:
        print('GET /api/settings/server: OK (Python 版とバイト単位で完全一致)')
    elif actual == expected:
        print('GET /api/settings/server: OK (Python 版と値は一致、JSON の表記が一部異なる)')
        failures.append(
            'GET /api/settings/server: JSON の表記が異なります: '
            f'actual = {body} '
            f'want   = {expected_json}'
        )
    else:
        for section in expected:
            if actual.get(section) != expected[section]:
                failures.append(
                    f'GET /api/settings/server: section {section}:\n'
                    f'actual = {actual.get(section)}\nwant   = {expected[section]}'
                )

    # ***** GET /api/settings/client (未ログインは 401) *****
    status, body = request('GET', f'{base_url}/api/settings/client')
    if status != 401:
        failures.append(f'GET /api/settings/client (未ログイン): status = {status}, want 401')
    else:
        print('GET /api/settings/client (未ログイン): OK (401)')

    # ***** PUT /api/settings/server (未ログインは 401) *****
    status, body = request('PUT', f'{base_url}/api/settings/server', json.dumps(expected).encode('utf-8'))
    if status != 401:
        failures.append(f'PUT /api/settings/server (未ログイン): status = {status}, want 401 (body = {body})')
    else:
        print('PUT /api/settings/server (未ログイン): OK (401)')

    # ***** PUT /api/settings/server (管理者) *****
    from app.routers.UsersRouter import GenerateAccessToken  # noqa: E402
    token = GenerateAccessToken(user_id)
    updated = json.loads(json.dumps(expected))
    # 一部の値を変更する (元の値が bool の場合のみ反転させる)
    updated['general']['debug'] = not updated['general']['debug']
    updated['iptv']['cache_ttl'] = 60
    updated['general']['mirakurun_url'] = 'http://127.0.0.1:40772'  # 末尾のスラッシュなし (正規化される)
    status, body = request(
        'PUT', f'{base_url}/api/settings/server',
        json.dumps(updated).encode('utf-8'), token,
    )
    if status != 204:
        failures.append(f'PUT /api/settings/server (管理者): status = {status}, body = {body}')
    else:
        print('PUT /api/settings/server (管理者): OK (204)')

        # 保存された config.yaml を Python 版のパイプライン (ruamel + デフォルト値マージ + Pydantic 検証) で
        # 読み込み、GET の結果と一致することを確認する
        import ruamel.yaml  # noqa: E402
        from app.config import ServerSettings  # noqa: E402
        with open(config_path, encoding='utf-8') as file:
            saved_raw = dict(ruamel.yaml.YAML().load(file))
        saved_merged = ServerSettings().model_dump(mode='json')

        def merge(base: dict, override: dict) -> dict:
            for key, value in override.items():
                if key in base and isinstance(base[key], dict) and isinstance(value, dict):
                    merge(base[key], value)
                else:
                    base[key] = value
            return base

        merge(saved_merged, saved_raw)
        saved = json.loads(ServerSettings.model_validate(
            saved_merged, context={'bypass_validation': True},
        ).model_dump_json())
        status, body = request('GET', f'{base_url}/api/settings/server')
        actual = json.loads(body)
        saved_json = ServerSettings.model_validate(saved_merged, context={'bypass_validation': True}).model_dump_json()
        if body == saved_json:
            print('PUT 後の GET /api/settings/server: OK (Python 版とバイト単位で完全一致)')
        elif actual == saved:
            print('PUT 後の GET /api/settings/server: OK (Python 版と値は一致、JSON の表記が一部異なる)')
            failures.append(
                'PUT 後の GET: JSON の表記が異なります: '
                f'actual = {body} '
                f'want   = {saved_json}'
            )
        else:
            for section in saved:
                if actual.get(section) != saved[section]:
                    failures.append(
                        f'PUT 後の GET /api/settings/server: section {section}:\n'
                        f'actual = {actual.get(section)}\nwant   = {saved[section]}'
                    )
        if saved['general']['mirakurun_url'] != 'http://127.0.0.1:40772/':
            failures.append(f'mirakurun_url was not normalized: {saved["general"]["mirakurun_url"]}')
        if saved['iptv']['cache_ttl'] != 60:
            failures.append(f'cache_ttl was not saved: {saved["iptv"]["cache_ttl"]}')

    # ***** PUT /api/settings/server (バリデーションエラー) *****
    invalid = json.loads(json.dumps(expected))
    invalid['general']['backend'] = 'Invalid'
    before = fileHash(config_path)
    status, body = request('PUT', f'{base_url}/api/settings/server', json.dumps(invalid).encode('utf-8'), token)
    if status != 422:
        failures.append(f'PUT /api/settings/server (不正な backend): status = {status}, want 422')
    elif fileHash(config_path) != before:
        failures.append('config.yaml was modified on a validation error')
    else:
        print('PUT /api/settings/server (不正な backend): OK (422 / config.yaml は変更なし)')

    # ***** PUT /api/settings/client *****
    # クライアント設定を更新し、DB に保存された JSON が Python 版 (Tortoise の JSONField) と
    # バイト単位で一致することを確認する
    import sqlite3  # noqa: E402
    from app.config import ClientSettings  # noqa: E402
    database_path = config_path.parent / 'server' / 'data' / 'database.sqlite'
    connection = sqlite3.connect(database_path)
    before_settings = connection.execute(
        'SELECT client_settings FROM users WHERE id = ?', (user_id,),
    ).fetchone()[0]
    client_body = {
        'last_synced_at': 1700000000.0,
        'caption_font': 'テストフォント',
        'mylist': [{'id': 'test', 'title': 'テスト'}, {'id': 'test2', 'title': 'テスト2'}],
        'timetable_channel_width': 'Wide',
        'caption_opacity': 0.5,
        'muted_comment_keywords': [{'keyword': 'テスト'}],
        'unknown_key': True,  # Pydantic では無視される
    }
    status, body = request(
        'PUT', f'{base_url}/api/settings/client',
        json.dumps(client_body).encode('utf-8'), token,
    )
    if status != 204:
        failures.append(f'PUT /api/settings/client: status = {status}, body = {body}')
    else:
        expected_client = json.dumps(
            ClientSettings.model_validate(client_body).model_dump(mode='json'),
            ensure_ascii=False, separators=(',', ':'),
        )
        stored = connection.execute(
            'SELECT client_settings FROM users WHERE id = ?', (user_id,),
        ).fetchone()[0]
        if stored == expected_client:
            print('PUT /api/settings/client: OK (Python 版とバイト単位で完全一致)')
        else:
            failures.append(
                'PUT /api/settings/client: 保存された JSON が異なります: '
                f'actual = {stored} '
                f'want   = {expected_client}'
            )
        # GET でも同じ JSON が返る
        status, body = request('GET', f'{base_url}/api/settings/client', token=token)
        if body.strip() != expected_client:
            failures.append(f'GET /api/settings/client: actual = {body.strip()} want = {expected_client}')
        else:
            print('GET /api/settings/client: OK (Python 版とバイト単位で完全一致)')
    # 元の値に戻す
    connection.execute(
        'UPDATE users SET client_settings = ? WHERE id = ?', (before_settings, user_id),
    )
    connection.commit()
    connection.close()

    # ***** リポジトリの config.yaml が変更されていないことを確認する *****
    if fileHash(REPO_ROOT / 'config.yaml') != repo_config_hash:
        failures.append('the repository config.yaml was modified!')
    else:
        print('リポジトリの config.yaml: OK (変更なし)')

    return report(failures)


def report(failures: list[str]) -> int:
    if failures:
        print('')
        print('FAILED:')
        for failure in failures:
            print(f'  - {failure}')
        return 1
    print('')
    print('All checks passed.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
