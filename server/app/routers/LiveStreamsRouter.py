
import asyncio
import copy
import time
from typing import Annotated

from fastapi import APIRouter, Depends, HTTPException, Path, status
from fastapi.requests import Request
from fastapi.responses import Response, StreamingResponse
from sse_starlette.sse import EventSourceResponse
from starlette.types import Receive

from app import logging, schemas
from app.models.Channel import Channel
from app.streams.IPTVLiveStream import IPTVMPEGTSStream
from app.streams.LiveStream import LiveStream, LiveStreamStatus
from app.streams.StreamEncodingOptions import (
    SplitQualityAndEncodingOptions,
    StreamQualityWithOptions,
)
from app.utils import IPTVUtil
from app.utils.IPTVUtil import IPTVChannel


# ルーター
router = APIRouter(
    tags = ['Streams'],
    prefix = '/api/streams/live',
)


async def ValidateChannelID(display_channel_id: Annotated[str, Path(description='チャンネル ID 。ex: gr011')]) -> str:
    """ チャンネル ID のバリデーション """

    # IPTV ページからテレビ視聴 UI に登録された IPTV の疑似チャンネルの場合
    if IPTVUtil.IsIPTVDisplayChannelID(display_channel_id):
        if IPTVUtil.GetChannelByDisplayChannelID(display_channel_id) is None:
            logging.error(f'[LiveStreamsRouter][ValidateChannelID] Specified IPTV display_channel_id was not found. [display_channel_id: {display_channel_id}]')
            raise HTTPException(
                status_code = status.HTTP_422_UNPROCESSABLE_ENTITY,
                detail = 'Specified display_channel_id was not found',
            )
        return display_channel_id

    # チャンネル ID が存在するか確認
    if await Channel.filter(display_channel_id=display_channel_id).get_or_none() is None:
        logging.error(f'[LiveStreamsRouter][ValidateChannelID] Specified display_channel_id was not found. [display_channel_id: {display_channel_id}]')
        raise HTTPException(
            status_code = status.HTTP_422_UNPROCESSABLE_ENTITY,
            detail = 'Specified display_channel_id was not found',
        )

    return display_channel_id


async def ValidateQuality(quality: Annotated[str, Path(description='映像の品質。ex: 1080p')]) -> StreamQualityWithOptions:
    """ 映像の品質のバリデーション """

    # 指定された品質が存在するか確認
    ## 品質の指定に -10bit や -24fps が付いていれば分解する
    stream_quality = SplitQualityAndEncodingOptions(quality)
    if stream_quality is None:
        logging.error(f'[LiveStreamsRouter][ValidateQuality] Specified quality was not found. [quality: {quality}]')
        raise HTTPException(
            status_code = status.HTTP_422_UNPROCESSABLE_ENTITY,
            detail = 'Specified quality was not found',
        )

    return stream_quality


# ***** IPTV の疑似チャンネル向けのヘルパー *****


def BuildIPTVLiveStreamStatus(display_channel_id: str) -> schemas.LiveStreamStatus:
    """
    IPTV の疑似チャンネルのライブストリームステータスを生成する。

    IPTV のストリームは KonomiTV のエンコードタスクを介さずに直接配信されるため、
    状態は常に ONAir として扱う。

    Args:
        display_channel_id (str): IPTV チャンネルの display_channel_id

    Returns:
        schemas.LiveStreamStatus: ライブストリームのステータス
    """

    now = time.time()
    return schemas.LiveStreamStatus(
        status = 'ONAir',
        detail = 'IPTV のストリームを配信しています。',
        started_at = now,
        updated_at = now,
        client_count = LiveStream.getViewerCount(display_channel_id),
    )


