"""IPTVUtil のパリティ検証用フィクスチャを生成する (Go 版のテストから参照する) 。

server/ で `uv run python ../server-go/tools/generate_iptv_parity_fixture.py` として実行する。
生成先: server-go/internal/iptv/testdata/iptv_parity.json
"""

import json
import sys
from dataclasses import asdict
from pathlib import Path
from urllib.parse import urljoin

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))

from app.utils import IPTVUtil  # noqa: E402

PLAYLIST = """#EXTM3U
#EXTINF:-1 tvg-id="NHKWorldJapan.jp@SD" tvg-logo="https://example.com/nhk.png" group-title="News",NHK World-Japan (1080p)
https://example.com/nhk/index.m3u8
#EXTINF:-1 tvg-country="US;GB" http-user-agent="CustomAgent/1.0",Some US Channel
https://example.com/us.ts
#EXTGRP:Music
#EXTVLCOPT:http-referrer=https://example.com/
#EXTINF:-1,Music Channel [Geo-blocked]
relative/stream.m3u8
#EXTINF:-1 tvg-name="Fallback Name", 
https://example.com/dup.m3u8
https://example.com/dup.m3u8
#EXTINF:-1,Raw URL Only
?token=abc&x=1
#EXTINF:-1 tvg-language="ja" tvg-logo="",MP4 Channel
https://example.com/video.mp4?a=1&hls=1
#EXTINF:-1 group-title="News" tvg-id="Invalid.toolong",Comma, in name, with (720p)
https://example.com/comma.m3u8
#EXTINF:-1 tvg-country="jp",Lowercase Country
#EXTGRP:Music
#EXTVLCOPT:http-user-agent=ExtVlcAgent/2.0
#EXTVLCOPT:http-referrer=https://music.example.com/
https://example.com/music/index.m3u8
#EXTINF:-1 tvg-country="",Empty Country
https://example.com/empty.m3u8
#EXTINF:-1 tvg-id="Foo.us",US by tvg-id
https://example.com/byid.m3u8
https://example.com/bare-url.ts
#EXTINF:-1,Bare Relative
stream.ts
"""

SOURCE_URL = 'https://iptv-org.github.io/iptv/countries/jp.m3u'

COUNTRIES = {
    'JP': {'name': 'Japan', 'flag': '🇯🇵'},
    'US': {'name': 'United States', 'flag': '🇺🇸'},
    'GB': {'name': 'United Kingdom', 'flag': '🇬🇧'},
}

HLS_MASTER = """#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1280000,RESOLUTION=640x360,CODECS="avc1.4d401f,mp4a.40.2"
360p.m3u8
#EXT-X-STREAM-INF:AVERAGE-BANDWIDTH=12800000,RESOLUTION=3840x2160,CODECS="hvc1.1.6.L150.90"
2160p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080
1080p.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=9000000,RESOLUTION=1920x1080,CODECS="av01.0.08M.08"
1080p-high.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=800000
240p.m3u8
"""

HLS_MEDIA = """#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:4
#EXTINF:4.000,
segment1.ts
#EXTINF:4.000,
segment2.ts
"""


