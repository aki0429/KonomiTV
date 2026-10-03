"""録画番組 API の E2E 検証スクリプト。

Go 版サーバーの応答を、Python 版 (schemas.RecordedProgram) の応答と比較する。

使い方: python tools/e2e_videos_verify.py <base_url> <作業ディレクトリ> <管理者ユーザーの ID>
"""

import json
import sqlite3
import sys
import urllib.error
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))


def header(headers: dict[str, str], name: str) -> str | None:
    """HTTP ヘッダーを大文字小文字を区別せずに取得する"""
    for key, value in headers.items():
        if key.lower() == name.lower():
            return value
    return None


def request(
    method: str, url: str, body: bytes | None = None, token: str | None = None,
    headers: dict[str, str] | None = None,
) -> tuple[int, bytes, dict[str, str]]:
    req = urllib.request.Request(url, data=body, method=method)
    if body is not None:
        req.add_header('Content-Type', 'application/json')
    if token is not None:
        req.add_header('Authorization', f'Bearer {token}')
    for key, value in (headers or {}).items():
        req.add_header(key, value)
    try:
        with urllib.request.urlopen(req, timeout=30) as response:
            return response.status, response.read(), dict(response.headers)
    except urllib.error.HTTPError as error:
        return error.code, error.read(), dict(error.headers)


# Python 版 ConvertRowToRecordedProgram() と同じ変換を行う
def convert_row_to_recorded_program(row: dict) -> dict:
    from app import schemas
    cm_sections = json.loads(row['cm_sections']) if row['cm_sections'] is not None else None
    thumbnail_info = json.loads(row['thumbnail_info']) if row['thumbnail_info'] is not None else None
    recorded_video_dict = {
        'id': row['rv_id'], 'status': row['status'], 'file_path': row['file_path'],
        'file_hash': row['file_hash'], 'file_size': row['file_size'],
        'file_created_at': row['file_created_at'], 'file_modified_at': row['file_modified_at'],
        'recording_start_time': row['recording_start_time'], 'recording_end_time': row['recording_end_time'],
        'duration': row['video_duration'], 'container_format': row['container_format'],
        'video_codec': row['video_codec'], 'video_codec_profile': row['video_codec_profile'],
        'video_scan_type': row['video_scan_type'], 'video_frame_rate': row['video_frame_rate'],
        'video_resolution_width': row['video_resolution_width'],
        'video_resolution_height': row['video_resolution_height'],
        'has_video_stream_changes': row['has_video_stream_changes'],
        'primary_audio_codec': row['primary_audio_codec'],
        'primary_audio_channel': row['primary_audio_channel'],
        'primary_audio_sampling_rate': row['primary_audio_sampling_rate'],
        'secondary_audio_codec': row['secondary_audio_codec'],
        'secondary_audio_channel': row['secondary_audio_channel'],
        'secondary_audio_sampling_rate': row['secondary_audio_sampling_rate'],
        'cm_sections': cm_sections, 'thumbnail_info': thumbnail_info,
        'created_at': row['rv_created_at'], 'updated_at': row['rv_updated_at'],
    }
    channel_dict = None
    if row['ch_id'] is not None:
        channel_dict = {
            'id': row['ch_id'], 'display_channel_id': row['display_channel_id'],
            'network_id': row['ch_network_id'], 'service_id': row['ch_service_id'],
            'transport_stream_id': row['transport_stream_id'], 'remocon_id': row['remocon_id'],
            'channel_number': row['channel_number'], 'type': row['type'], 'name': row['ch_name'],
            'jikkyo_force': row['jikkyo_force'], 'is_subchannel': bool(row['is_subchannel']),
            'is_radiochannel': bool(row['is_radiochannel']), 'is_watchable': bool(row['is_watchable']),
        }
    recorded_program_dict = {
        'id': row['rp_id'], 'recorded_video': recorded_video_dict,
        'recording_start_margin': row['recording_start_margin'],
        'recording_end_margin': row['recording_end_margin'],
        'is_partially_recorded': bool(row['is_partially_recorded']),
        'channel': channel_dict, 'network_id': row['network_id'], 'service_id': row['service_id'],
        'event_id': row['event_id'], 'series_id': row['series_id'],
        'series_broadcast_period_id': row['series_broadcast_period_id'],
        'title': row['title'], 'series_title': row['series_title'],
        'episode_number': row['episode_number'], 'subtitle': row['subtitle'],
        'description': row['description'], 'detail': json.loads(row['detail']),
        'start_time': row['start_time'], 'end_time': row['end_time'], 'duration': row['duration'],
        'is_free': bool(row['is_free']), 'genres': json.loads(row['genres']),
        'primary_audio_type': row['primary_audio_type'],
        'primary_audio_language': row['primary_audio_language'],
        'secondary_audio_type': row['secondary_audio_type'],
        'secondary_audio_language': row['secondary_audio_language'],
        'created_at': row['created_at'], 'updated_at': row['updated_at'],
    }
    return json.loads(schemas.RecordedProgram.model_validate(recorded_program_dict).model_dump_json())