async def BuildIPTVMPEGTSResponse(request: Request, channel: IPTVChannel) -> StreamingResponse:
    """
    IPTV チャンネルの MPEG-TS ストリームを配信する StreamingResponse を生成する。

    配信元が停止しているなどでストリームを開けなかった場合は、空のストリームを返さずに
    502 Bad Gateway を返す。空のストリームを返すと、ブラウザ側は MSE のデマルチプレクサエラー
    (DEMUXER_ERROR_COULD_NOT_OPEN) として扱い、再生の再試行を繰り返してしまうため。

    Args:
        request (Request): クライアントからのリクエスト
        channel (IPTVChannel): 配信する IPTV チャンネル

    Returns:
        StreamingResponse: MPEG-TS ストリームのレスポンス

    Raises:
        HTTPException: ストリームを開けなかった場合 (502 Bad Gateway)
    """

    # FFmpeg を起動し、最初のデータが届くまで待つ
    stream = IPTVMPEGTSStream(channel)
    first_chunk = await stream.open()

    # 最初のデータが届かなかった場合は、配信元が停止しているためエラーを返す
    if first_chunk is None:
        raise HTTPException(
            status_code = status.HTTP_502_BAD_GATEWAY,
            detail = 'Failed to open the IPTV stream. The stream may be offline or unavailable.',
        )

    async def generator():
        """IPTV のストリームを読み取って出力するジェネレーター"""

        try:
            # 最初に読み取ったデータを出力する
            yield first_chunk
            # 残りのデータを出力する
            async for stream_data in stream.iterate():
                # クライアントが切断した場合はジェネレーターを終了する (FFmpeg も close() で終了する)
                if await request.is_disconnected():
                    logging.debug(f'[LiveStreamsRouter] IPTV request is disconnected. [display_channel_id: {channel.id}]')
                    break
                yield stream_data
        finally:
            # FFmpeg を確実に終了させる
            await stream.close()

    return StreamingResponse(generator(), media_type='video/mp2t')


def BuildIPTVEventResponse(display_channel_id: str) -> EventSourceResponse:
    """
    IPTV の疑似チャンネルのイベントストリームを生成する。

    IPTV のストリームはエンコードタスクの状態変化を持たないため、
    初回に ONAir のステータスを 1 回送信したあとは接続を維持するだけにする。

    Args:
        display_channel_id (str): IPTV チャンネルの display_channel_id

    Returns:
        EventSourceResponse: イベントストリームのレスポンス
    """

    async def generator():
        """イベントストリームを出力するジェネレーター"""

        # 初回接続時に必ず現在のステータスを返す
        yield {
            'event': 'initial_update',
            'data': BuildIPTVLiveStreamStatus(display_channel_id).model_dump_json(),
        }

        # 以降はステータスが変わらないため、接続を維持するだけにする
        ## keep-alive のコメントは EventSourceResponse が定期的に送信してくれる
        while True:
            await asyncio.sleep(10)

    return EventSourceResponse(generator())


@router.get(
    '',
    summary = 'ライブストリーム一覧 API',
    response_description = 'ステータスごとに分類された、すべてのライブストリームの状態。',
    response_model = schemas.LiveStreamStatuses,
)
async def LiveStreamsAPI():
    """
    すべてのライブストリームの状態を Offline・Standby・ONAir・Idling・Restart の各ステータスごとに取得する。
    """

    # 返却するデータ
    # 逆順になっているのは、デバッグ時に全体の大半を占める Offline なストリームが邪魔なため
    result: dict[str, dict[str, LiveStreamStatus]] = {
        'Restart': {},
        'Idling' : {},
        'ONAir'  : {},
        'Standby': {},
        'Offline': {},
    }

    # すべてのストリームごとに
    for live_stream in LiveStream.getAllLiveStreams():
        live_stream_status = live_stream.getStatus()
        result[live_stream_status.status][live_stream.live_stream_id] = live_stream_status

    # すべてのライブストリームの状態を返す
    return result


@router.get(
    '/{display_channel_id}/{quality}',
    summary = 'ライブストリーム API',
    response_description = 'ライブストリームの状態。',
    response_model = schemas.LiveStreamStatus,
)
async def LiveStreamAPI(
    display_channel_id: Annotated[str, Depends(ValidateChannelID)],
    stream_quality: Annotated[StreamQualityWithOptions, Depends(ValidateQuality)],
):
    """
    ライブストリームの状態を取得する。<br>
    ライブストリーム イベント API にて配信されるイベントと同一のデータだが、一回限りの取得である点が異なる。
    """

    # IPTV の疑似チャンネルの場合は、常に ONAir 状態として扱う
    if IPTVUtil.IsIPTVDisplayChannelID(display_channel_id):
        return BuildIPTVLiveStreamStatus(display_channel_id)

    # 品質とオプション指定に対応する LiveStream を取得する
    # ステータスを取得したいだけなので、接続はしない
    live_stream = LiveStream(display_channel_id, stream_quality.quality, stream_quality.encoding_options)

    # 取得してきた値をそのまま返す
    return live_stream.getStatus()


