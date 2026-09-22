"""
インターネット上で公開されている IPTV の M3U プレイリストを取り込み、
KonomiTV 上で視聴できるようにするためのユーティリティ群。

- config.yaml の iptv セクションに設定された M3U プレイリストの URL (またはローカルファイル) を取得し、解析する
- 解析結果はメモリ上にキャッシュされ、cache_ttl 秒間は再利用される
- チャンネルは国別 (ISO 3166-1 alpha-2) ・グループ別に絞り込めるようにする
- 各チャンネルのストリームはブラウザから直接アクセスできないことが多い (CORS・リファラ・User-Agent 制限) ため、
  IPTVRouter のプロキシ API 経由で配信する。ここではそのプロキシに必要なヘルパーも提供する
"""

import asyncio
import hashlib
import json
import re
import time
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any, Literal
from urllib.parse import quote, urljoin, urlparse

import httpx

from app import logging
from app.config import Config
from app.constants import DATA_DIR, LIBRARY_PATH


# IPTV ストリームをプロキシする API のパス (IPTVRouter と一致させる必要がある)
IPTV_PROXY_PATH = '/api/iptv/proxy'

# 追加登録した M3U プレイリストの URL を保存するファイル
IPTV_USER_SOURCES_PATH = DATA_DIR / 'iptv_sources.json'

# テレビ視聴 UI (TV ホーム / 視聴画面) に登録した IPTV チャンネルの display_channel_id を保存するファイル
IPTV_TVUI_CHANNELS_PATH = DATA_DIR / 'iptv_tvui_channels.json'

# IPTV チャンネルの display_channel_id のプレフィックス
## クライアント側の ChannelUtils.getChannelType() は「英字 + 数字」の ID を前提としているため、
## プレフィックスの後ろは必ず数字 (10 進数) にする
IPTV_DISPLAY_CHANNEL_ID_PREFIX = 'iptv'

# IPTV の疑似チャンネルの id のプレフィックス (通常のチャンネルの id と衝突しないようにする)
IPTV_CHANNEL_ID_PREFIX = 'IPTV-'

# テレビ視聴 UI に登録できる IPTV チャンネルの最大数
IPTV_TVUI_MAX_CHANNELS = 200

# 国コード (ISO 3166-1 alpha-2) から国名・国旗を取得するための iptv-org の API
IPTV_COUNTRIES_API_URL = 'https://iptv-org.github.io/api/countries.json'

# EXTINF 行の属性 (key="value") を抽出するための正規表現
_EXTINF_ATTRIBUTE_PATTERN = re.compile(r'([\w-]+)="([^"]*)"')

# プレイリスト内の URI="..." (EXT-X-KEY / EXT-X-MAP など) を抽出するための正規表現
_PLAYLIST_URI_PATTERN = re.compile(r'URI="([^"]+)"')

# tvg-id (例: "NHKWorldJapan.jp@SD") から国コードを抽出するための正規表現
_TVG_ID_COUNTRY_PATTERN = re.compile(r'\.([A-Za-z]{2})(?:@|$)')

# プレイリストの URL (例: ".../countries/jp.m3u") から国コードを抽出するための正規表現
_SOURCE_URL_COUNTRY_PATTERN = re.compile(r'/countries/([A-Za-z]{2})\.m3u', re.IGNORECASE)


@dataclass
class IPTVChannel:
    """IPTV の1チャンネル分の情報を保持するデータクラス。"""

    # チャンネルを一意に識別する ID (URL のハッシュから生成する)
    id: str
    # チャンネル名 (EXTINF 行のカンマ以降、または tvg-name)
    name: str
    # ストリームの URL (m3u8 / ts / mp4 など)
    url: str
    # チャンネルロゴの URL
    logo_url: str | None
    # グループ (ジャンルやカテゴリ)
    group: str | None
    # 国コード (ISO 3166-1 alpha-2, 大文字)
    country: str | None
    # 国名
    country_name: str | None
    # 国旗の絵文字
    country_flag: str | None
    # 元のプレイリストで振られている tvg-id
    tvg_id: str | None
    # 言語コード
    language: str | None
    # ストリーム取得時に送信する User-Agent (プレイリストで指定されている場合)
    user_agent: str | None
    # ストリーム取得時に送信する Referer (プレイリストで指定されている場合)
    referrer: str | None
    # ジオブロック (地域制限) されているとプレイリストが明記しているか
    is_geo_blocked: bool
    # このチャンネルを提供しているプレイリストの URL
    source_url: str


# 解析済みのチャンネル一覧のキャッシュ
_channels: list[IPTVChannel] = []

# 国コード → 国情報 (国名・国旗) の対応表
_countries_by_code: dict[str, dict[str, str]] = {}

# display_channel_id → IPTVChannel の対応表 (RefreshChannels() で再構築する)
_channels_by_display_id: dict[str, IPTVChannel] = {}

