"""キャプチャ API の E2E 検証スクリプト。

実サーバー (Go 版) の応答を、Python 版 CapturesRouter の処理結果と比較する。

使い方: python tools/e2e_captures_verify.py <base_url> <workdir> <user_id>
"""

import io
import json
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))

from PIL import Image  # noqa: E402
from app.config import Config, LoadConfig  # noqa: E402
from app.routers.CapturesRouter import (  # noqa: E402
    ExtractCaptureInfo,
    FindCaptureFile,
    GetUploadFolders,
)
from app.routers.UsersRouter import GenerateAccessToken  # noqa: E402


def request(
    method: str,
    url: str,
    body: bytes | None = None,
    token: str | None = None,
    content_type: str | None = None,
) -> tuple[int, bytes, dict[str, str]]:
    req = urllib.request.Request(url, data=body, method=method)
    if content_type is not None:
        req.add_header('Content-Type', content_type)
    if token is not None:
        req.add_header('Authorization', f'Bearer {token}')
    try:
        with urllib.request.urlopen(req, timeout=60) as response:
            return response.status, response.read(), dict(response.headers)
    except urllib.error.HTTPError as error:
        return error.code, error.read(), dict(error.headers)


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
    user_id = int(sys.argv[3])
    failures: list[str] = []

    # サーバー設定をロードし、キャプチャの保存先フォルダをテスト用フォルダに差し替える
    LoadConfig()
    capture_dir = workdir / 'captures'
    Config().capture.upload_folders = [str(capture_dir)]
    token = GenerateAccessToken(user_id)

    # ***** GET /api/captures *****

    status, body, _ = request('GET', f'{base_url}/api/captures')
    if status != 200:
        failures.append(f'GET /api/captures: status = {status}, body = {body[:200]}')
        return report(failures)
    response = json.loads(body)

    # Python 版の処理結果と比較する (キャプチャ画像 2 件、更新日時の降順)
    upload_folders = GetUploadFolders()
    expected_files = sorted(
        [(path.name, path.stat().st_mtime) for folder in upload_folders for path in folder.iterdir() if path.is_file()],
        key=lambda item: item[1],
        reverse=True,
    )
    expected_files = [item for item in expected_files if item[0].endswith(('.jpg', '.jpeg', '.png'))]
    if response['total'] != len(expected_files):
        failures.append(f'total = {response["total"]}, want {len(expected_files)}')
    if len(response['captures']) != len(expected_files):
        failures.append(f'captures = {len(response["captures"])}, want {len(expected_files)}')
    for index, (filename, _) in enumerate(expected_files):
        if index >= len(response['captures']):
            break
        actual = response['captures'][index]
        if actual['filename'] != filename:
            failures.append(f'captures[{index}].filename = {actual["filename"]}, want {filename}')
            continue
        # Python 版 ExtractCaptureInfo() の結果と完全に一致することを確認する
        filepath = FindCaptureFile(filename)
        expected = ExtractCaptureInfo(filepath).model_dump(mode='json')
        if actual != expected:
            failures.append(f'captures[{index}] = {actual}\n    want {expected}')

    # ***** 検索 *****

    # ファイル名での検索
    status, body, _ = request('GET', f'{base_url}/api/captures?search=e2e_capture1')
    searched = json.loads(body)
    if searched['total'] != 1 or searched['captures'][0]['filename'] != 'e2e_capture1.jpg':
        failures.append(f'ファイル名検索: {searched["total"]} 件')

    # 番組名での検索 (EXIF メタデータ)
    status, body, _ = request('GET', f'{base_url}/api/captures?search={urllib.parse.quote("テスト番組")}')
    searched = json.loads(body)
    if searched['total'] != 1 or searched['captures'][0]['filename'] != 'e2e_capture1.jpg':
        failures.append(f'番組名検索: {searched["total"]} 件')

    # チャンネル名での検索 (チャンネルテーブルとの突き合わせ)
    status, body, _ = request('GET', f'{base_url}/api/captures?search={urllib.parse.quote("NHK総合")}')
    searched = json.loads(body)
    if searched['total'] != 1 or searched['captures'][0]['filename'] != 'e2e_capture1.jpg':
        failures.append(f'チャンネル名検索: {searched["total"]} 件')

    # 一致しない検索
    status, body, _ = request('GET', f'{base_url}/api/captures?search=not-found-keyword')
    if json.loads(body)['total'] != 0:
        failures.append('一致しない検索で 0 件になりません')

    # 不正なパラメーター
    status, body, _ = request('GET', f'{base_url}/api/captures?order=invalid')
    if status != 422:
        failures.append(f'order=invalid: status = {status}')
    status, body, _ = request('GET', f'{base_url}/api/captures?page=0')
    if status != 422:
        failures.append(f'page=0: status = {status}')

    # ***** GET /api/captures/{filename} *****

    status, body, headers = request('GET', f'{base_url}/api/captures/e2e_capture1.jpg')
    if status != 200 or headers.get('Content-Type') != 'image/jpeg':
        failures.append(f'画像取得: status = {status}, Content-Type = {headers.get("Content-Type")}')
    elif len(body) != (capture_dir / 'e2e_capture1.jpg').stat().st_size:
        failures.append('画像のサイズが一致しません')

    # サムネイル画像 (Pillow の thumbnail() と同じサイズになることを確認する)
    status, body, headers = request('GET', f'{base_url}/api/captures/e2e_capture1.jpg?thumbnail=true')
    if status != 200 or headers.get('Content-Type') != 'image/jpeg':
        failures.append(f'サムネイル: status = {status}, Content-Type = {headers.get("Content-Type")}')
    else:
        with Image.open(capture_dir / 'e2e_capture1.jpg') as image:
            from PIL import ImageOps

            transposed = ImageOps.exif_transpose(image)
            if transposed is not None:
                image = transposed
            image.thumbnail((400, 400), Image.Resampling.LANCZOS)
            expected_size = image.size
        with Image.open(io.BytesIO(body)) as thumbnail:
            if thumbnail.size != expected_size:
                failures.append(f'サムネイルのサイズ = {thumbnail.size}, want {expected_size}')

    # 存在しないファイルは 404
    status, body, _ = request('GET', f'{base_url}/api/captures/not-found.jpg')
    if status != 404:
        failures.append(f'存在しないファイル: status = {status}')

    # ***** キャプチャフォルダ *****

    # 未ログインの場合は 401
    status, body, _ = request('GET', f'{base_url}/api/captures/folders')
    if status != 401:
        failures.append(f'フォルダ一覧 (未ログイン): status = {status}')

    # フォルダ一覧 (初期状態は空)
    status, body, _ = request('GET', f'{base_url}/api/captures/folders', token=token)
    if status != 200 or body != b'{"total":0,"folders":[]}\n':
        failures.append(f'フォルダ一覧: status = {status}, body = {body[:200]}')

    # フォルダを作成する
    status, body, _ = request(
        'POST', f'{base_url}/api/captures/folders',
        body=json.dumps({'name': 'テストフォルダ'}).encode('utf-8'),
        token=token, content_type='application/json',
    )
    if status != 201:
        failures.append(f'フォルダ作成: status = {status}, body = {body[:200]}')
        return report(failures)
    folder = json.loads(body)
    folder_id = folder['id']
    if folder['name'] != 'テストフォルダ' or folder['sort_order'] != 0 or folder['capture_count'] != 0:
        failures.append(f'フォルダ作成: {folder}')
    # 日時は Pydantic の datetime 形式 (+09:00) で返る
    if not folder['created_at'].endswith('+09:00') or not folder['updated_at'].endswith('+09:00'):
        failures.append(f'フォルダ作成: 日時形式が異なります: {folder["created_at"]}, {folder["updated_at"]}')

    # 名前が空の場合は 422
    status, body, _ = request(
        'POST', f'{base_url}/api/captures/folders',
        body=json.dumps({'name': '  '}).encode('utf-8'), token=token, content_type='application/json',
    )
    if status != 422:
        failures.append(f'空のフォルダ名: status = {status}')

    # キャプチャをフォルダに追加する (存在しないファイルは無視される)
    status, body, _ = request(
        'POST', f'{base_url}/api/captures/folders/{folder_id}/captures',
        body=json.dumps({'filenames': ['e2e_capture1.jpg', 'e2e_capture2.png', 'not-found.jpg']}).encode('utf-8'),
        token=token, content_type='application/json',
    )
    if status != 204:
        failures.append(f'キャプチャ追加: status = {status}, body = {body[:200]}')

    # フォルダ内のキャプチャ一覧 (Python 版の ExtractCaptureInfo() と一致することを確認する)
    status, body, _ = request('GET', f'{base_url}/api/captures/folders/{folder_id}/captures', token=token)
    folder_captures = json.loads(body)
    if folder_captures['total'] != 2:
        failures.append(f'フォルダ内キャプチャ: total = {folder_captures["total"]}')
    for capture in folder_captures['captures']:
        expected = ExtractCaptureInfo(FindCaptureFile(capture['filename'])).model_dump(mode='json')
        if capture != expected:
            failures.append(f'フォルダ内キャプチャ {capture["filename"]} = {capture}\n    want {expected}')

    # フォルダ一覧にキャプチャ数が反映される
    status, body, _ = request('GET', f'{base_url}/api/captures/folders', token=token)
    if json.loads(body)['folders'][0]['capture_count'] != 2:
        failures.append(f'フォルダ一覧のキャプチャ数: {body[:200]}')

    # フォルダの更新
    status, body, _ = request(
        'PUT', f'{base_url}/api/captures/folders/{folder_id}',
        body=json.dumps({'name': '新しい名前', 'sort_order': 3}).encode('utf-8'),
        token=token, content_type='application/json',
    )
    updated = json.loads(body)
    if status != 200 or updated['name'] != '新しい名前' or updated['sort_order'] != 3 or updated['capture_count'] != 2:
        failures.append(f'フォルダ更新: status = {status}, body = {body[:200]}')

    # 存在しないフォルダは 404
    status, body, _ = request('GET', f'{base_url}/api/captures/folders/9999/captures', token=token)
    if status != 404:
        failures.append(f'存在しないフォルダ: status = {status}')

    # フォルダからキャプチャを削除する
    status, body, _ = request(
        'DELETE', f'{base_url}/api/captures/folders/{folder_id}/captures',
        body=json.dumps({'filenames': ['e2e_capture1.jpg']}).encode('utf-8'),
        token=token, content_type='application/json',
    )
    if status != 204:
        failures.append(f'キャプチャ削除: status = {status}')
    status, body, _ = request('GET', f'{base_url}/api/captures/folders/{folder_id}/captures', token=token)
    if json.loads(body)['total'] != 1:
        failures.append(f'キャプチャ削除後の一覧: {body[:200]}')

    # ***** POST /api/captures (アップロード) *****

    # アップロードしたファイルは保存され、204 が返る
    status, body, _ = request(
        'POST', f'{base_url}/api/captures',
        body=multipartBody('image', 'e2e_upload.jpg', (capture_dir / 'e2e_capture1.jpg').read_bytes()),
        token=token, content_type=multipartContentType('image', 'e2e_upload.jpg'),
    )
    if status != 204:
        failures.append(f'アップロード: status = {status}, body = {body[:200]}')
    elif not (capture_dir / 'e2e_upload.jpg').exists():
        failures.append('アップロードされたファイルが保存されていません')

    # 同名のファイルをアップロードすると連番が付与される
    status, body, _ = request(
        'POST', f'{base_url}/api/captures',
        body=multipartBody('image', 'e2e_upload.jpg', (capture_dir / 'e2e_capture1.jpg').read_bytes()),
        token=token, content_type=multipartContentType('image', 'e2e_upload.jpg'),
    )
    if status != 204 or not (capture_dir / 'e2e_upload-1.jpg').exists():
        failures.append('同名ファイルのアップロードで連番が付与されません')

    # 画像以外のファイルは 422
    status, body, _ = request(
        'POST', f'{base_url}/api/captures',
        body=multipartBody('image', 'text.txt', b'this is not an image'),
        token=token, content_type=multipartContentType('image', 'text.txt'),
    )
    if status != 422:
        failures.append(f'画像以外のアップロード: status = {status}')
    elif (capture_dir / 'text.txt').exists():
        failures.append('画像以外のファイルが保存されています')

    # ***** DELETE /api/captures/{filename} *****

    status, body, _ = request('DELETE', f'{base_url}/api/captures/e2e_upload.jpg')
    if status != 204 or (capture_dir / 'e2e_upload.jpg').exists():
        failures.append(f'画像削除: status = {status}')
    status, body, _ = request('DELETE', f'{base_url}/api/captures/e2e_upload.jpg')
    if status != 404:
        failures.append(f'削除済みファイルの削除: status = {status}')

    # フォルダを削除する (ブックマークも削除される)
    status, body, _ = request('DELETE', f'{base_url}/api/captures/folders/{folder_id}', token=token)
    if status != 204:
        failures.append(f'フォルダ削除: status = {status}')
    status, body, _ = request('GET', f'{base_url}/api/captures/folders', token=token)
    if body != b'{"total":0,"folders":[]}\n':
        failures.append(f'フォルダ削除後の一覧: {body[:200]}')

    return report(failures)


def multipartBody(field: str, filename: str, content: bytes) -> bytes:
    """multipart/form-data のボディを生成する。"""
    boundary = '----KonomiTVBoundary'
    body = io.BytesIO()
    body.write(f'--{boundary}\r\n'.encode('utf-8'))
    body.write(f'Content-Disposition: form-data; name="{field}"; filename="{filename}"\r\n'.encode('utf-8'))
    body.write(b'Content-Type: application/octet-stream\r\n\r\n')
    body.write(content)
    body.write(f'\r\n--{boundary}--\r\n'.encode('utf-8'))
    return body.getvalue()


def multipartContentType(field: str, filename: str) -> str:
    """multipart/form-data の Content-Type を返す。"""
    return 'multipart/form-data; boundary=----KonomiTVBoundary'


if __name__ == '__main__':
    sys.exit(main())