@router.get(
    '/{display_channel_id}/{quality}/events',
    summary = 'ライブストリーム イベント API',
    response_class = Response,
    responses = {
        status.HTTP_200_OK: {
            'description': 'ライブストリームのイベントが随時配信されるイベントストリーム。',
            'content': {'text/event-stream': {}},
        }
    }
)
async def LiveStreamEventAPI(
    display_channel_id: Annotated[str, Depends(ValidateChannelID)],
    stream_quality: Annotated[StreamQualityWithOptions, Depends(ValidateQuality)],
):
    """
    ライブストリームのイベントを Server-Sent Events で随時配信する。

    イベントには、
    - 初回接続時に現在のステータスを示す **initial_update**
    - ステータスの更新を示す **status_update**
    - ステータス詳細の更新を示す **detail_update**
    - クライアント数の更新を示す **clients_update**
    の4種類がある。

    どのイベントでも配信される JSON 構造は同じ。<br>
    ステータスが Offline になった、あるいは既にそうなっている時は、status_update イベントが配信された後に接続を終了する。
    """

    # IPTV の疑似チャンネルの場合は、状態変化が無いイベントストリームを返す
    if IPTVUtil.IsIPTVDisplayChannelID(display_channel_id):
        return BuildIPTVEventResponse(display_channel_id)

    # 品質とオプション指定に対応する LiveStream を取得する
    # ステータスを取得したいだけなので、接続はしない
    live_stream = LiveStream(display_channel_id, stream_quality.quality, stream_quality.encoding_options)

    # ステータスの変更を監視し、変更があればステータスをイベントストリームとして出力する
    async def generator():
        """イベントストリームを出力するジェネレーター"""

        # 初期値
        previous_status = live_stream.getStatus()

        # 取得できたクライアント数はあくまで同じチャンネル+同じ画質で視聴中のクライアントをカウントしたものなので、
        # 同じチャンネル+すべての画質で視聴中のクライアント数を別途取得して上書きする
        previous_status.client_count = LiveStream.getViewerCount(display_channel_id)

        # 初回接続時に必ず現在のステータスを返す
        yield {
            'event': 'initial_update',  # initial_update イベントを設定
            'data': previous_status.model_dump_json(),
        }

        while True:

            # 現在のライブストリームのステータスを取得
            status = live_stream.getStatus()

            # 取得できたクライアント数はあくまで同じチャンネル+同じ画質で視聴中のクライアントをカウントしたものなので、
            # 同じチャンネル+すべての画質で視聴中のクライアント数を別途取得して上書きする
            status.client_count = LiveStream.getViewerCount(display_channel_id)

            # 以前の結果と異なっている場合のみレスポンスを返す
            if previous_status != status:

                # ステータスが以前と異なる
                if previous_status.status != status.status:
                    yield {
                        'event': 'status_update',  # status_update イベントを設定
                        'data': status.model_dump_json(),
                    }
                # 詳細が以前と異なる
                elif previous_status.detail != status.detail:
                    yield {
                        'event': 'detail_update',  # detail_update イベントを設定
                        'data': status.model_dump_json(),
                    }
                # クライアント数が以前と異なる
                elif previous_status.client_count != status.client_count:
                    yield {
                        'event': 'clients_update',  # clients_update イベントを設定
                        'data': status.model_dump_json(),
                    }

                # 取得結果を保存
                previous_status = copy.copy(status)

            # 一応スリープを入れておく
            await asyncio.sleep(0.05)

    # EventSourceResponse でイベントストリームを配信する
    return EventSourceResponse(generator())


# ***** ライブ PSI/SI アーカイブデータストリーミング API *****