# ストリーム URL → 追加ヘッダーの対応表
_stream_headers_by_url: dict[str, dict[str, str]] = {}

# ホスト名 → 追加ヘッダーの対応表 (セグメントなど、URL が完全一致しない場合のフォールバック)
_stream_headers_by_host: dict[str, dict[str, str]] = {}

# プレイリストの取得に失敗したソースとそのエラーメッセージ
_source_errors: dict[str, str] = {}

# 最後にチャンネル一覧を更新した時刻
_updated_at: float = 0.0

# 更新処理を排他制御するためのロック
_refresh_lock = asyncio.Lock()


def _BuildChannelID(url: str) -> str:
    """ストリームの URL から、チャンネルを一意に識別する安定した ID を生成する。"""

    return hashlib.sha1(url.encode('utf-8')).hexdigest()[:16]


def _NormalizeURL(base_url: str, url: str) -> str:
    """プレイリスト内の相対 URL を、プレイリストの URL を基準に絶対 URL に変換する。"""

    return urljoin(base_url, url.strip())


def _DeriveCountryCode(attributes: dict[str, str], source_url: str) -> str | None:
    """
    EXTINF の属性とプレイリストの URL から、チャンネルの国コードを推定する。

    推定の優先順位は tvg-country > tvg-id > プレイリストの URL の順。

    Args:
        attributes (dict[str, str]): EXTINF 行の属性
        source_url (str): プレイリストの URL

    Returns:
        str | None: 国コード (ISO 3166-1 alpha-2, 大文字) 。推定できなかった場合は None
    """

    # tvg-country が指定されている場合 (複数指定されている場合は先頭を使う)
    tvg_country = attributes.get('tvg-country')
    if tvg_country is not None:
        code = re.split(r'[;,\s]+', tvg_country.strip())[0]
        if len(code) == 2:
            return code.upper()

    # tvg-id (例: "NHKWorldJapan.jp@SD") から抽出する
    tvg_id = attributes.get('tvg-id')
    if tvg_id is not None:
        match = _TVG_ID_COUNTRY_PATTERN.search(tvg_id)
        if match is not None:
            return match.group(1).upper()

    # プレイリストの URL (例: ".../countries/jp.m3u") から抽出する
    match = _SOURCE_URL_COUNTRY_PATTERN.search(source_url)
    if match is not None:
        return match.group(1).upper()

    return None


def ParseM3UPlaylist(content: str, source_url: str) -> list[IPTVChannel]:
    """
    M3U / M3U8 プレイリストの文字列を解析し、チャンネル一覧を生成する。

    拡張 M3U (#EXTINF) の属性 (tvg-id / tvg-logo / tvg-name / group-title / tvg-country /
    http-user-agent / http-referrer など) を読み取り、次の行の URL と組み合わせて1チャンネル分の情報を組み立てる。
    同じストリーム URL が複数のソースに存在する場合は、最初に見つかったものを優先して重複を除外する。

    Args:
        content (str): プレイリストの内容
        source_url (str): プレイリストの取得元 URL (相対 URL の解決とエラー表示に利用する)

    Returns:
        list[IPTVChannel]: 解析したチャンネル一覧
    """

    channels: list[IPTVChannel] = []
    seen_urls: set[str] = set()

    # 現在処理中の #EXTINF 行の属性
    current_attributes: dict[str, str] = {}
    current_name: str | None = None

    for raw_line in content.splitlines():
        line = raw_line.strip()

        # 空行は無視
        if line == '':
            continue

        # コメント行 (タグ) の場合
        if line.startswith('#'):
            # #EXTINF 行なら、属性とチャンネル名を保持しておく
            if line.startswith('#EXTINF:'):
                current_attributes = dict(_EXTINF_ATTRIBUTE_PATTERN.findall(line))
                # カンマ以降がチャンネル名だが、属性値 (http-user-agent など) にカンマが含まれることがあるため、
                ## まず属性 (key="value") をすべて取り除いてからカンマで分割する
                line_without_attributes = _EXTINF_ATTRIBUTE_PATTERN.sub('', line)
                if ',' in line_without_attributes:
                    current_name = line_without_attributes.split(',', 1)[1].strip()
                else:
                    current_name = current_attributes.get('tvg-name') or None
            # #EXTGRP はグループ指定の別形式
            elif line.startswith('#EXTGRP:') and current_attributes.get('group-title') is None:
                current_attributes['group-title'] = line.split(':', 1)[1].strip()
            # #EXTVLCOPT:http-user-agent=... / http-referrer=... 形式にも対応する
            elif line.startswith('#EXTVLCOPT:'):
                option = line.split(':', 1)[1].strip()
                if '=' in option:
                    key, value = option.split('=', 1)
                    key = key.strip().lower()
                    if key == 'http-user-agent':
                        current_attributes['http-user-agent'] = value.strip()
                    elif key == 'http-referrer':
                        current_attributes['http-referrer'] = value.strip()
            continue

        # ここに到達する行は URL
        url = _NormalizeURL(source_url, line)

        # #EXTINF が無いまま URL が来た場合は、前のチャンネル名などを引き継がないようリセットする
        name = current_name or current_attributes.get('tvg-name') or urlparse(url).netloc or url
        attributes = current_attributes
        current_attributes = {}
        current_name = None

        # 同一 URL の重複を除外
        if url in seen_urls:
            continue
        seen_urls.add(url)

        # ジオブロック (地域制限) の表記を検出する
        is_geo_blocked = '[Geo-blocked]' in name

        # 国コードを推定する
        country = _DeriveCountryCode(attributes, source_url)
        country_info = _countries_by_code.get(country, {}) if country is not None else {}

        channels.append(IPTVChannel(
            id = _BuildChannelID(url),
            name = name,
            url = url,
            logo_url = attributes.get('tvg-logo') or None,
            group = attributes.get('group-title') or None,
            country = country,
            country_name = country_info.get('name'),
            country_flag = country_info.get('flag'),
            tvg_id = attributes.get('tvg-id') or None,
            language = attributes.get('tvg-language') or None,
            user_agent = attributes.get('http-user-agent') or None,
            referrer = attributes.get('http-referrer') or None,
            is_geo_blocked = is_geo_blocked,
            source_url = source_url,
        ))

    return channels


