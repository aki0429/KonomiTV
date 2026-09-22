"""
インターネット上の IPTV (M3U プレイリスト) を KonomiTV で視聴するための API を提供するルーター。

- チャンネル一覧 (国・グループ・キーワードで絞り込み、ページネーション対応) の取得
- 国一覧・グループ一覧の取得
- M3U プレイリストのソース (URL) の管理
- IPTV ストリーム (HLS / TS / MP4) のプロキシ配信

IPTV のストリームは CORS や Referer・User-Agent による制限を受けていることが多いため、
ブラウザから直接アクセスせず、必ずこのルーターのプロキシ API を経由して配信する。
"""

from typing import Annotated
from urllib.parse import urlparse

import httpx
from fastapi import APIRouter, HTTPException, Query, Request, status
from fastapi.responses import (
    FileResponse,
    PlainTextResponse,
    Response,
    StreamingResponse,
)

from app import logging, schemas
from app.config import Config
from app.constants import LOGO_DIR
from app.routers.UsersRouter import ResolveUserKey
from app.utils import IPTVUtil


# ルーター
router = APIRouter(
    tags = ['IPTV'],
    prefix = '/api/iptv',
)

# プロキシで許可するスキーム
ALLOWED_PROXY_SCHEMES = ['http', 'https']

# プロキシのリクエストで送信する Accept 系ヘッダー
PROXY_ACCEPT_HEADERS = {
    'Accept': '*/*',
    'Accept-Language': 'ja,en-US;q=0.9,en;q=0.8',
}

# チャンネル一覧 API の1ページあたりの最大件数
MAX_PER_PAGE = 500

# チャンネル一覧 API の1ページあたりの既定件数
DEFAULT_PER_PAGE = 60


def ValidateProxyURL(url: str) -> str:
    """
    プロキシ対象の URL を検証し、正規化した URL を返す。

    Args:
        url (str): プロキシ対象の URL

    Returns:
        str: 検証済みの URL

    Raises:
        HTTPException: URL のスキームが http / https 以外の場合
    """

    # http / https 以外のスキームは許可しない (file:// などによるローカルファイル参照を防ぐ)
    if urlparse(url).scheme not in ALLOWED_PROXY_SCHEMES:
        raise HTTPException(
            status_code = status.HTTP_400_BAD_REQUEST,
            detail = 'Proxy URL must be started with http:// or https://.',
        )
    return url


def GetProxyRequestHeaders(url: str, range_header: str | None = None) -> dict[str, str]:
    """プロキシのリクエストに送信するヘッダーを組み立てる。"""

    # プレイリストで指定された User-Agent / Referer を反映する
    headers = {
        'User-Agent': Config().iptv.user_agent,
        **PROXY_ACCEPT_HEADERS,
        **IPTVUtil.GetStreamHeaders(url),
    }
    # Range ヘッダーはシークや一部のプレイヤーで必要になるため、そのまま転送する
    if range_header is not None:
        headers['Range'] = range_header
    return headers


def BuildDefaultLogoResponse() -> FileResponse:
    """
    ロゴが取得できなかった場合に返す、既定のロゴのレスポンスを生成する。

    IPTV のチャンネルロゴはリンク切れになっていることが多いため、
    404 を返す代わりに既定のロゴを返してブラウザのコンソールにエラーを出さないようにする。

    Returns:
        FileResponse: 既定のロゴのレスポンス
    """

    return FileResponse(
        LOGO_DIR / 'default.png',
        media_type = 'image/png',
        headers = {'Cache-Control': 'public, max-age=3600'},
    )


def IsHLSPlaylist(url: str) -> bool:
    """URL が HLS プレイリスト (.m3u8) を指しているかを判定する。"""

    return urlparse(url).path.lower().endswith('.m3u8')


