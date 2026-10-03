"""メンテナンス API の E2E 検証スクリプト。

実サーバー (Go 版) に対してメンテナンス API を実行し、応答を検証する。
再起動 API はサーバープロセスを実際に終了させるため、最後に実行する。

使い方: python tools/e2e_maintenance_verify.py <base_url> <workdir> <admin_user_id> <normal_user_id>
"""

import http.client
import json
import re
import socket
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))

from app.routers.UsersRouter import GenerateAccessToken  # noqa: E402


def request(
    method: str,
    url: str,
    token: str | None = None,
    host: str | None = None,
) -> tuple[int, str]:
    req = urllib.request.Request(url, method=method)
    if token is not None:
        req.add_header('Authorization', f'Bearer {token}')
    if host is not None:
        req.add_header('Host', host)
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            return response.status, response.read().decode('utf-8')
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode('utf-8')


def requestStream(
    url: str,
    token: str | None = None,
    timeout: float = 5.0,
) -> tuple[int, str]:
    """イベントストリームを timeout 秒だけ読み取る (読み取りがタイムアウトしても、読み取れた分を返す) 。

    最初のイベントの data 行が完結するまで読み取る。
    """
    parts = urllib.parse.urlsplit(url)
    connection = http.client.HTTPConnection(parts.hostname, parts.port, timeout=timeout)
    headers = {}
    if token is not None:
        headers['Authorization'] = f'Bearer {token}'
    chunks: list[bytes] = []

    def isComplete() -> bool:
        data = b''.join(chunks)
        if b'data: ' not in data:
            return False
        return b'\r\n\r\n' in data.split(b'data: ', 1)[1]

    try:
        connection.request('GET', parts.path, headers=headers)
        response = connection.getresponse()
        if response.status != 200:
            return response.status, response.read().decode('utf-8', errors='replace')
        while not isComplete():
            # read1() は読み取り可能な分だけを返すため、チャンク転送でもブロックしない
            chunk = response.read1(256)
            if not chunk:
                break
            chunks.append(chunk)
    except (socket.timeout, TimeoutError, OSError):
        pass
    finally:
        connection.close()
    return 200, b''.join(chunks).decode('utf-8', errors='replace')


def report(failures: list[str]) -> int:
    if failures:
        print('FAILED')
        for failure in failures:
            print(f'  - {failure}')
        return 1
    print('ALL CHECKS PASSED')
    return 0


