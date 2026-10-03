"""実データ (iptv-org のプレイリスト) を使った Python 版 IPTVUtil の期待値を生成する。

server/ で `uv run python ../server-go/tools/generate_iptv_real_fixture.py [出力先]` として実行する。
生成後、`KONOMITV_IPTV_REAL_FIXTURE=<出力先> go test ./internal/iptv/ -run TestRealPlaylistParity` で
Go 版との完全一致を検証できる (出力先の既定値は server-go/tmp_real_iptv 、.gitignore 済み) 。
"""

import json
import sys
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))

from app.utils import IPTVUtil  # noqa: E402

PLAYLIST_URL = 'https://iptv-org.github.io/iptv/index.m3u'
COUNTRIES_URL = 'https://iptv-org.github.io/api/countries.json'
OUTPUT_DIR = Path(sys.argv[1]) if len(sys.argv) > 1 else REPO_ROOT / 'server-go' / 'tmp_real_iptv'


def fetch(url: str) -> bytes:
    request = urllib.request.Request(url, headers={'User-Agent': 'TestAgent/1.0'})
    with urllib.request.urlopen(request, timeout=120) as response:
        return response.read()


def main() -> None:
    OUTPUT_DIR.mkdir(exist_ok=True)
    playlist_path = OUTPUT_DIR / 'index.m3u'
    countries_path = OUTPUT_DIR / 'countries.json'
    if playlist_path.exists() is False:
        playlist_path.write_bytes(fetch(PLAYLIST_URL))
    if countries_path.exists() is False:
        countries_path.write_bytes(fetch(COUNTRIES_URL))

    content = playlist_path.read_text(encoding='utf-8')
    countries = json.loads(countries_path.read_text(encoding='utf-8'))
    countries_by_code = {}
    for country in countries:
        if isinstance(country.get('code'), str):
            countries_by_code[country['code'].upper()] = {
                'name': str(country.get('name') or country['code']),
                'flag': str(country.get('flag') or ''),
            }

    IPTVUtil._countries_by_code = countries_by_code
    channels = IPTVUtil.ParseM3UPlaylist(content, PLAYLIST_URL)
    channels.sort(key=lambda channel: (
        (channel.country_name or '\uffff').lower(),
        (channel.group or '\uffff').lower(),
        channel.name.lower(),
    ))
    IPTVUtil._channels = channels
    IPTVUtil._BuildDisplayChannelIDMap()

    expected = {
        'total': len(channels),
        'countries': IPTVUtil.GetCountries(),
        'groups': IPTVUtil.GetGroups(),
        'channels': [
            {
                'id': channel.id,
                'name': channel.name,
                'url': channel.url,
                'country': channel.country,
                'group': channel.group,
                'display_channel_id': IPTVUtil.BuildDisplayChannelID(channel.url),
                'proxy_url': IPTVUtil.BuildProxyURL(channel.url),
                'stream_type': IPTVUtil.DetectStreamType(channel.url),
                'header_user_agent': channel.user_agent,
                'header_referrer': channel.referrer,
            }
            for channel in channels
        ],
    }
    (OUTPUT_DIR / 'expected.json').write_text(
        json.dumps(expected, ensure_ascii=False),
        encoding='utf-8',
    )
    print(f'{expected["total"]} channels, {len(expected["countries"])} countries, {len(expected["groups"])} groups')
    print(OUTPUT_DIR / 'expected.json')


if __name__ == '__main__':
    main()