def BuildTVUIResponse(user_key: str) -> dict:
    """
    指定されたユーザーがテレビ視聴 UI に登録した IPTV チャンネルの一覧を、API レスポンス用の辞書に変換する。

    Args:
        user_key (str): ユーザーを識別するキー (例: 'user:1')

    Returns:
        dict: {'total': 件数, 'channels': [IPTVTVUIChannel 相当の辞書, ...]}
    """

    channels: list[dict] = []
    for channel in IPTVUtil.GetTVUIChannels(user_key):
        channels.append({
            'display_channel_id': IPTVUtil.BuildDisplayChannelID(channel.url),
            'name': channel.name,
            'logo_url': channel.logo_url,
            'country_name': channel.country_name,
        })
    return {
        'total': len(channels),
        'channels': channels,
    }


async def ProxyStream(url: str, range_header: str | None = None) -> StreamingResponse:
    """
    指定された URL のストリームを取得し、レスポンスとして返す。

    HLS プレイリスト (.m3u8) の場合は、内部の URI をすべてこのプロキシ経由に書き換えて返す。
    それ以外 (セグメントなど) の場合はバイト列をそのままストリーミングする。

    Args:
        url (str): プロキシ対象の URL
        range_header (str | None): クライアントから送信された Range ヘッダー

    Returns:
        StreamingResponse: ストリームのレスポンス

    Raises:
        HTTPException: ストリームの取得に失敗した場合 (502 Bad Gateway)
    """

    is_hls = IsHLSPlaylist(url)

    # ストリームを取得するための HTTP クライアント
    ## ライブストリームは応答が長時間返らないことがあるため、read タイムアウトを長めに設定する
    client = httpx.AsyncClient(
        follow_redirects = True,
        timeout = httpx.Timeout(Config().iptv.request_timeout, read = 60.0),
    )

    try:
        request = client.build_request('GET', url, headers = GetProxyRequestHeaders(url, range_header))
        response = await client.send(request, stream = True)
    except (httpx.NetworkError, httpx.TimeoutException) as ex:
        await client.aclose()
        raise HTTPException(
            status_code = status.HTTP_502_BAD_GATEWAY,
            detail = f'Failed to fetch IPTV stream: {type(ex).__name__}',
        )

    # 上流が 4xx / 5xx を返した場合はそのままエラーとして扱う
    if response.status_code >= 400:
        status_code = response.status_code
        await response.aclose()
        await client.aclose()
        raise HTTPException(
            status_code = status.HTTP_502_BAD_GATEWAY,
            detail = f'The IPTV stream server returned an error. (HTTP Error {status_code})',
        )

    # HLS プレイリストの場合は URI を書き換えて返す
    if is_hls:
        try:
            content = await response.aread()
        finally:
            await response.aclose()
            await client.aclose()
        # レスポンスの文字コードは UTF-8 として扱う (HLS の仕様上 UTF-8 が必須)
        rewritten = IPTVUtil.RewriteHLSPlaylist(content.decode('utf-8', errors='replace'), str(response.url))
        return StreamingResponse(
            iter([rewritten.encode('utf-8')]),
            status_code = response.status_code,
            media_type = 'application/vnd.apple.mpegurl',
            headers = {
                # ライブプレイリストは常に最新を取得する必要があるためキャッシュさせない
                'Cache-Control': 'no-store, no-cache, must-revalidate',
                'Pragma': 'no-cache',
            },
        )

    # 上流のレスポンスヘッダーのうち、転送する必要があるものを引き継ぐ
    proxy_headers: dict[str, str] = {
        'Cache-Control': 'no-store',
    }
    for header_name in ['Content-Type', 'Content-Length', 'Content-Range', 'Accept-Ranges', 'Last-Modified', 'ETag']:
        header_value = response.headers.get(header_name)
        if header_value is not None:
            proxy_headers[header_name] = header_value

    async def StreamBody():
        """上流のレスポンスボディをチャンク単位で転送する非同期ジェネレーター。"""

        try:
            async for chunk in response.aiter_raw():
                yield chunk
        finally:
            # 転送が終わったら (またはクライアントが切断したら) 必ず接続を閉じる
            await response.aclose()
            await client.aclose()

    return StreamingResponse(
        StreamBody(),
        status_code = response.status_code,
        media_type = proxy_headers.get('Content-Type', 'application/octet-stream'),
        headers = proxy_headers,
    )


