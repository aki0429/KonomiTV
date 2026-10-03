"""チャンネル一覧 API の E2E 検証 (実スキーマの DB + 実際の HTTP サーバー) 。

tools/e2e_channels_setup.py でデータを投入した DB を使い、
server-go のサーバー (例: ./konomitv-go.exe -no-proxy -listen 127.0.0.77:7005 -server-dir <server>) に対して検証する。
"""

import datetime
import json
import sqlite3
import sys
import urllib.error
import urllib.request

# 使い方: python tools/e2e_channels_verify.py <database.sqlite のパス> [http://127.0.0.77:7005]
BASE = sys.argv[2] if len(sys.argv) > 2 else 'http://127.0.0.77:7005'
DB_PATH = sys.argv[1]
JST = datetime.timezone(datetime.timedelta(hours=9))

LIVE_CHANNEL_KEYS = {
    'id', 'display_channel_id', 'network_id', 'service_id', 'transport_stream_id', 'remocon_id',
    'channel_number', 'type', 'name', 'terrestrial_regions', 'jikkyo_force', 'is_subchannel',
    'is_radiochannel', 'is_watchable', 'is_display', 'viewer_count', 'program_present', 'program_following',
}
PROGRAM_KEYS = {
    'id', 'channel_id', 'network_id', 'service_id', 'event_id', 'title', 'description', 'detail',
    'start_time', 'end_time', 'duration', 'is_free', 'genres', 'video_type', 'video_codec',
    'video_resolution', 'primary_audio_type', 'primary_audio_language', 'primary_audio_sampling_rate',
    'secondary_audio_type', 'secondary_audio_language', 'secondary_audio_sampling_rate',
}
IPTV_KEYS = LIVE_CHANNEL_KEYS


def get(path: str, headers: dict | None = None) -> tuple[int, dict, bytes]:
    request = urllib.request.Request(BASE + path, headers=headers or {})
    try:
        with urllib.request.urlopen(request, timeout=120) as response:
            return response.status, dict(response.headers), response.read()
    except urllib.error.HTTPError as error:
        return error.code, dict(error.headers), error.read()


def parse_db_time(value: str) -> datetime.datetime:
    parsed = datetime.datetime.fromisoformat(value.replace(' ', 'T'))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=JST)
    return parsed.astimezone(JST)