SELECT_QUERY = """
    SELECT
        rp.id AS rp_id, rp.recording_start_margin, rp.recording_end_margin, rp.is_partially_recorded,
        rp.channel_id, rp.network_id, rp.service_id, rp.event_id, rp.series_id, rp.series_broadcast_period_id,
        rp.title, rp.series_title, rp.episode_number, rp.subtitle, rp.description, rp.detail,
        rp.start_time, rp.end_time, rp.duration, rp.is_free, rp.genres,
        rp.primary_audio_type, rp.primary_audio_language, rp.secondary_audio_type, rp.secondary_audio_language,
        rp.created_at, rp.updated_at,
        rv.id AS rv_id, rv.status, rv.file_path, rv.file_hash, rv.file_size,
        rv.file_created_at, rv.file_modified_at, rv.recording_start_time, rv.recording_end_time,
        rv.duration AS video_duration, rv.container_format, rv.video_codec, rv.video_codec_profile,
        rv.video_scan_type, rv.video_frame_rate, rv.video_resolution_width, rv.video_resolution_height,
        rv.has_video_stream_changes, rv.primary_audio_codec, rv.primary_audio_channel,
        rv.primary_audio_sampling_rate, rv.secondary_audio_codec, rv.secondary_audio_channel,
        rv.secondary_audio_sampling_rate, rv.cm_sections, rv.thumbnail_info,
        rv.created_at AS rv_created_at, rv.updated_at AS rv_updated_at,
        ch.id AS ch_id, ch.display_channel_id, ch.network_id AS ch_network_id, ch.service_id AS ch_service_id,
        ch.transport_stream_id, ch.remocon_id, ch.channel_number, ch.type, ch.name AS ch_name,
        ch.jikkyo_force, ch.is_subchannel, ch.is_radiochannel, ch.is_watchable
    FROM recorded_programs rp
    JOIN recorded_videos rv ON rp.id = rv.recorded_program_id
    LEFT JOIN channels ch ON rp.channel_id = ch.id
"""