def main() -> int:
    base_url = sys.argv[1]
    workdir = Path(sys.argv[2])
    admin_user_id = int(sys.argv[3])
    normal_user_id = int(sys.argv[4])
    failures: list[str] = []
    admin_token = GenerateAccessToken(admin_user_id)
    normal_token = GenerateAccessToken(normal_user_id)

    # ***** GET /api/maintenance/logs/{log_type} *****

    # 未ログインの場合は 401
    status, body = request('GET', f'{base_url}/api/maintenance/logs/server')
    if status != 401:
        failures.append(f'GET /api/maintenance/logs/server (未ログイン): status = {status}, body = {body}')

    # 一般ユーザーの場合は 403
    status, body = request('GET', f'{base_url}/api/maintenance/logs/server', token=normal_token)
    if status != 403:
        failures.append(f'GET /api/maintenance/logs/server (一般ユーザー): status = {status}, body = {body}')

    # 不正な log_type の場合は 422
    status, body = request('GET', f'{base_url}/api/maintenance/logs/invalid', token=admin_token)
    if status != 422:
        failures.append(f'GET /api/maintenance/logs/invalid: status = {status}, body = {body}')

    # アクセスログは存在するため 200 でイベントストリームが配信される
    status, body = requestStream(f'{base_url}/api/maintenance/logs/access', token=admin_token, timeout=3.0)
    if status != 200:
        failures.append(f'GET /api/maintenance/logs/access: status = {status}, body = {body[:200]}')

    # 管理者ユーザーの場合は 200 で、initial_log_update イベントが配信される
    status, body = requestStream(f'{base_url}/api/maintenance/logs/server', token=admin_token)
    if status != 200:
        failures.append(f'GET /api/maintenance/logs/server (管理者): status = {status}, body = {body}')
    else:
        # SSE の行末は CRLF のため、正規表現で扱いやすいように CR を除去する
        body = body.replace(chr(13), '')
        if 'event: initial_log_update' not in body:
            failures.append(f'GET /api/maintenance/logs/server: initial_log_update がありません: {body[:200]}')
        # Go 版サーバーの起動ログが Python 版と同じ形式で配信されている
        match = re.search(r'^data: (\[.*\])$', body, re.MULTILINE)
        if match is None:
            failures.append(f'GET /api/maintenance/logs/server: data 行がありません: {body[:200]}')
        else:
            lines = json.loads(match.group(1))
            if not any('KonomiTV server (Go) started' in line for line in lines):
                failures.append(f'GET /api/maintenance/logs/server: 起動ログがありません: {lines[:3]}')
            pattern = re.compile(r'^\[\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{3}\] (INFO|WARNING|ERROR|DEBUG): +\S')
            if not all(pattern.match(line) for line in lines):
                failures.append(f'GET /api/maintenance/logs/server: ログ形式が異なります: {lines[:3]}')

    # ***** POST /api/maintenance/update-database *****

    # IPTV バックエンドでは認証不要で 204 を返す
    status, body = request('POST', f'{base_url}/api/maintenance/update-database')
    if status != 204:
        failures.append(f'POST /api/maintenance/update-database: status = {status}, body = {body}')

    # ***** POST /api/maintenance/restart / shutdown *****

    # 未ログインの場合は 401
    status, body = request('POST', f'{base_url}/api/maintenance/restart')
    if status != 401:
        failures.append(f'POST /api/maintenance/restart (未ログイン): status = {status}, body = {body}')

    # 一般ユーザーの場合は 403
    status, body = request('POST', f'{base_url}/api/maintenance/restart', token=normal_token)
    if status != 403:
        failures.append(f'POST /api/maintenance/restart (一般ユーザー): status = {status}, body = {body}')

    # 内部ポートからのアクセス (Host ヘッダーが 127.0.0.77:7010) は認証不要
    status, body = request('POST', f'{base_url}/api/maintenance/restart', host='127.0.0.77:7010')
    if status != 204:
        failures.append(f'POST /api/maintenance/restart (内部ポート): status = {status}, body = {body}')

    # 再起動が必要であることを示すロックファイルが作成される
    lock_path = workdir / 'server' / 'data' / 'restart_required.lock'
    if not lock_path.exists():
        failures.append(f'POST /api/maintenance/restart: {lock_path} が作成されていません')

    # サーバーログとアクセスログが Python 版と同じ形式で出力されている
    server_log_path = workdir / 'server' / 'logs' / 'KonomiTV-Server.log'
    if not server_log_path.exists():
        failures.append(f'{server_log_path} が存在しません')
    else:
        log_lines = server_log_path.read_text(encoding='utf-8').splitlines()
        pattern = re.compile(r'^\[\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{3}\] (INFO|WARNING|ERROR|DEBUG): +\S')
        if not log_lines or not all(pattern.match(line) for line in log_lines):
            failures.append(f'{server_log_path}: ログ形式が異なります: {log_lines[:3]}')

    access_log_path = workdir / 'server' / 'logs' / 'KonomiTV-Access.log'
    if not access_log_path.exists():
        failures.append(f'{access_log_path} が存在しません')
    else:
        access_lines = access_log_path.read_text(encoding='utf-8').splitlines()
        pattern = re.compile(
            r'^\[\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d{3}\] INFO:     [\d.:]+ - "POST /api/maintenance/restart HTTP/1\.1" 204 No Content$',
        )
        if not any(pattern.match(line) for line in access_lines):
            failures.append(f'{access_log_path}: 再起動 API のアクセスログがありません: {access_lines[-3:]}')

    return report(failures)


if __name__ == '__main__':
    sys.exit(main())