def LoadUserSources() -> list[str]:
    """追加登録された M3U プレイリストの URL の一覧を読み込む。ファイルが無い場合は空のリストを返す。"""

    if IPTV_USER_SOURCES_PATH.exists() is False:
        return []
    try:
        data = json.loads(IPTV_USER_SOURCES_PATH.read_text(encoding='utf-8'))
        if isinstance(data, list):
            return [str(item) for item in data]
    except (json.JSONDecodeError, OSError) as ex:
        logging.warning(f'Failed to load IPTV user sources: {ex}')
    return []


def SaveUserSources(sources: list[str]) -> None:
    """追加登録された M3U プレイリストの URL の一覧を保存する。"""

    IPTV_USER_SOURCES_PATH.parent.mkdir(parents=True, exist_ok=True)
    IPTV_USER_SOURCES_PATH.write_text(
        json.dumps(sources, ensure_ascii=False, indent=2),
        encoding = 'utf-8',
    )


def GetAllSourceURLs() -> list[str]:
    """config.yaml に設定されたソースと、追加登録されたソースをまとめて返す (重複は除外する)。"""

    sources: list[str] = []
    for source in list(Config().iptv.sources) + LoadUserSources():
        source = source.strip()
        if source != '' and source not in sources:
            sources.append(source)
    return sources


async def _FetchPlaylist(client: httpx.AsyncClient, source: str) -> tuple[str, str]:
    """
    指定されたソースからプレイリストの内容を取得する。

    http(s):// で始まる場合はネットワークから、それ以外はローカルファイルとして読み込む。

    Args:
        client (httpx.AsyncClient): 使い回す HTTP クライアント
        source (str): プレイリストの URL またはローカルファイルのパス

    Returns:
        tuple[str, str]: (プレイリストのURL, プレイリストの内容)

    Raises:
        Exception: プレイリストの取得に失敗した場合
    """

    if source.startswith('http://') or source.startswith('https://'):
        response = await client.get(source)
        if response.status_code != 200:
            raise Exception(f'HTTP Error {response.status_code}')
        return source, response.text

    file_path = Path(source)
    if file_path.exists() is False:
        raise Exception('File Not Found')
    return source, file_path.read_text(encoding='utf-8', errors='replace')


async def _FetchCountries(client: httpx.AsyncClient) -> dict[str, dict[str, str]]:
    """
    iptv-org の API から国コード → 国名・国旗の対応表を取得する。

    取得に失敗した場合でもチャンネル一覧の更新自体は継続させたいため、例外は送出せず空の辞書を返す。

    Args:
        client (httpx.AsyncClient): 使い回す HTTP クライアント

    Returns:
        dict[str, dict[str, str]]: 国コード (大文字) → {'name': 国名, 'flag': 国旗}
    """

    try:
        # iptv-org の国一覧 API を取得する
        response = await client.get(IPTV_COUNTRIES_API_URL)
        if response.status_code != 200:
            logging.warning(f'Failed to fetch IPTV countries. (HTTP Error {response.status_code})')
            return {}
        countries = response.json()
        if not isinstance(countries, list):
            return {}

        # 国コード → 国名・国旗 の辞書を組み立てる
        result: dict[str, dict[str, str]] = {}
        for country in countries:
            if isinstance(country, dict) and isinstance(country.get('code'), str):
                result[country['code'].upper()] = {
                    'name': str(country.get('name') or country['code']),
                    'flag': str(country.get('flag') or ''),
                }
        return result
    except (httpx.NetworkError, httpx.TimeoutException, ValueError) as ex:
        logging.warning(f'Failed to fetch IPTV countries: {ex}')
        return {}