def main() -> None:
    con = sqlite3.connect(DB_PATH)
    con.row_factory = sqlite3.Row

    # ***** DB から期待値を組み立てる *****
    channels = con.execute(
        'SELECT * FROM channels WHERE is_watchable = 1 ORDER BY remocon_id ASC, channel_number ASC'
    ).fetchall()
    now = datetime.datetime.now(JST)
    programs = con.execute(
        """
        SELECT * FROM (
            SELECT
                DENSE_RANK() OVER (PARTITION BY channel_id ORDER BY start_time ASC) AS program_order,
                CASE WHEN "start_time" <= (?) THEN 1 ELSE 0 END AS is_present,
                *
            FROM "programs"
            WHERE
                ("start_time" <= (?) AND (?) <= "end_time")
                OR
                ((?) <= "start_time" AND "start_time" <= (?))
        ) WHERE program_order <= 2
        """,
        tuple(value.isoformat(sep=' ') for value in (
            now, now, now, now, now + datetime.timedelta(hours=24),
        )),
    ).fetchall()
    expected_programs = {}
    for row in programs:
        expected_programs.setdefault((row['network_id'], row['service_id']), []).append(row)

    def expected_titles(channel) -> tuple[str | None, str | None]:
        rows = expected_programs.get((channel['network_id'], channel['service_id']), [])
        if len(rows) == 0:
            return None, None
        if len(rows) == 1:
            return (rows[0]['title'], None) if rows[0]['is_present'] else (None, rows[0]['title'])
        if rows[0]['is_present'] and rows[1]['is_present']:
            return rows[0]['title'], None
        if not rows[0]['is_present'] and not rows[1]['is_present']:
            for row in rows:
                if row['program_order'] == 1:
                    return None, row['title']
            return None, None
        present = next((row['title'] for row in rows if row['is_present']), None)
        following = next((row['title'] for row in rows if not row['is_present']), None)
        return present, following

    # ***** サーバーから取得 *****
    status, _, body = get('/api/channels')
    assert status == 200, status
    data = json.loads(body.decode('utf-8'))
    assert set(data.keys()) == {'GR', 'BS', 'CS', 'CATV', 'SKY', 'BS4K', 'IPTV'}, data.keys()

    # チャンネルタイプごとに分類されている
    by_type = {}
    for channel_type, entries in data.items():
        if channel_type == 'IPTV':
            continue
        by_type[channel_type] = entries
    assert len(by_type['GR']) == 5, len(by_type['GR'])
    assert len(by_type['BS']) == 1 and len(by_type['CS']) == 1 and len(by_type['CATV']) == 1
    assert len(by_type['SKY']) == 1 and len(by_type['BS4K']) == 1
    assert data['IPTV'] == []
    print('[OK] channel grouping')

    # 並び順は remocon_id → channel_number
    gr_ids = [channel['id'] for channel in by_type['GR']]
    assert gr_ids == [row['id'] for row in channels if row['type'] == 'GR'], gr_ids
    print('[OK] channel order')

    # 各チャンネルの内容を DB と照合する
    for row in channels:
        entries = by_type[row['type']]
        channel = next((entry for entry in entries if entry['id'] == row['id']), None)
        assert channel is not None, row['id']
        assert set(channel.keys()) == LIVE_CHANNEL_KEYS, set(channel.keys()) ^ LIVE_CHANNEL_KEYS
        assert channel['display_channel_id'] == row['display_channel_id']
        assert channel['network_id'] == row['network_id'] and channel['service_id'] == row['service_id']
        assert channel['remocon_id'] == row['remocon_id'] and channel['channel_number'] == row['channel_number']
        assert channel['name'] == row['name']
        assert channel['is_watchable'] is True and channel['viewer_count'] == 0
        assert channel['transport_stream_id'] is None and channel['jikkyo_force'] is None
        assert channel['is_subchannel'] == bool(row['is_subchannel'])
        assert channel['is_radiochannel'] == bool(row['is_radiochannel'])
        # サブチャンネルで現在放送中の番組がない場合は非表示
        expected_display = not (bool(row['is_subchannel']) and expected_titles(row)[0] is None)
        assert channel['is_display'] == expected_display, (row['id'], channel['is_display'], expected_display)
        # 地域名は地デジのみ
        if row['type'] == 'GR':
            assert isinstance(channel['terrestrial_regions'], list) and len(channel['terrestrial_regions']) > 0
        else:
            assert channel['terrestrial_regions'] is None, channel['terrestrial_regions']

        # 現在/次の番組
        present_title, following_title = expected_titles(row)
        assert (channel['program_present'] or {}).get('title') == present_title, (row['id'], channel, present_title)
        assert (channel['program_following'] or {}).get('title') == following_title, (row['id'], channel, following_title)
        for program in (channel['program_present'], channel['program_following']):
            if program is None:
                continue
            assert set(program.keys()) == PROGRAM_KEYS, set(program.keys()) ^ PROGRAM_KEYS
            assert program['detail'] == {'テスト': '値'}
            assert program['genres'] == [{'major': 'ニュース／報道', 'middle': '国内'}]
            assert program['is_free'] is True
            assert isinstance(program['duration'], float)
            assert parse_db_time(program['start_time']).tzinfo is not None
            assert program['start_time'].endswith('+09:00'), program['start_time']
            assert program['secondary_audio_type'] is None
    print('[OK] channel and program contents match the database')

    # 視聴不可チャンネルは含まれない
    all_ids = [channel['id'] for entries in by_type.values() for channel in entries]
    assert 'NID32738-SID1029' not in all_ids
    print('[OK] unwatchable channel is excluded')

    # ***** IPTV の疑似チャンネル *****
    status, _, body = get('/api/iptv/channels?per_page=1')
    assert status == 200
    iptv_channel = json.loads(body.decode('utf-8'))['channels'][0]

    register = urllib.request.Request(
        BASE + '/api/iptv/tvui',
        data=json.dumps({'display_channel_id': iptv_channel['display_channel_id']}).encode(),
        headers={'Content-Type': 'application/json'},
        method='POST',
    )
    with urllib.request.urlopen(register, timeout=60) as response:
        cookie = response.headers.get('Set-Cookie', '').split(';')[0]

    status, _, body = get('/api/channels', {'Cookie': cookie})
    assert status == 200
    data = json.loads(body.decode('utf-8'))
    assert len(data['IPTV']) == 1, data['IPTV']
    entry = data['IPTV'][0]
    assert set(entry.keys()) == IPTV_KEYS, set(entry.keys()) ^ IPTV_KEYS
    assert entry['id'] == 'IPTV-' + iptv_channel['display_channel_id']
    assert entry['type'] == 'IPTV' and entry['display_channel_id'] == iptv_channel['display_channel_id']
    assert entry['name'] == f'{iptv_channel["name"]} ({iptv_channel["country_name"]})'
    assert entry['network_id'] == 0 and entry['service_id'] == 0 and entry['remocon_id'] == 0
    assert entry['transport_stream_id'] is None and entry['terrestrial_regions'] is None
    assert entry['jikkyo_force'] is None and entry['is_subchannel'] is False
    assert entry['is_radiochannel'] is False and entry['is_watchable'] is True and entry['is_display'] is True
    assert entry['viewer_count'] == 0
    assert entry['program_present'] is None and entry['program_following'] is None
    print('[OK] IPTV pseudo channel')

    # ***** 単体のチャンネル API との整合性 *****
    for channel_id in ['NID32736-SID1024', 'gr011', 'NID32736-SID1024']:
        status, _, body = get('/api/channels/' + channel_id)
        assert status == 200, status
        detail = json.loads(body.decode('utf-8'))
        assert detail['id'] == 'NID32736-SID1024'
        assert detail['program_present']['title'] == '放送中の番組'
    print('[OK] channel detail API')

    print('ALL CHANNELS E2E CHECKS PASSED')


if __name__ == '__main__':
    main()