def main() -> None:
    IPTVUtil._countries_by_code = dict(COUNTRIES)
    channels = IPTVUtil.ParseM3UPlaylist(PLAYLIST, SOURCE_URL)
    fixture = {
        'source_url': SOURCE_URL,
        'playlist': PLAYLIST,
        'countries': COUNTRIES,
        'channels': [IPTVUtil.ChannelToDict(channel) for channel in channels],
        'raw_channels': [asdict(channel) for channel in channels],
        'tvui_registry_json': json.dumps(
            {'user:1': ['iptv1', 'iptv2'], 'anon:00000000-0000-0000-0000-000000000000': []},
            ensure_ascii=False,
            indent=2,
        ),
        'user_sources_json': json.dumps(['https://example.com/a.m3u', 'https://example.com/b.m3u'], ensure_ascii=False, indent=2),
        'display_channel_ids': {channel.url: IPTVUtil.BuildDisplayChannelID(channel.url) for channel in channels},
        'proxy_urls': {channel.url: IPTVUtil.BuildProxyURL(channel.url) for channel in channels},
        'quality_names': {
            'NHK World-Japan (1080p)': IPTVUtil.ParseQualityFromName('NHK World-Japan (1080p)'),
            'NHK World-Japan [720p]': IPTVUtil.ParseQualityFromName('NHK World-Japan [720p]'),
            'NHK World-Japan （480i）': IPTVUtil.ParseQualityFromName('NHK World-Japan （480i）'),
            'No Quality': IPTVUtil.ParseQualityFromName('No Quality'),
            'Almost 1080': IPTVUtil.ParseQualityFromName('Almost 1080'),
            '4K (2160p)': IPTVUtil.ParseQualityFromName('4K (2160p)'),
        },
        'hls_master_qualities': IPTVUtil.ParseHLSQualities(HLS_MASTER),
        'hls_media_qualities': IPTVUtil.ParseHLSQualities(HLS_MEDIA),
        'rewritten_master': IPTVUtil.RewriteHLSPlaylist(HLS_MASTER, 'https://example.com/live/master.m3u8'),
        'rewritten_media': IPTVUtil.RewriteHLSPlaylist(
            '#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x1\n#EXT-X-MAP:URI="init.mp4"\nseg1.ts\n\n',
            'https://example.com/live/out/index.m3u8',
        ),
        'normalize_urls': [
            {'base': base, 'target': target, 'expected': urljoin(base, target.strip())}
            for base, target in [
                ('https://example.com/a/b.m3u8', 'c.m3u8'),
                ('https://example.com/a/b.m3u8', '/c.m3u8'),
                ('https://example.com/a/b.m3u8', 'https://other.com/d.m3u8'),
                ('https://example.com/a/b.m3u8', '//other.com/d.m3u8'),
                ('https://example.com/a/b.m3u8', '?x=1'),
                ('https://example.com/a/b.m3u8', '#frag'),
                ('https://example.com/a/b.m3u8', ''),
                ('https://example.com/a/b.m3u8', ' c.m3u8 '),
                ('https://example.com/a/b.m3u8', '../c.m3u8'),
                ('https://example.com/a/b.m3u8', './c.m3u8'),
                ('https://example.com/a/b.m3u8', '../../../../c.m3u8'),
                ('https://example.com/a/b.m3u8', 'x/./y/../z.m3u8'),
                ('https://example.com/a/b.m3u8', '//other.com//double//slash.m3u8'),
                ('https://example.com/a//b/c.m3u8', 'd.m3u8'),
                ('https://example.com/ssh101/ssh101/Cosmovisión/playlist.m3u8', 'chunk.m3u8'),
                ('https://example.com/a/b.m3u8', 'chunk.m3u8;session=abc'),
                ('https://example.com/a/b.m3u8?token=1', 'c.m3u8'),
                ('https://example.com/a/b.m3u8?token=1', '?other=2'),
                ('https://example.com/a/b.m3u8?token=1', ''),
                ('http://146.88.62.69:1935/Transcoder/มายาHD.stream_576p/playlist.m3u8', 'seg.ts'),
                ('https://example.com/a/b.m3u8', 'rtmp://example.com/live'),
                ('https://example.com/a/b.m3u8', 'https:relative.m3u8'),
                ('https://example.com/a/b.m3u8', 'mailto:a@b.c'),
                ('https://example.com/a/b.m3u8', 'x y.m3u8'),
                ('https://example.com/a/b.m3u8', 'x%20y.m3u8'),
                ('https://example.com/a/b.m3u8', '/a/../b.m3u8'),
                ('https://example.com', 'c.m3u8'),
                ('https://example.com', '/c.m3u8'),
                ('https://example.com/', 'c.m3u8'),
                ('https://example.com/a/b.m3u8', '..'),
                ('https://example.com/a/b.m3u8', '.'),
                ('https://example.com/a/b.m3u8', 'HTTPS://Other.com/D.M3U8'),
                ('https://example.com/a/b.m3u8', 'http://other.com/d.m3u8'),
                ('https://example.com/a/b.m3u8', '//other.com'),
                ('https://example.com:8443/a/b.m3u8', 'c.m3u8'),
                ('https://user:pass@example.com/a/b.m3u8', 'c.m3u8'),
            ]
        ],
        'stream_types': {
            url: IPTVUtil.DetectStreamType(url)
            for url in [
                'https://example.com/a.m3u8',
                'https://example.com/a.m3u8?token=1',
                'https://example.com/live?type=hls',
                'https://example.com/a.mpd',
                'https://example.com/a.mpd?type=dash',
                'https://example.com/a.mp4',
                'https://example.com/a.m4v',
                'https://example.com/a.ts',
                'https://example.com/a.m2ts',
                'https://example.com/a.mts',
                'https://example.com/a.flv',
                'https://example.com/live',
                'https://example.com/master.m3u8;session=live_stream_1341',
                'https://example.com/a.m3u8;a=1?token=2',
                'https://example.com/a.ts;session=1',
            ]
        },
        'm3u_output': IPTVUtil.BuildM3UPlaylist(channels),
        'groups': IPTVUtil.GetGroups(channels),
        'countries_list': IPTVUtil.GetCountries(channels),
    }
    output = REPO_ROOT / 'server-go' / 'internal' / 'iptv' / 'testdata' / 'iptv_parity.json'
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(fixture, ensure_ascii=False, indent=2), encoding='utf-8')
    print(f'{len(fixture["channels"])} channels written to {output}')


if __name__ == '__main__':
    main()