async def RefreshChannels(force: bool = False) -> list[IPTVChannel]:
    """
    config.yaml に設定された全ての M3U プレイリストを取得し、チャンネル一覧のキャッシュを更新する。

    キャッシュが有効な間 (cache_ttl 秒以内) は、force が True でない限り再取得しない。
    複数のソースは並行して取得するが、1つのソースの失敗が他のソースに影響しないようにする。

    Args:
        force (bool): キャッシュを無視して強制的に再取得するかどうか

    Returns:
        list[IPTVChannel]: チャンネル一覧
    """

    global _channels, _countries_by_code, _stream_headers_by_url, _stream_headers_by_host, _source_errors, _updated_at

    # IPTV 機能が無効の場合は何もしない
    if Config().iptv.enabled is False:
        return _channels

    # キャッシュが有効な場合はキャッシュを返す
    if force is False and _updated_at > 0 and (time.time() - _updated_at) < Config().iptv.cache_ttl:
        return _channels

    async with _refresh_lock:
        # ロック取得待ちの間に他のリクエストが更新を完了している可能性があるため、再度キャッシュを確認する
        if force is False and _updated_at > 0 and (time.time() - _updated_at) < Config().iptv.cache_ttl:
            return _channels

        sources = GetAllSourceURLs()
        timestamp = time.time()
        logging.info(f'IPTV playlists updating... ({len(sources)} source(s))')

        channels: list[IPTVChannel] = []
        headers_by_url: dict[str, dict[str, str]] = {}
        headers_by_host: dict[str, dict[str, str]] = {}
        errors: dict[str, str] = {}
        countries_by_code: dict[str, dict[str, str]] = {}

        default_headers = {
            'User-Agent': Config().iptv.user_agent,
        }

        async with httpx.AsyncClient(
            headers = default_headers,
            follow_redirects = True,
            timeout = Config().iptv.request_timeout,
        ) as client:
            # 国一覧とプレイリストを並行して取得する
            countries_task = asyncio.create_task(_FetchCountries(client))
            results = await asyncio.gather(
                *[_FetchPlaylist(client, source) for source in sources],
                return_exceptions = True,
            )
            countries_by_code = await countries_task

        # 国コード → 国名・国旗 の対応表をキャッシュへ反映する
        # 国コードが特定できているチャンネルに国名・国旗を付与する必要があるため、
        # ParseM3UPlaylist() の前にグローバルへ設定する
        _countries_by_code = countries_by_code

        seen_urls: set[str] = set()
        for source, result in zip(sources, results, strict=True):
            # 取得に失敗した場合
            if isinstance(result, BaseException):
                error_message = str(result) or type(result).__name__
                errors[source] = error_message
                logging.warning(f'Failed to fetch IPTV playlist: {source} ({error_message})')
                continue

            playlist_source, content = result
            for channel in ParseM3UPlaylist(content, playlist_source):
                # ソースをまたいだ重複を除外する
                if channel.url in seen_urls:
                    continue
                seen_urls.add(channel.url)
                channels.append(channel)

                # ストリーム取得用のヘッダーを記録する
                stream_headers: dict[str, str] = {}
                if channel.user_agent is not None:
                    stream_headers['User-Agent'] = channel.user_agent
                if channel.referrer is not None:
                    stream_headers['Referer'] = channel.referrer
                if stream_headers:
                    headers_by_url[channel.url] = stream_headers
                    host = urlparse(channel.url).netloc
                    if host not in headers_by_host:
                        headers_by_host[host] = stream_headers

        # 国名・グループ名・チャンネル名の順で並べ替える
        channels.sort(key = lambda channel: (
            (channel.country_name or '\uffff').lower(),
            (channel.group or '\uffff').lower(),
            channel.name.lower(),
        ))

        _channels = channels
        _stream_headers_by_url = headers_by_url
        _stream_headers_by_host = headers_by_host
        _source_errors = errors
        _updated_at = time.time()
        # display_channel_id との対応表を再構築する
        _BuildDisplayChannelIDMap()

        logging.info(
            f'IPTV playlists update complete. ({len(channels)} channels, '
            f'{len(countries_by_code)} countries, {len(errors)} error(s), {round(time.time() - timestamp, 3)} sec)'
        )

    return _channels


def GetChannels() -> list[IPTVChannel]:
    """現在キャッシュされているチャンネル一覧を返す。"""

    return _channels


def GetUpdatedAt() -> float:
    """チャンネル一覧を最後に更新した時刻 (UNIX 時間) を返す。"""

    return _updated_at


def GetSourceErrors() -> dict[str, str]:
    """取得に失敗したプレイリストの URL とエラーメッセージの対応表を返す。"""

    return _source_errors


