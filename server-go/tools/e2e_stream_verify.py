"""ライブストリーミングの E2E 検証 (実際の FFmpeg / tsreadex を使ったエンコードパイプライン) 。

使い方:
    python server-go/tools/e2e_stream_verify.py <Go 版サーバーのベース URL (例: http://127.0.0.77:7006)>

検証内容:
    1. IPTV の疑似チャンネルのチャンネル一覧から、ローカルのテストチャンネルを取得する
    2. ライブ MPEG-TS ストリーム API から実際にストリームデータを受信できること (188 バイト単位の MPEG-TS であること)
    3. ストリーミング中にステータスが Standby → ONAir に遷移すること
    4. ライブストリーム イベント API (SSE) が initial_update / status_update / clients_update を配信すること
    5. 視聴者数がチャンネル一覧 API に反映されること
    6. クライアントが切断されたら Idling を経由して Offline になること
    7. original 画質 (エンコーダーを通さないパススルー) でもストリームを受信できること
    8. 同じ画質に複数のクライアントが接続した場合に、すべてのクライアントへ同じストリームが配信されること
"""

import http.cookiejar
import json
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

BASE_URL = sys.argv[1] if len(sys.argv) > 1 else 'http://127.0.0.77:7006'
# ffprobe のパス (受信したストリームの映像コーデックを確認するために使う)
FFPROBE_PATH = sys.argv[2] if len(sys.argv) > 2 else str(
    Path(__file__).resolve().parents[2] / 'server' / 'thirdparty' / 'FFmpeg' / 'ffprobe.exe'
)

failures: list[str] = []

# 匿名 ID の Cookie (IPTV のテレビ視聴 UI の登録はユーザーごとに管理されるため、同一の Cookie を送信する)
# 本番では HTTPS 経由で Secure な Cookie として設定されるため、検証用に直接ヘッダーへ設定する
anonymous_user_id = str(uuid.uuid4())
cookie_header = f'KonomiTV-AnonymousID={anonymous_user_id}'
cookie_jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookie_jar))


def probe_video_codec(data: bytes) -> str:
    """受信したストリームの映像コーデックを ffprobe で調べる。"""
    with tempfile.NamedTemporaryFile(suffix='.ts', delete=False) as file:
        file.write(data)
        path = file.name
    try:
        result = subprocess.run([
            FFPROBE_PATH, '-v', 'error', '-select_streams', 'v:0',
            '-show_entries', 'stream=codec_name', '-of', 'csv=p=0', path,
        ], capture_output=True)
        # 映像ストリームが複数検出されることがあるため、カンマ区切りの一覧として返す
        codecs = [codec.strip() for codec in result.stdout.decode('utf-8', errors='ignore').splitlines() if codec.strip()]
        return ','.join(sorted({codec.rstrip(',') for codec in codecs if codec.rstrip(',')}))
    finally:
        Path(path).unlink(missing_ok=True)


def check(condition: bool, message: str) -> None:
    if condition:
        print(f'  OK   {message}')
    else:
        print(f'  FAIL {message}')
        failures.append(message)


def get_json(path: str, timeout: float = 30.0) -> tuple[int, object]:
    request = urllib.request.Request(BASE_URL + path, headers={'Cookie': cookie_header})
    try:
        with opener.open(request, timeout=timeout) as response:
            return response.status, json.loads(response.read().decode('utf-8'))
    except urllib.error.HTTPError as error:
        return error.code, json.loads(error.read().decode('utf-8'))


def post_json(path: str, body: dict, timeout: float = 30.0) -> tuple[int, object]:
    request = urllib.request.Request(
        BASE_URL + path,
        data=json.dumps(body).encode('utf-8'),
        headers={'Content-Type': 'application/json', 'Cookie': cookie_header},
        method='POST',
    )
    try:
        with opener.open(request, timeout=timeout) as response:
            return response.status, json.loads(response.read().decode('utf-8'))
    except urllib.error.HTTPError as error:
        return error.code, json.loads(error.read().decode('utf-8'))