@router.get(
    '/{display_channel_id}/{quality}/psi-archived-data',
    summary = 'ライブ PSI/SI アーカイブデータストリーミング API',
    response_class = Response,
    responses = {
        status.HTTP_200_OK: {
            'description': 'ライブ PSI/SI アーカイブデータストリーム。',
            'content': {'application/octet-stream': {}},
        }
    }
)
async def LivePSIArchivedDataAPI(
    request: Request,
    display_channel_id: Annotated[str, Depends(ValidateChannelID)],
    stream_quality: Annotated[StreamQualityWithOptions, Depends(ValidateQuality)],
):
    """
    ライブ PSI/SI アーカイブデータストリームを配信する。

    何らかの理由でライブストリームが終了しない限り、継続的にレスポンスが出力される（ストリーミング）。
    """

    # IPTV の疑似チャンネルには PSI/SI アーカイブデータが存在しないため、空のレスポンスを返す
    if IPTVUtil.IsIPTVDisplayChannelID(display_channel_id):
        return Response(content = b'', media_type = 'application/octet-stream')

    # 品質とオプション指定に対応する LiveStream を取得する
    # PSI/SI アーカイブデータを取得したいだけなので、接続はしない
    live_stream = LiveStream(display_channel_id, stream_quality.quality, stream_quality.encoding_options)

    # LivePSIDataArchiver がまだ初期化されていない場合は、起動するまで最大10秒待つ
    ## LivePSIDataArchiver は LiveEncodingTask が起動次第自動的に初期化されるので、ここでは待つだけ
    for _ in range(20):
        if live_stream.psi_data_archiver is not None:
            break
        await asyncio.sleep(0.5)

    # 10秒待っても起動しなかった場合はエラー
    if live_stream.psi_data_archiver is None:
        logging.error(f'{live_stream.log_prefix} PSI/SI Data Archiver is not running.')
        raise HTTPException(
            status_code = status.HTTP_500_INTERNAL_SERVER_ERROR,
            detail = 'PSI/SI Data Archiver is not running',
        )

    # StreamingResponse で読み取ったストリームデータをストリーミングする
    # LivePSIDataArchiver.getPSIArchivedData() は AsyncGenerator なので、そのまま渡せる
    response = StreamingResponse(live_stream.psi_data_archiver.getPSIArchivedData(request), media_type='application/octet-stream')

    # HTTP リクエストがキャンセルされたときに psisiarc を終了できるよう、StreamingResponse のインスタンスにモンキーパッチを当てる
    # モンキーパッチしている理由は LiveMPEGTSStreamAPI と同じ
    # ref: https://github.com/encode/starlette/pull/839
    async def listen_for_disconnect_monkeypatch(receive: Receive) -> None:
        try:
            while True:
                message = await receive()
                if message['type'] == 'http.disconnect':
                    # 上のループで HTTP リクエストの切断を検知できるようにしばらく待つ
                    await asyncio.sleep(5)
                    break
        except asyncio.CancelledError:
            pass
    response.listen_for_disconnect = listen_for_disconnect_monkeypatch

    return response


# ***** MPEG-TS ストリーミング API *****