def FilterChannels(
    country: str | None = None,
    group: str | None = None,
    search: str | None = None,
) -> list[IPTVChannel]:
    """
    キャッシュされているチャンネル一覧を、国・グループ・キーワードで絞り込む。

    Args:
        country (str | None): 国コード (ISO 3166-1 alpha-2, 大文字小文字は問わない) 。None なら全件
        group (str | None): グループ名。None なら全件
        search (str | None): チャンネル名の部分一致キーワード。None なら全件

    Returns:
        list[IPTVChannel]: 絞り込んだチャンネル一覧
    """

    # 国コードは大文字に正規化して比較する
    country_code = country.upper() if country is not None else None
    search_keyword = search.lower() if search is not None else None

    return [
        channel for channel in _channels
        if (
            (country_code is None or channel.country == country_code) and
            (group is None or channel.group == group) and
            (search_keyword is None or search_keyword in channel.name.lower())
        )
    ]


def GetCountries(channels: list[IPTVChannel] | None = None) -> list[dict]:
    """
    チャンネル一覧に含まれる国を、チャンネル数付きで返す (チャンネル数の多い順) 。

    Args:
        channels (list[IPTVChannel] | None): 集計対象のチャンネル一覧 (None の場合はキャッシュ全体)

    Returns:
        list[dict]: [{'code': 国コード, 'name': 国名, 'flag': 国旗, 'count': チャンネル数}]
    """

    target = _channels if channels is None else channels
    counts: dict[str, int] = {}
    for channel in target:
        if channel.country is None:
            continue
        counts[channel.country] = counts.get(channel.country, 0) + 1

    countries: list[dict] = []
    for code, count in counts.items():
        info = _countries_by_code.get(code, {})
        countries.append({
            'code': code,
            'name': info.get('name', code),
            'flag': info.get('flag', ''),
            'count': count,
        })
    # チャンネル数の多い順 → 国名の順で並べる
    countries.sort(key = lambda country: (-country['count'], country['name'].lower()))
    return countries


def GetGroups(channels: list[IPTVChannel] | None = None) -> list[dict]:
    """
    チャンネル一覧に含まれるグループを、チャンネル数付きで返す (チャンネル数の多い順) 。

    Args:
        channels (list[IPTVChannel] | None): 集計対象のチャンネル一覧 (None の場合はキャッシュ全体)

    Returns:
        list[dict]: [{'name': グループ名, 'count': チャンネル数}]
    """

    target = _channels if channels is None else channels
    counts: dict[str, int] = {}
    for channel in target:
        if channel.group is None:
            continue
        counts[channel.group] = counts.get(channel.group, 0) + 1

    groups = [{'name': name, 'count': count} for name, count in counts.items()]
    groups.sort(key = lambda group: (-group['count'], group['name'].lower()))
    return groups


def GetStreamHeaders(url: str) -> dict[str, str]:
    """
    指定されたストリーム URL を取得する際に送信すべき追加ヘッダーを返す。

    URL が完全一致しない場合 (HLS のセグメントなど) は、ホスト名が一致するチャンネルのヘッダーをフォールバックとして返す。

    Args:
        url (str): ストリームの URL

    Returns:
        dict[str, str]: 追加で送信するリクエストヘッダー
    """

    if url in _stream_headers_by_url:
        return dict(_stream_headers_by_url[url])
    host = urlparse(url).netloc
    if host in _stream_headers_by_host:
        return dict(_stream_headers_by_host[host])
    return {}


def BuildProxyURL(url: str) -> str:
    """ストリームの URL を、KonomiTV サーバーのプロキシ API の URL に変換する。"""

    return f'{IPTV_PROXY_PATH}?url={quote(url, safe="")}'


def RewriteHLSPlaylist(content: str, base_url: str) -> str:
    """
    HLS プレイリスト内の URI を、すべて KonomiTV サーバーのプロキシ API 経由に書き換える。

    - セグメントや子プレイリストの URI (タグではない行) を書き換える
    - #EXT-X-KEY / #EXT-X-MAP などのタグ内の URI="..." を書き換える

    Args:
        content (str): 書き換え前のプレイリストの内容
        base_url (str): プレイリスト自体の URL (相対 URL の解決に利用する)

    Returns:
        str: 書き換え後のプレイリストの内容
    """

    rewritten_lines: list[str] = []

    for line in content.splitlines():
        stripped = line.strip()

        if stripped == '':
            rewritten_lines.append('')
            continue

        # タグ行の場合
        if stripped.startswith('#'):
            # タグ内に URI="..." がある場合は書き換える
            if 'URI="' in line:
                line = _PLAYLIST_URI_PATTERN.sub(
                    lambda match: f'URI="{BuildProxyURL(_NormalizeURL(base_url, match.group(1)))}"',
                    line,
                )
            rewritten_lines.append(line)
            continue

        # それ以外の行は URI なのでプロキシ経由に書き換える
        rewritten_lines.append(BuildProxyURL(_NormalizeURL(base_url, stripped)))

    return '\n'.join(rewritten_lines) + '\n'