@router.get(
    '/channels',
    summary = 'IPTV チャンネル一覧 API',
    response_description = 'M3U プレイリストから取得した IPTV のチャンネル一覧。',
    response_model = schemas.IPTVChannels,
)
async def IPTVChannelsAPI(
    request: Request,
    response: Response,
    country: Annotated[str | None, Query(description='国コード (ISO 3166-1 alpha-2) で絞り込む。')] = None,
    group: Annotated[str | None, Query(description='グループ名で絞り込む。')] = None,
    search: Annotated[str | None, Query(description='チャンネル名の部分一致キーワードで絞り込む。')] = None,
    page: Annotated[int, Query(ge=1, description='ページ番号 (1 始まり) 。')] = 1,
    per_page: Annotated[int, Query(ge=1, le=MAX_PER_PAGE, description='1ページあたりの件数。')] = DEFAULT_PER_PAGE,
    refresh: Annotated[bool, Query(description='キャッシュを無視してプレイリストを再取得するかどうか。')] = False,
    with_quality: Annotated[bool, Query(description='各チャンネルの元配信の画質を検出して含めるかどうか。')] = False,
):
    """
    config.yaml の iptv セクションに設定された M3U プレイリストから IPTV のチャンネル一覧を取得する。<br>
    国コード・グループ・キーワードで絞り込むことができ、ページネーションにも対応している。
    """

    # IPTV 機能が無効の場合は空の一覧を返す
    if Config().iptv.enabled is False:
        return {
            'total': 0,
            'page': page,
            'per_page': per_page,
            'max_page': 1,
            'all_total': 0,
            'updated_at': None,
            'sources': [],
            'errors': {},
            'channels': [],
        }

    # チャンネル一覧を取得する (キャッシュが有効な間は再取得しない)
    await IPTVUtil.RefreshChannels(force = refresh)

    # 絞り込みを適用する
    filtered = IPTVUtil.FilterChannels(country = country, group = group, search = search)

    # ページネーションを適用する
    total = len(filtered)
    max_page = max((total + per_page - 1) // per_page, 1)
    page = min(page, max_page)
    start = (page - 1) * per_page
    page_channels = filtered[start:start + per_page]

    # ログイン中のユーザーを取得する
    ## テレビ視聴 UI への登録状態はユーザーごとに異なるため、ログインしていない場合はすべて未登録として扱う
    # 呼び出し元を識別するキーを取得する
    ## ログイン中のユーザー、または Cookie の匿名 ID ごとに登録状態が分離される
    user_key = await ResolveUserKey(request, response)
    tvui_display_channel_ids = set(IPTVUtil.LoadTVUIChannelIDs(user_key))

    # ページ内のチャンネルを辞書に変換する
    channel_dicts: list[dict] = []
    for channel in page_channels:
        channel_dict = IPTVUtil.ChannelToDict(channel)
        channel_dict['is_tvui_registered'] = channel_dict['display_channel_id'] in tvui_display_channel_ids
        channel_dicts.append(channel_dict)

    # 元配信の画質を検出して含める
    ## 画質の検出はストリームへのアクセスを伴うため、要求された場合のみ実行する (結果はサーバー側でキャッシュされる)
    if with_quality is True:
        detected_qualities = await IPTVUtil.DetectChannelsQualities(page_channels)
        for channel_dict in channel_dicts:
            qualities = detected_qualities.get(channel_dict['id'])
            if qualities is None:
                continue
            channel_dict['source_quality'] = qualities['source_quality']
            channel_dict['source_codec'] = qualities['codec']
            channel_dict['qualities'] = qualities['qualities']

    return {
        'total': total,
        'page': page,
        'per_page': per_page,
        'max_page': max_page,
        'all_total': len(IPTVUtil.GetChannels()),
        'updated_at': IPTVUtil.GetUpdatedAt() or None,
        'sources': IPTVUtil.GetAllSourceURLs(),
        'errors': IPTVUtil.GetSourceErrors(),
        'channels': channel_dicts,
    }


@router.get(
    '/countries',
    summary = 'IPTV 国一覧 API',
    response_description = '取り込んだチャンネルに含まれる国と、そのチャンネル数。',
    response_model = schemas.IPTVCountries,
)
async def IPTVCountriesAPI(
    refresh: Annotated[bool, Query(description='キャッシュを無視してプレイリストを再取得するかどうか。')] = False,
):
    """
    取り込んだ IPTV チャンネルに含まれる国を、チャンネル数付きで取得する。<br>
    チャンネル数の多い順に並んでいるため、クライアントの国選択 UI にそのまま利用できる。
    """

    # IPTV 機能が無効の場合は空の一覧を返す
    if Config().iptv.enabled is False:
        return {'total': 0, 'updated_at': None, 'all_total': 0, 'countries': []}

    # チャンネル一覧を取得してから国を集計する
    await IPTVUtil.RefreshChannels(force = refresh)
    countries = IPTVUtil.GetCountries()

    return {
        'total': len(countries),
        'updated_at': IPTVUtil.GetUpdatedAt() or None,
        'all_total': len(IPTVUtil.GetChannels()),
        'countries': countries,
    }


@router.get(
    '/groups',
    summary = 'IPTV グループ一覧 API',
    response_description = '取り込んだチャンネルに含まれるグループと、そのチャンネル数。',
    response_model = schemas.IPTVGroups,
)
async def IPTVGroupsAPI(
    country: Annotated[str | None, Query(description='国コード (ISO 3166-1 alpha-2) で絞り込む。')] = None,
    refresh: Annotated[bool, Query(description='キャッシュを無視してプレイリストを再取得するかどうか。')] = False,
):
    """
    取り込んだ IPTV チャンネルに含まれるグループを、チャンネル数付きで取得する。<br>
    国コードを指定すると、その国に含まれるグループのみを返す。
    """

    # IPTV 機能が無効の場合は空の一覧を返す
    if Config().iptv.enabled is False:
        return {'total': 0, 'groups': []}

    # チャンネル一覧を取得してからグループを集計する
    await IPTVUtil.RefreshChannels(force = refresh)
    filtered = IPTVUtil.FilterChannels(country = country)
    groups = IPTVUtil.GetGroups(filtered)

    return {
        'total': len(groups),
        'groups': groups,
    }


@router.get(
    '/playlist.m3u',
    summary = 'IPTV プレイリスト出力 API',
    response_class = PlainTextResponse,
)
async def IPTVPlaylistAPI(
    country: Annotated[str | None, Query(description='国コード (ISO 3166-1 alpha-2) で絞り込む。')] = None,
    group: Annotated[str | None, Query(description='グループ名で絞り込む。')] = None,
    refresh: Annotated[bool, Query(description='キャッシュを無視してプレイリストを再取得するかどうか。')] = False,
):
    """
    取得済みの IPTV チャンネルを M3U プレイリストとして出力する。<br>
    VLC など他のプレイヤーに KonomiTV サーバー経由のストリーム URL を渡したいときに利用する。
    """

    # チャンネル一覧を取得してから、M3U プレイリストを生成する
    await IPTVUtil.RefreshChannels(force = refresh)
    filtered = IPTVUtil.FilterChannels(country = country, group = group)
    return PlainTextResponse(
        IPTVUtil.BuildM3UPlaylist(filtered),
        media_type = 'audio/x-mpegurl',
        headers = {'Cache-Control': 'no-store'},
    )


@router.get(
    '/sources',
    summary = 'IPTV プレイリストソース一覧 API',
    response_description = '登録されている M3U プレイリストのソース一覧。',
    response_model = schemas.IPTVSources,
)
async def IPTVSourcesAPI():
    """
    登録されている M3U プレイリストのソース (config.yaml の設定 + 追加登録分) の一覧を取得する。
    """

    config_sources = [source.strip() for source in Config().iptv.sources if source.strip() != '']
    user_sources = IPTVUtil.LoadUserSources()
    return {
        'config_sources': config_sources,
        'user_sources': user_sources,
    }


@router.post(
    '/sources',
    summary = 'IPTV プレイリストソース追加 API',
    response_description = '追加後の M3U プレイリストのソース一覧。',
    response_model = schemas.IPTVSources,
)
async def IPTVSourceAddAPI(
    payload: schemas.IPTVSourceAddRequest,
):
    """
    M3U プレイリストのソース (URL またはローカルファイルのパス) を追加登録する。<br>
    追加登録した内容は server/data/iptv_sources.json に保存され、次回起動時にも引き継がれる。
    """

    source = payload.url.strip()
    if source == '':
        raise HTTPException(
            status_code = status.HTTP_400_BAD_REQUEST,
            detail = 'The playlist URL must not be empty.',
        )

    # 追加登録分のソースに追加する (既に登録済みの場合は何もしない)
    user_sources = IPTVUtil.LoadUserSources()
    if source not in user_sources:
        user_sources.append(source)
        IPTVUtil.SaveUserSources(user_sources)
        logging.info(f'IPTV playlist source added: {source}')

    # 追加したソースを即座に反映させる
    await IPTVUtil.RefreshChannels(force = True)

    return {
        'config_sources': [item.strip() for item in Config().iptv.sources if item.strip() != ''],
        'user_sources': IPTVUtil.LoadUserSources(),
    }


@router.delete(
    '/sources',
    summary = 'IPTV プレイリストソース削除 API',
    response_description = '削除後の M3U プレイリストのソース一覧。',
    response_model = schemas.IPTVSources,
)
async def IPTVSourceDeleteAPI(
    url: Annotated[str, Query(description='削除する M3U プレイリストの URL。')],
):
    """
    追加登録した M3U プレイリストのソースを削除する。config.yaml に設定されたソースは削除できない。
    """

    user_sources = IPTVUtil.LoadUserSources()
    if url in user_sources:
        user_sources.remove(url)
        IPTVUtil.SaveUserSources(user_sources)
        logging.info(f'IPTV playlist source removed: {url}')
        # ソースの削除を即座に反映させる
        await IPTVUtil.RefreshChannels(force = True)

    return {
        'config_sources': [item.strip() for item in Config().iptv.sources if item.strip() != ''],
        'user_sources': IPTVUtil.LoadUserSources(),
    }


@router.get(
    '/tvui',
    summary = 'IPTV テレビ視聴 UI 登録チャンネル一覧 API',
    response_description = 'テレビ視聴 UI に登録された IPTV チャンネルの一覧。',
    response_model = schemas.IPTVTVUIChannels,
)
async def IPTVTVUIChannelsAPI(
    request: Request,
    response: Response,
):
    """
    テレビ視聴 UI (TV ホームの IPTV タブと /tv/watch/) に登録された IPTV チャンネルの一覧を取得する。

    登録内容は呼び出し元 (ログイン中のユーザー、または Cookie の匿名 ID) ごとに分離されており、
    他のユーザーの登録内容は返さない。
    """

    return BuildTVUIResponse(await ResolveUserKey(request, response))


@router.post(
    '/tvui',
    summary = 'IPTV テレビ視聴 UI 登録 API',
    response_description = '登録後の IPTV チャンネルの一覧。',
    response_model = schemas.IPTVTVUIChannels,
)
async def IPTVTVUIRegisterAPI(
    payload: schemas.IPTVTVUIRegisterRequest,
    request: Request,
    response: Response,
):
    """
    呼び出し元 (ログイン中のユーザー、または Cookie の匿名 ID) のテレビ視聴 UI に IPTV チャンネルを登録する。

    登録したチャンネルは、TV ホームの「IPTV」タブと /tv/watch/{display_channel_id} で視聴できるようになる。
    登録内容は呼び出し元ごとに分離されており、server/data/iptv_tvui_channels.json に保存される。
    """

    if Config().iptv.enabled is False:
        raise HTTPException(status_code=status.HTTP_400_BAD_REQUEST, detail = 'IPTV is disabled.')

    # 登録前にチャンネルが存在するか確認する (プレイリストが未取得の場合はここで取得される)
    await IPTVUtil.RefreshChannels()
    channel = IPTVUtil.GetChannelByDisplayChannelID(payload.display_channel_id)
    if channel is None:
        raise HTTPException(
            status_code = status.HTTP_404_NOT_FOUND,
            detail = 'Specified IPTV channel was not found.',
        )

    # 呼び出し元のテレビ視聴 UI に登録する
    user_key = await ResolveUserKey(request, response)
    IPTVUtil.RegisterTVUIChannel(user_key, payload.display_channel_id)
    logging.info(f'IPTV channel registered to TV UI: {channel.name} ({payload.display_channel_id}) [user_key: {user_key}]')

    return BuildTVUIResponse(user_key)


@router.delete(
    '/tvui',
    summary = 'IPTV テレビ視聴 UI 登録解除 API',
    response_description = '解除後の IPTV チャンネルの一覧。',
    response_model = schemas.IPTVTVUIChannels,
)
async def IPTVTVUIUnregisterAPI(
    display_channel_id: Annotated[str, Query(description='登録解除する IPTV チャンネルの display_channel_id。')],
    request: Request,
    response: Response,
):
    """
    呼び出し元 (ログイン中のユーザー、または Cookie の匿名 ID) のテレビ視聴 UI から、IPTV チャンネルの登録を解除する。
    """

    user_key = await ResolveUserKey(request, response)
    IPTVUtil.UnregisterTVUIChannel(user_key, display_channel_id)
    return BuildTVUIResponse(user_key)


@router.get(
    '/proxy',
    summary = 'IPTV ストリームプロキシ API',
    response_class = StreamingResponse,
)
async def IPTVProxyAPI(
    url: Annotated[str, Query(description='プロキシするストリームの URL。')],
    range_header: Annotated[str | None, Query(alias='range')] = None,
):
    """
    IPTV のストリームをプロキシして配信する。<br>
    HLS プレイリストの場合は、内部のセグメント URL をこの API 経由に書き換えて返す。
    """

    # URL を検証する
    target_url = ValidateProxyURL(url)
    # プロキシして配信する
    return await ProxyStream(target_url, range_header)


@router.get(
    '/logo',
    summary = 'IPTV チャンネルロゴプロキシ API',
)
async def IPTVLogoProxyAPI(
    url: Annotated[str, Query(description='プロキシするチャンネルロゴの URL。')],
):
    """
    IPTV のチャンネルロゴをプロキシして配信する。<br>
    ロゴ画像は CORS やホットリンク制限でブラウザから直接読み込めないことがあるため、この API を経由させる。
    """

    # URL を検証する
    target_url = ValidateProxyURL(url)

    # ロゴ画像を取得する
    try:
        async with httpx.AsyncClient(
            follow_redirects = True,
            timeout = Config().iptv.request_timeout,
        ) as client:
            response = await client.get(target_url, headers = {
                'User-Agent': Config().iptv.user_agent,
                **PROXY_ACCEPT_HEADERS,
            })
    except (httpx.NetworkError, httpx.TimeoutException):
        # 取得できなかった場合は既定のロゴを返す
        return BuildDefaultLogoResponse()

    # 上流がエラーを返した場合も既定のロゴを返す (ロゴが無いだけなので、クライアント側でエラーを出さない)
    if response.status_code != 200:
        return BuildDefaultLogoResponse()

    return StreamingResponse(
        iter([response.content]),
        media_type = response.headers.get('Content-Type', 'image/png'),
        headers = {
            # ロゴは頻繁に変わらないため、1日キャッシュさせる
            'Cache-Control': 'public, max-age=86400',
        },
    )