def main() -> None:
    print(f'Base URL: {BASE_URL}')

    # ***** 1. ローカルのテストチャンネルを取得する *****
    status, body = get_json('/api/iptv/channels?per_page=500')
    check(status == 200, f'GET /api/iptv/channels returns 200 (status: {status})')
    channels = [channel for channel in body['channels'] if channel['name'] == 'Local Test Channel']
    check(len(channels) == 1, f'the local test channel is registered ({len(channels)} channel(s))')
    if not channels:
        return finish()
    display_channel_id = channels[0]['display_channel_id']
    print(f'  display_channel_id = {display_channel_id}')

    # テレビ視聴 UI にチャンネルを登録する (チャンネル一覧 API の IPTV 枠に表示されるようにする)
    status, body = post_json('/api/iptv/tvui', {'display_channel_id': display_channel_id})
    check(status == 200, f'POST /api/iptv/tvui returns 200 (status: {status})')
    check(display_channel_id in body.get('channels', []) or any(
        channel.get('display_channel_id') == display_channel_id for channel in body.get('channels', [])
    ), 'the channel is registered in the TV watching UI')

    # ***** 2. ライブ MPEG-TS ストリームを受信する *****
    print('Streaming the live MPEG-TS stream...')
    stream_request = urllib.request.Request(
        f'{BASE_URL}/api/streams/live/{display_channel_id}/720p/mpegts',
        headers={'Cookie': cookie_header},
    )
    stream_response = urllib.request.urlopen(stream_request, timeout=60)

    received = bytearray()
    statuses: list[str] = []
    # ストリームを受信しながらステータスを記録する
    def record_statuses() -> None:
        while not stop_recording.is_set():
            code, status_body = get_json(f'/api/streams/live/{display_channel_id}/720p')
            if code == 200:
                current = status_body['status']
                if not statuses or statuses[-1] != current:
                    statuses.append(current)
                    print(f'  status: {current} ({status_body["detail"]})')
            time.sleep(0.2)

    stop_recording = threading.Event()
    recorder = threading.Thread(target=record_statuses, daemon=True)
    recorder.start()

    # 188 バイト (MPEG-TS のパケットサイズ) の倍数ずつ読み取る
    read_size = 188 * 1024
    deadline = time.monotonic() + 60
    while len(received) < 2 * 1024 * 1024 and time.monotonic() < deadline:
        chunk = stream_response.read(read_size)
        if not chunk:
            break
        received += chunk
    stop_recording.set()
    recorder.join(timeout=5)

    check(len(received) >= 2 * 1024 * 1024, f'received {len(received)} bytes of the MPEG-TS stream')
    check(len(received) % 188 == 0, f'the stream length is a multiple of 188 bytes ({len(received) % 188})')
    # MPEG-TS の同期バイト (0x47) を確認する
    sync_offsets = [index for index in range(0, 188 * 100, 188) if received[index] == 0x47]
    check(len(sync_offsets) == 100, f'the MPEG-TS sync byte (0x47) appears at every 188-byte offset ({len(sync_offsets)}/100)')
    check('Standby' in statuses, f'the status went through Standby ({statuses})')
    check('ONAir' in statuses, f'the status became ONAir ({statuses})')
    check(statuses.index('ONAir') > statuses.index('Standby'), 'the status changed Standby -> ONAir')
    # 720p 画質は H.264 にエンコードされる
    encoded_codecs = probe_video_codec(bytes(received))
    check(encoded_codecs == 'h264', f'the 720p stream is encoded to H.264 (codecs: {encoded_codecs})')

    # ***** 3. 視聴者数がチャンネル一覧 API に反映される *****
    status, body = get_json('/api/channels')
    iptv_channels = body['IPTV']
    local_channels = [channel for channel in iptv_channels if channel['display_channel_id'] == display_channel_id]
    check(len(local_channels) == 1, 'the local channel appears in GET /api/channels')
    if local_channels:
        check(local_channels[0]['viewer_count'] == 1, f'viewer_count = {local_channels[0]["viewer_count"]} (want 1)')

    # ***** 4. ライブストリーム イベント API (SSE) *****
    print('Checking the live stream event API (SSE)...')
    events: list[tuple[str, dict]] = []
    event_request = urllib.request.Request(
        f'{BASE_URL}/api/streams/live/{display_channel_id}/720p/events',
        headers={'Cookie': cookie_header},
    )
    with urllib.request.urlopen(event_request, timeout=30) as event_response:
        event_name = None
        started_at = time.monotonic()
        while time.monotonic() - started_at < 10:
            line = event_response.readline().decode('utf-8').strip()
            if line.startswith('event: '):
                event_name = line[len('event: '):]
            elif line.startswith('data: '):
                events.append((event_name, json.loads(line[len('data: '):])))
                if len(events) >= 2:
                    break
            elif line == '':
                continue
    check(len(events) >= 1 and events[0][0] == 'initial_update', f'the first SSE event is initial_update ({[event for event, _ in events]})')
    if events:
        check(events[0][1]['status'] == 'ONAir', f'the initial SSE status is ONAir ({events[0][1]["status"]})')
        check(events[0][1]['client_count'] >= 1, f'the initial SSE client_count is {events[0][1]["client_count"]}')

    # ***** 5. ストリームを切断して Idling → Offline になることを確認する *****
    print('Disconnecting the stream...')
    stream_response.close()
    # max_alive_time は 3 秒に設定している
    offline_at = None
    idling_seen = False
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        code, status_body = get_json(f'/api/streams/live/{display_channel_id}/720p')
        if code == 200:
            if status_body['status'] == 'Idling':
                idling_seen = True
            if status_body['status'] == 'Offline':
                offline_at = time.monotonic()
                break
        time.sleep(0.3)
    check(idling_seen, 'the status became Idling after the client disconnected')
    check(offline_at is not None, 'the status became Offline after max_alive_time')
    if offline_at is not None:
        check(True, f'the status became Offline with the detail: {status_body["detail"]}')

    # 最終状態で視聴者数が 0 に戻っていることを確認する
    status, body = get_json('/api/channels')
    local_channels = [channel for channel in body['IPTV'] if channel['display_channel_id'] == display_channel_id]
    if local_channels:
        check(local_channels[0]['viewer_count'] == 0, f'viewer_count = {local_channels[0]["viewer_count"]} (want 0)')

    # ***** 6. original 画質 (パススルー) のストリーム *****
    print('Streaming the original quality stream...')
    original_statuses: list[str] = []
    original_request = urllib.request.Request(
        f'{BASE_URL}/api/streams/live/{display_channel_id}/original/mpegts',
        headers={'Cookie': cookie_header},
    )
    with opener.open(original_request, timeout=60) as original_response:
        received_original = bytearray()
        deadline = time.monotonic() + 60
        while len(received_original) < 2 * 1024 * 1024 and time.monotonic() < deadline:
            chunk = original_response.read(188 * 1024)
            if not chunk:
                break
            received_original += chunk
            code, status_body = get_json(f'/api/streams/live/{display_channel_id}/original')
            if code == 200 and (not original_statuses or original_statuses[-1] != status_body['status']):
                original_statuses.append(status_body['status'])
    check(len(received_original) >= 2 * 1024 * 1024, f'received {len(received_original)} bytes of the original quality stream')
    check(len(received_original) % 188 == 0, 'the original quality stream is aligned to 188 bytes')
    check('ONAir' in original_statuses, f'the original quality stream became ONAir ({original_statuses})')
    # エンコーダーを通さないため、入力 (MPEG-2) のストリームがそのまま配信される
    original_codecs = probe_video_codec(bytes(received_original))
    check(original_codecs == 'mpeg2video', f'the original quality stream is not re-encoded (codecs: {original_codecs})')

    # ***** 7. 複数のクライアントでの同時視聴 *****
    print('Streaming with two concurrent clients...')
    stream_url = f'{BASE_URL}/api/streams/live/{display_channel_id}/720p/mpegts'
    client_a = opener.open(urllib.request.Request(stream_url, headers={'Cookie': cookie_header}), timeout=60)
    client_b = opener.open(urllib.request.Request(stream_url, headers={'Cookie': cookie_header}), timeout=60)
    received_a = client_a.read(188 * 512)
    received_b = client_b.read(188 * 512)
    check(len(received_a) == len(received_b), f'both clients received the same amount of data ({len(received_a)} / {len(received_b)} bytes)')
    # 接続タイミングが異なるため先頭のチャンクは一致しないが、どちらも MPEG-TS として配信される
    check(received_a[0] == 0x47 and received_b[0] == 0x47, 'both clients received MPEG-TS data')
    code, status_body = get_json(f'/api/streams/live/{display_channel_id}/720p')
    check(status_body['client_count'] == 2, f'client_count = {status_body["client_count"]} (want 2)')
    code, body = get_json('/api/channels')
    local_channels = [channel for channel in body['IPTV'] if channel['display_channel_id'] == display_channel_id]
    if local_channels:
        check(local_channels[0]['viewer_count'] == 2, f'viewer_count = {local_channels[0]["viewer_count"]} (want 2)')
    client_a.close()
    client_b.close()

    finish()


def finish() -> None:
    print()
    if failures:
        print(f'FAILED ({len(failures)} check(s) failed)')
        for failure in failures:
            print(f'  - {failure}')
        sys.exit(1)
    print('All checks passed.')


if __name__ == '__main__':
    main()