def DetectStreamType(url: str) -> Literal['HLS', 'DASH', 'MP4', 'MPEGTS', 'Other']:
    """
    ストリームの URL から、配信フォーマットを推定する。

    Args:
        url (str): ストリームの URL

    Returns:
        Literal['HLS', 'DASH', 'MP4', 'MPEGTS', 'Other']: 推定した配信フォーマット
    """

    parsed = urlparse(url)
    path = parsed.path.lower()
    query = parsed.query.lower()

    if path.endswith('.m3u8') or 'm3u8' in query or 'hls' in query:
        return 'HLS'
    if path.endswith('.mpd') or 'dash' in query:
        return 'DASH'
    if path.endswith('.mp4') or path.endswith('.m4v'):
        return 'MP4'
    if path.endswith('.ts') or path.endswith('.m2ts') or path.endswith('.mts'):
        return 'MPEGTS'
    return 'Other'


def BuildM3UPlaylist(channels: list[IPTVChannel] | None = None) -> str:
    """
    チャンネル一覧を、外部プレイヤーでも利用できる M3U プレイリストとして出力する。

    Args:
        channels (list[IPTVChannel] | None): 出力するチャンネル一覧 (None の場合はキャッシュ全体)

    Returns:
        str: M3U プレイリストの内容
    """

    target = _channels if channels is None else channels
    lines = ['#EXTM3U']
    for channel in target:
        attributes = ''
        if channel.tvg_id is not None:
            attributes += f' tvg-id="{channel.tvg_id}"'
        if channel.logo_url is not None:
            attributes += f' tvg-logo="{channel.logo_url}"'
        if channel.group is not None:
            attributes += f' group-title="{channel.group}"'
        if channel.country is not None:
            attributes += f' tvg-country="{channel.country}"'
        lines.append(f'#EXTINF:-1{attributes},{channel.name}')
        # 外部プレイヤーが KonomiTV サーバー経由でアクセスできるよう、プロキシの URL を出力する
        lines.append(BuildProxyURL(channel.url))
    return '\n'.join(lines) + '\n'


def ChannelToDict(channel: IPTVChannel) -> dict:
    """チャンネル情報を、API レスポンス用の辞書に変換する。"""

    data = asdict(channel)
    # ストリームの URL はそのままクライアントに公開せず、プロキシ経由の URL に置き換える
    data['stream_url'] = BuildProxyURL(channel.url)
    data['stream_type'] = DetectStreamType(channel.url)
    data['is_hls'] = data['stream_type'] == 'HLS'
    # テレビ視聴 UI で再生するための疑似チャンネル ID
    data['display_channel_id'] = BuildDisplayChannelID(channel.url)
    # 内部利用のみのフィールドは削除する
    data.pop('url', None)
    data.pop('user_agent', None)
    data.pop('referrer', None)
    return data


# ***** テレビ視聴 UI との連携 *****


def _BuildDisplayChannelIDMap() -> None:
    """display_channel_id → IPTVChannel の対応表を再構築する (RefreshChannels() から呼ばれる) 。"""

    global _channels_by_display_id
    _channels_by_display_id = {BuildDisplayChannelID(channel.url): channel for channel in _channels}


def BuildDisplayChannelID(url: str) -> str:
    """
    ストリームの URL から、IPTV チャンネルの display_channel_id を生成する。

    クライアント側の ChannelUtils.getChannelType() は「英字 + 数字」の ID を前提としているため、
    プレフィックス (iptv) の後ろは 10 進数の数字のみにする。

    Args:
        url (str): ストリームの URL

    Returns:
        str: display_channel_id (例: iptv123456789012345678)
    """

    return IPTV_DISPLAY_CHANNEL_ID_PREFIX + str(int(hashlib.sha1(url.encode('utf-8')).hexdigest()[:15], 16))


def IsIPTVDisplayChannelID(display_channel_id: str) -> bool:
    """
    指定された display_channel_id が IPTV の疑似チャンネルのものかを判定する。

    Args:
        display_channel_id (str): 判定する display_channel_id

    Returns:
        bool: IPTV の疑似チャンネルなら True
    """

    if display_channel_id.startswith(IPTV_DISPLAY_CHANNEL_ID_PREFIX) is False:
        return False
    return display_channel_id[len(IPTV_DISPLAY_CHANNEL_ID_PREFIX):].isdigit()


def GetChannelByDisplayChannelID(display_channel_id: str) -> IPTVChannel | None:
    """
    display_channel_id に一致する IPTV チャンネルを取得する。

    Args:
        display_channel_id (str): IPTV チャンネルの display_channel_id

    Returns:
        IPTVChannel | None: 一致する IPTV チャンネル (見つからなかった場合は None)
    """

    # まだ対応表が構築されていない場合はここで構築する
    if _channels_by_display_id == {} and _channels:
        _BuildDisplayChannelIDMap()
    return _channels_by_display_id.get(display_channel_id)