@router.get(
    '/{display_channel_id}/{quality}/mpegts',
    summary = 'ライブ MPEG-TS ストリーム API',
    response_class = Response,
    responses = {
        status.HTTP_200_OK: {
            'description': 'ライブ MPEG-TS ストリーム。',
            'content': {'video/mp2t': {}},
        }
    }
)
async def LiveMPEGTSStreamAPI(
    request: Request,
    display_channel_id: Annotated[str, Depends(ValidateChannelID)],
    stream_quality: Annotated[StreamQualityWithOptions, Depends(ValidateQuality)],
):
    """
    ライブ MPEG-TS ストリームを配信する。

    同じチャンネル ID 、同じ画質のライブストリームが Offline 状態のときは、新たにエンコードタスクを立ち上げて、
    ONAir 状態になるのを待機してからストリームデータを配信する。<br>
    同じチャンネル ID 、同じ画質のライブストリームが ONAir や Idling 状態のときは、新たにエンコードタスクを立ち上げることなく、他のクライアントとストリームデータを共有して配信する。

    何らかの理由でライブストリームが終了しない限り、継続的にレスポンスが出力される（ストリーミング）。
    """

    # IPTV の疑似チャンネルの場合は、FFmpeg で MPEG-TS に変換して配信する
    iptv_channel = IPTVUtil.GetChannelByDisplayChannelID(display_channel_id)
    if iptv_channel is not None:
        return await BuildIPTVMPEGTSResponse(request, iptv_channel)

    # 品質とオプション指定に対応する LiveStream に接続し、ライブストリームクライアントを取得する
    ## 接続時に Offline だった場合は自動的にエンコードタスクが起動される
    live_stream = LiveStream(display_channel_id, stream_quality.quality, stream_quality.encoding_options)
    live_stream_client = await live_stream.connect('mpegts')

    # ライブストリームを出力するジェネレーター
    async def generator():
        while True:

            # リクエストがキャンセル（切断）されている場合
            ## エンコードに失敗とかしない限り基本エンドレスで配信されるので、
            ## チャンネル変えたりやタブの再読み込みで必然的にリクエストがキャンセルされる
            if await request.is_disconnected():

                # ライブストリームへの接続を切断し、ループを終了する
                logging.debug(f'{live_stream.log_prefix} Request is disconnected.')
                live_stream.disconnect(live_stream_client)
                break

            if live_stream.getStatus().status != 'Offline':

                # クライアントが持つ Queue から読み取ったストリームデータ
                stream_data: bytes | None = await live_stream_client.readStreamData()

                # 読み取ったストリームデータを yield で随時出力する
                if stream_data is not None:
                    yield stream_data

                # stream_data に None が入った場合はエンコードタスクが終了し、接続が切断されたものとみなす
                else:

                    # ライブストリームへの接続を切断し、ループを終了する
                    logging.debug(f'{live_stream.log_prefix} Encode task is finished.')
                    live_stream.disconnect(live_stream_client)  # 必要ないとは思うけど念のため
                    break

            # ライブストリームが Offline になった場合もエンコードタスクが終了し、接続が切断されたものとみなす
            else:

                # ライブストリームへの接続を切断し、ループを終了する
                logging.debug(f'{live_stream.log_prefix} LiveStream is currently Offline.')
                live_stream.disconnect(live_stream_client)  # 必要ないとは思うけど念のため
                break

    # StreamingResponse で読み取ったストリームデータをストリーミングする
    response = StreamingResponse(generator(), media_type='video/mp2t')

    # HTTP リクエストがキャンセルされたときに自前でライブストリームの接続を切断できるよう、StreamingResponse のインスタンスにモンキーパッチを当てる
    ## Starlette の StreamingResponse は stream_response() と listen_for_disconnect() を TaskGroup で並行実行し、
    ## listen_for_disconnect() が完了すると cancel_scope.cancel() で stream_response() (ジェネレーター) を強制終了する
    ## デフォルトの listen_for_disconnect() は http.disconnect を受け取ると即座に完了するため、
    ## ジェネレーターが強制終了されて disconnect() が呼ばれず、client_count が減少しない問題があった
    ## これを避けるため listen_for_disconnect() を書き換え、http.disconnect の受信時点で即座に LiveStream.disconnect() を呼び出す
    ## LiveStream.disconnect() は二重呼び出しに安全なので、ジェネレーター側で重複して呼ばれても問題ない
    # ref: https://github.com/encode/starlette/pull/839
    async def listen_for_disconnect_monkeypatch(receive: Receive) -> None:
        try:
            while True:
                message = await receive()
                if message['type'] == 'http.disconnect':
                    # HTTP リクエストの切断を検知したら即座にライブストリームへの接続を切断する
                    ## こうすることで client_count が即座に減少し、チューナー再利用の判定が高速化される
                    logging.debug(f'{live_stream.log_prefix} Request is disconnected.')
                    live_stream.disconnect(live_stream_client)
                    break
        except asyncio.CancelledError:
            pass
    response.listen_for_disconnect = listen_for_disconnect_monkeypatch

    return response