def main() -> int:
    base_url = sys.argv[1]
    workdir = Path(sys.argv[2])
    user_id = int(sys.argv[3])
    failures: list[str] = []

    program_ids: list[int] = json.loads((workdir / 'ids.json').read_text(encoding='utf-8'))['program_ids']
    connection = sqlite3.connect(workdir / 'server' / 'data' / 'database.sqlite')
    connection.row_factory = sqlite3.Row

    def fetch_expected(program_id: int) -> dict:
        row = connection.execute(SELECT_QUERY + ' WHERE rp.id = ?', (program_id,)).fetchone()
        return convert_row_to_recorded_program(dict(row))

    # ***** GET /api/videos *****
    status, body, _ = request('GET', f'{base_url}/api/videos')
    actual = json.loads(body)
    expected = {
        'total': len(program_ids),
        'recorded_programs': [fetch_expected(program_id) for program_id in reversed(program_ids)],
    }
    if status != 200:
        failures.append(f'GET /api/videos: status = {status}, body = {body[:200]}')
    elif actual != expected:
        failures.append(f'GET /api/videos: 応答が Python 版と異なります:\nactual = {json.dumps(actual, ensure_ascii=False)[:600]}\nwant   = {json.dumps(expected, ensure_ascii=False)[:600]}')
    else:
        print(f'GET /api/videos: OK ({actual["total"]} 件、Python 版と完全一致)')

    # ***** GET /api/videos?order=asc *****
    status, body, _ = request('GET', f'{base_url}/api/videos?order=asc')
    actual = json.loads(body)
    expected = {
        'total': len(program_ids),
        'recorded_programs': [fetch_expected(program_id) for program_id in program_ids],
    }
    if actual != expected:
        failures.append('GET /api/videos?order=asc: 応答が Python 版と異なります')
    else:
        print('GET /api/videos?order=asc: OK')

    # ***** GET /api/videos?ids=...&order=ids *****
    target_ids = list(reversed(program_ids))[:2]
    url = f'{base_url}/api/videos?order=ids&' + '&'.join(f'ids={program_id}' for program_id in target_ids)
    status, body, _ = request('GET', url)
    actual = json.loads(body)
    # Python 版は order=ids の場合、total には指定された ID の数のみを数える
    expected = {
        'total': len(target_ids),
        'recorded_programs': [fetch_expected(program_id) for program_id in target_ids],
    }
    if actual != expected:
        failures.append(f'GET /api/videos?order=ids: 応答が Python 版と異なります:\nactual = {json.dumps(actual, ensure_ascii=False)[:400]}\nwant   = {json.dumps(expected, ensure_ascii=False)[:400]}')
    else:
        print('GET /api/videos?order=ids: OK (指定された順序を維持)')

    # ***** GET /api/videos/search *****
    status, body, _ = request('GET', f'{base_url}/api/videos/search?query=%E3%83%86%E3%82%B9%E3%83%88%E3%83%89%E3%83%A9%E3%83%9E')
    actual = json.loads(body)
    drama_ids = program_ids[:2]
    expected = {
        'total': len(drama_ids),
        'recorded_programs': [fetch_expected(program_id) for program_id in reversed(drama_ids)],
    }
    if actual != expected:
        failures.append(f'GET /api/videos/search: 応答が Python 版と異なります:\nactual = {json.dumps(actual, ensure_ascii=False)[:400]}\nwant   = {json.dumps(expected, ensure_ascii=False)[:400]}')
    else:
        print('GET /api/videos/search: OK (部分一致検索)')

    # ***** GET /api/videos/{video_id} *****
    target_id = program_ids[0]
    status, body, _ = request('GET', f'{base_url}/api/videos/{target_id}')
    actual = json.loads(body)
    expected = fetch_expected(target_id)
    if actual != expected:
        failures.append(f'GET /api/videos/{target_id}: 応答が Python 版と異なります:\nactual = {json.dumps(actual, ensure_ascii=False)[:600]}\nwant   = {json.dumps(expected, ensure_ascii=False)[:600]}')
    else:
        print(f'GET /api/videos/{target_id}: OK (Python 版と完全一致)')

    # 存在しない ID
    status, body, _ = request('GET', f'{base_url}/api/videos/999999')
    if status != 422 or json.loads(body)['detail'] != 'Specified video_id was not found':
        failures.append(f'GET /api/videos/999999: status = {status}, body = {body[:200]}')
    else:
        print('GET /api/videos/999999: OK (422)')

    # ***** GET /api/videos/{video_id}/thumbnail *****
    # サムネイル画像がある録画番組
    status, body, headers = request('GET', f'{base_url}/api/videos/{program_ids[0]}/thumbnail')
    if status != 200 or body != b'webp-thumbnail':
        failures.append(f'GET /api/videos/{program_ids[0]}/thumbnail: status = {status}, body = {body[:50]}')
    elif 'public, no-transform, immutable, max-age=2592000' not in (header(headers, 'Cache-Control') or ''):
        failures.append(f'GET thumbnail: Cache-Control = {header(headers, "Cache-Control")}')
    else:
        print('GET /api/videos/{id}/thumbnail: OK (WebP サムネイル)')

        # ETag による 304
        etag = header(headers, 'ETag')
        status, body, _ = request('GET', f'{base_url}/api/videos/{program_ids[0]}/thumbnail', headers={'If-None-Match': etag})
        if status != 304 or body != b'':
            failures.append(f'GET thumbnail (If-None-Match): status = {status}, body = {body[:50]}')
        else:
            print('GET /api/videos/{id}/thumbnail (If-None-Match): OK (304)')

    # サムネイル画像がない録画番組 (デフォルトのサムネイル画像が返る)
    status, body, headers = request('GET', f'{base_url}/api/videos/{program_ids[2]}/thumbnail')
    default_thumbnail = (REPO_ROOT / 'server' / 'static' / 'thumbnails' / 'default.webp').read_bytes()
    if status != 200 or body != default_thumbnail:
        failures.append(f'GET /api/videos/{program_ids[2]}/thumbnail: status = {status}, body = {body[:50]}')
    elif header(headers, 'Cache-Control') != 'no-store, no-cache, must-revalidate, proxy-revalidate':
        failures.append(f'GET default thumbnail: Cache-Control = {header(headers, "Cache-Control")}')
    else:
        print('GET /api/videos/{id}/thumbnail (サムネイルなし): OK (デフォルト画像)')

    # タイルサムネイル
    status, body, _ = request('GET', f'{base_url}/api/videos/{program_ids[0]}/thumbnail/tiled')
    if status != 200 or body != b'webp-tile-thumbnail':
        failures.append(f'GET /api/videos/{program_ids[0]}/thumbnail/tiled: status = {status}, body = {body[:50]}')
    else:
        print('GET /api/videos/{id}/thumbnail/tiled: OK')

    # ***** GET /api/videos/{video_id}/download *****
    status, body, headers = request('GET', f'{base_url}/api/videos/{program_ids[0]}/download')
    if status != 200 or len(body) != 1024:
        failures.append(f'GET /api/videos/{program_ids[0]}/download: status = {status}, length = {len(body)}')
    elif header(headers, 'Content-Type') != 'video/mp2t':
        failures.append(f'GET download: Content-Type = {header(headers, "Content-Type")}')
    else:
        print('GET /api/videos/{id}/download: OK (video/mp2t)')

        # Range リクエスト (動画プレイヤーのシークで使われる)
        status, body, headers = request(
            'GET', f'{base_url}/api/videos/{program_ids[0]}/download', headers={'Range': 'bytes=0-99'},
        )
        if status != 206 or len(body) != 100 or 'bytes 0-99/1024' not in (header(headers, 'Content-Range') or ''):
            failures.append(f'GET download (Range): status = {status}, length = {len(body)}, Content-Range = {header(headers, "Content-Range")}')
        else:
            print('GET /api/videos/{id}/download (Range): OK (206)')

    # ***** GET /api/videos/{video_id}/jikkyo *****
    status, body, _ = request('GET', f'{base_url}/api/videos/{program_ids[0]}/jikkyo')
    jikkyo_response = json.loads(body)
    if status != 200 or 'is_success' not in jikkyo_response or 'comments' not in jikkyo_response or 'detail' not in jikkyo_response:
        failures.append(f'GET /api/videos/{program_ids[0]}/jikkyo: status = {status}, body = {body[:200]}')
    else:
        print(f'GET /api/videos/{{id}}/jikkyo: OK (is_success = {jikkyo_response["is_success"]})')

    # ***** POST /api/videos/{video_id}/reanalyze (プロキシ) *****
    status, body, _ = request('POST', f'{base_url}/api/videos/{program_ids[0]}/reanalyze')
    if status != 404:
        failures.append(f'POST reanalyze (プロキシ無効時): status = {status}, want 404')
    else:
        print('POST /api/videos/{id}/reanalyze: OK (プロキシ無効時は 404)')

    # ***** DELETE /api/videos/{video_id} *****
    from app.routers.UsersRouter import GenerateAccessToken  # noqa: E402
    token = GenerateAccessToken(user_id)

    # 未ログインの場合は 401
    status, body, _ = request('DELETE', f'{base_url}/api/videos/{program_ids[1]}')
    if status != 401:
        failures.append(f'DELETE (未ログイン): status = {status}, want 401')

    target_id = program_ids[1]
    row = connection.execute('SELECT file_path, file_hash FROM recorded_videos WHERE recorded_program_id = ?', (target_id,)).fetchone()
    file_path = Path(row['file_path'])
    file_hash = row['file_hash']

    status, body, _ = request('DELETE', f'{base_url}/api/videos/{target_id}', token=token)
    if status != 204:
        failures.append(f'DELETE /api/videos/{target_id}: status = {status}, body = {body[:200]}')
    else:
        # データベースから削除されている
        remaining = connection.execute('SELECT COUNT(*) FROM recorded_programs WHERE id = ?', (target_id,)).fetchone()[0]
        remaining_videos = connection.execute('SELECT COUNT(*) FROM recorded_videos WHERE recorded_program_id = ?', (target_id,)).fetchone()[0]
        if remaining != 0 or remaining_videos != 0:
            failures.append(f'DELETE: DB のレコードが削除されていません (programs = {remaining}, videos = {remaining_videos})')
        # ファイルが削除されている
        deleted_files = [file_path, Path(str(file_path) + '.program.txt'), Path(str(file_path) + '.err')]
        for deleted_file in deleted_files:
            if deleted_file.exists():
                failures.append(f'DELETE: {deleted_file} が削除されていません')
        if failures:
            pass
        else:
            print(f'DELETE /api/videos/{target_id}: OK (DB・録画ファイル・補助ファイルを削除)')

    connection.close()

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