def _LoadTVUIRegistry() -> dict[str, list[str]]:
    """
    テレビ視聴 UI への IPTV チャンネル登録を、ユーザーキーごとに読み込む。

    保存形式は {ユーザーキー: [display_channel_id, ...]} の辞書。
    旧形式 (グローバルな配列) のファイルが存在する場合は、ユーザーごとに分離できないため無視する。

    Returns:
        dict[str, list[str]]: ユーザーキー → 登録済みの display_channel_id の一覧
    """

    if IPTV_TVUI_CHANNELS_PATH.exists() is False:
        return {}
    try:
        data = json.loads(IPTV_TVUI_CHANNELS_PATH.read_text(encoding='utf-8'))
        if isinstance(data, dict):
            registry: dict[str, list[str]] = {}
            for user_key, display_channel_ids in data.items():
                if isinstance(display_channel_ids, list):
                    registry[str(user_key)] = [str(item) for item in display_channel_ids]
            return registry
    except (json.JSONDecodeError, OSError) as ex:
        logging.warning(f'Failed to load IPTV TV UI channels: {ex}')
    return {}


def _SaveTVUIRegistry(registry: dict[str, list[str]]) -> None:
    """テレビ視聴 UI への IPTV チャンネル登録を、ユーザーキーごとに保存する。"""

    IPTV_TVUI_CHANNELS_PATH.parent.mkdir(parents=True, exist_ok=True)
    IPTV_TVUI_CHANNELS_PATH.write_text(
        json.dumps(registry, ensure_ascii=False, indent=2),
        encoding = 'utf-8',
    )


def LoadTVUIChannelIDs(user_key: str) -> list[str]:
    """
    指定されたユーザーがテレビ視聴 UI に登録した IPTV チャンネルの display_channel_id の一覧を読み込む。

    Args:
        user_key (str): ユーザーを識別するキー (例: 'user:1')

    Returns:
        list[str]: 登録済みの display_channel_id の一覧
    """

    return _LoadTVUIRegistry().get(user_key, [])


def SaveTVUIChannelIDs(user_key: str, display_channel_ids: list[str]) -> None:
    """
    指定されたユーザーの IPTV チャンネルの登録内容を保存する。

    Args:
        user_key (str): ユーザーを識別するキー (例: 'user:1')
        display_channel_ids (list[str]): 保存する display_channel_id の一覧
    """

    registry = _LoadTVUIRegistry()
    registry[user_key] = display_channel_ids
    _SaveTVUIRegistry(registry)


def RegisterTVUIChannel(user_key: str, display_channel_id: str) -> list[str]:
    """
    指定されたユーザーのテレビ視聴 UI に IPTV チャンネルを登録する (既に登録済みの場合は末尾に移動する) 。

    登録数が上限を超える場合は、古いものから削除する。

    Args:
        user_key (str): ユーザーを識別するキー (例: 'user:1')
        display_channel_id (str): 登録する IPTV チャンネルの display_channel_id

    Returns:
        list[str]: 登録後の display_channel_id の一覧
    """

    display_channel_ids = LoadTVUIChannelIDs(user_key)
    # 既に登録済みの場合は一旦削除して末尾 (最新) に移動する
    if display_channel_id in display_channel_ids:
        display_channel_ids.remove(display_channel_id)
    display_channel_ids.append(display_channel_id)
    # 上限を超えた分は古いものから削除する
    if len(display_channel_ids) > IPTV_TVUI_MAX_CHANNELS:
        display_channel_ids = display_channel_ids[-IPTV_TVUI_MAX_CHANNELS:]
    SaveTVUIChannelIDs(user_key, display_channel_ids)
    return display_channel_ids


def UnregisterTVUIChannel(user_key: str, display_channel_id: str) -> list[str]:
    """
    指定されたユーザーのテレビ視聴 UI から IPTV チャンネルを削除する。

    Args:
        user_key (str): ユーザーを識別するキー (例: 'user:1')
        display_channel_id (str): 削除する IPTV チャンネルの display_channel_id

    Returns:
        list[str]: 削除後の display_channel_id の一覧
    """

    display_channel_ids = LoadTVUIChannelIDs(user_key)
    if display_channel_id in display_channel_ids:
        display_channel_ids.remove(display_channel_id)
        SaveTVUIChannelIDs(user_key, display_channel_ids)
    return display_channel_ids


def GetTVUIChannels(user_key: str) -> list[IPTVChannel]:
    """
    指定されたユーザーがテレビ視聴 UI に登録した IPTV チャンネルの一覧を、登録順で返す。

    Args:
        user_key (str): ユーザーを識別するキー (例: 'user:1')

    Returns:
        list[IPTVChannel]: 登録済みの IPTV チャンネルの一覧
    """

    channels: list[IPTVChannel] = []
    for display_channel_id in LoadTVUIChannelIDs(user_key):
        channel = GetChannelByDisplayChannelID(display_channel_id)
        if channel is not None:
            channels.append(channel)
    return channels


def BuildUserKey(user_id: int | None) -> str:
    """
    テレビ視聴 UI の IPTV 登録をユーザーごとに分離するためのキーを生成する。

    ログインしていない場合は 'anonymous' を返すが、呼び出し側でログイン必須とするかどうかを判断する。

    Args:
        user_id (int | None): ログイン中のユーザー ID (ログインしていない場合は None)

    Returns:
        str: ユーザーを識別するキー
    """

    return f'user:{user_id}' if user_id is not None else 'anonymous'


# ***** ライブエンコード処理 (LiveEncodingTask) との連携 *****


def BuildTSConversionArguments(channel: IPTVChannel) -> list[str]:
    """
    IPTV のストリームを MPEG-2 TS に変換する FFmpeg の引数を組み立てる。

    KonomiTV の既存のライブエンコード処理 (LiveEncodingTask) は放送波の MPEG-2 TS を
    入力として受け取るため、IPTV のストリーム (主に HLS) を MPEG-2 TS に変換して渡す。
    ここではコンテナの変換のみを行い、映像・音声は再エンコードしない
    (画質ごとの H.264 への変換は、後段の LiveEncodingTask のエンコーダーが行う) 。

    Args:
        channel (IPTVChannel): 変換する IPTV チャンネル

    Returns:
        list[str]: FFmpeg の引数
    """

    args = [
        LIBRARY_PATH['FFmpeg'],
        '-hide_banner',
        '-loglevel', 'error',
        # 配信サーバーによっては User-Agent / Referer を要求するため、プレイリストの指定を反映する
        '-user_agent', Config().iptv.user_agent,
    ]
    if channel.referrer is not None:
        args += ['-referer', channel.referrer]
    args += [
        # ライブストリームではタイムスタンプが欠落することがあるため、必要に応じて生成させる
        '-fflags', '+genpts',
        '-i', channel.url,
        # 映像・音声は再エンコードせず、MPEG-2 TS に詰め替えるだけにする
        '-c', 'copy',
        '-f', 'mpegts',
        'pipe:1',
    ]
    return args


def BuildEncodingChannel(channel: IPTVChannel) -> Any:
    """
    IPTV チャンネルを、既存のライブエンコード処理 (LiveEncodingTask) が扱える
    Channel モデル相当のオブジェクト (未保存) に変換する。

    LiveEncodingTask は放送波のチャンネル情報 (Channel モデル) を前提として実装されているため、
    IPTV チャンネルをそれと同じ形に変換して既存の処理にそのまま載せる。

    Args:
        channel (IPTVChannel): 変換する IPTV チャンネル

    Returns:
        Any: Channel モデルのインスタンス (DB には保存しない)
    """

    # 循環インポートを避けるため、関数内でインポートする
    from app.models.Channel import Channel

    display_channel_id = BuildDisplayChannelID(channel.url)
    encoding_channel: Any = Channel()
    encoding_channel.id = f'{IPTV_CHANNEL_ID_PREFIX}{display_channel_id}'
    encoding_channel.display_channel_id = display_channel_id
    # 放送波ではないため、チューナーに関連する値は 0 / None にする
    encoding_channel.network_id = 0
    encoding_channel.service_id = 0
    encoding_channel.transport_stream_id = None
    encoding_channel.remocon_id = 0
    encoding_channel.channel_number = channel.country or 'IPTV'
    # エンコードオプションはチャンネルタイプで分岐するため、地デジ相当として扱う
    encoding_channel.type = 'GR'
    encoding_channel.name = channel.name
    encoding_channel.is_subchannel = False
    encoding_channel.is_radiochannel = False
    encoding_channel.is_watchable = True
    return encoding_channel


def ChannelToLiveChannelDict(channel: IPTVChannel) -> dict:
    """
    IPTV チャンネルを、/api/channels のレスポンス (LiveChannel) 用の辞書に変換する。

    TV ホーム画面のチャンネルカードと、TV 視聴画面のチャンネル情報として利用される。
    番組情報 (EPG) は存在しないため、program_present / program_following は None にする。

    Args:
        channel (IPTVChannel): 変換する IPTV チャンネル

    Returns:
        dict: LiveChannel 相当の辞書
    """

    display_channel_id = BuildDisplayChannelID(channel.url)
    name = channel.name
    if channel.country_name is not None:
        name = f'{channel.name} ({channel.country_name})'

    return {
        'id': f'{IPTV_CHANNEL_ID_PREFIX}{display_channel_id}',
        'display_channel_id': display_channel_id,
        'network_id': 0,
        'service_id': 0,
        'transport_stream_id': None,
        'remocon_id': 0,
        # チャンネル番号には国コードを入れて、一覧で見分けやすくする
        'channel_number': (channel.country or 'IPTV'),
        'type': 'IPTV',
        'name': name,
        'terrestrial_regions': None,
        'jikkyo_force': None,
        'is_subchannel': False,
        'is_radiochannel': False,
        'is_watchable': True,
        'is_display': True,
        'viewer_count': 0,
        'program_present': None,
        'program_following': None,
    }
