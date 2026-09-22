"""
IPTV のストリームを FFmpeg で MPEG-TS に変換し、KonomiTV のテレビ視聴 UI 向けに配信するモジュール。

KonomiTV のテレビ視聴画面は、放送波 (EDCB / Mirakurun) のライブストリームと同様に
`/api/streams/live/{display_channel_id}/{quality}/mpegts` から MPEG-TS を受け取って再生する。
そのため IPTV のストリーム (主に HLS) を、再生前に MPEG-TS へ変換しておく必要がある。

ここでは FFmpeg を 1 リクエストにつき 1 プロセス起動し、
- 映像は再エンコードせずそのまま (copy)
- 音声はブラウザで再生可能な AAC (ステレオ) に変換
して MPEG-TS を標準出力へ流す。呼び出し側のジェネレーターが閉じられると FFmpeg も終了する。
"""

import asyncio
from collections.abc import AsyncIterator

from app import logging
from app.config import Config
from app.constants import LIBRARY_PATH
from app.utils.IPTVUtil import IPTVChannel


# FFmpeg から一度に読み取るデータサイズ (バイト)
READ_CHUNK_SIZE = 64 * 1024

# FFmpeg が起動直後に終了してしまった場合にログへ残すエラーメッセージの最大長
MAX_STDERR_LOG_LENGTH = 500


def BuildFFmpegArguments(channel: IPTVChannel) -> list[str]:
    """
    IPTV のストリームを MPEG-TS に変換する FFmpeg の引数を組み立てる。

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
        # 映像は再エンコードしない (CPU 負荷を抑える)
        '-c:v', 'copy',
        # 音声はブラウザで再生できる AAC (ステレオ) に変換する
        '-c:a', 'aac',
        '-b:a', '128k',
        '-ac', '2',
        # MPEG-TS として標準出力へ出力する
        '-f', 'mpegts',
        'pipe:1',
    ]
    return args


async def StreamIPTVAsMPEGTS(channel: IPTVChannel) -> AsyncIterator[bytes]:
    """
    IPTV のストリームを FFmpeg で MPEG-TS に変換し、チャンク単位で yield する非同期ジェネレーター。

    呼び出し側がジェネレーターを閉じた (またはクライアントが切断した) 場合は、
    finally で FFmpeg プロセスを確実に終了させる。

    Args:
        channel (IPTVChannel): 配信する IPTV チャンネル

    Yields:
        bytes: MPEG-TS のストリームデータ
    """

    # FFmpeg を起動する
    ## 標準エラー出力はパイプで受け取り、異常終了した場合のログ出力に使う
    process = await asyncio.create_subprocess_exec(
        *BuildFFmpegArguments(channel),
        stdin = asyncio.subprocess.DEVNULL,
        stdout = asyncio.subprocess.PIPE,
        stderr = asyncio.subprocess.PIPE,
    )
    logging.info(f'[IPTVLiveStream] Started FFmpeg for "{channel.name}" (pid={process.pid})')

    async def collect_stderr_and_wait() -> tuple[int, str]:
        """FFmpeg の終了を待ち、標準エラー出力を回収する。"""

        stderr_text = ''
        if process.stderr is not None:
            stderr_text = (await process.stderr.read()).decode('utf-8', 'replace').strip()
        returncode = await process.wait()
        return returncode, stderr_text

    # FFmpeg の終了待ちを別のタスクで行う
    ## ジェネレーターがキャンセルされた場合でも、このタスクがプロセスを回収する
    reaper_task = asyncio.ensure_future(collect_stderr_and_wait())

    try:
        # FFmpeg の出力をチャンク単位で転送する
        assert process.stdout is not None
        while True:
            chunk = await process.stdout.read(READ_CHUNK_SIZE)
            # 空のチャンクはストリームの終端 (FFmpeg が終了した) を意味する
            if not chunk:
                break
            yield chunk
    finally:
        # 転送が終わったら (またはクライアントが切断したら) FFmpeg を必ず終了させる
        if process.returncode is None:
            try:
                process.kill()
            except ProcessLookupError:
                pass

        # 回収タスクから終了コードとエラー出力を取得してログに残す
        ## ジェネレーターがキャンセルされている場合は wait_for が即座に CancelledError になるため、
        ## その場合でも kill() は実行済みなのでプロセスは残らない (回収タスクがバックグラウンドで終了させる)
        try:
            returncode, stderr_text = await asyncio.wait_for(asyncio.shield(reaper_task), timeout=5.0)
            logging.info(f'[IPTVLiveStream] FFmpeg for "{channel.name}" stopped. (pid={process.pid}, rc={returncode})')
            # 異常終了していた場合は、原因調査のためにエラー出力をログに残す
            if returncode not in (0, -9) and stderr_text != '':
                logging.warning(
                    f'[IPTVLiveStream] FFmpeg failed for "{channel.name}": '
                    f'{stderr_text[:MAX_STDERR_LOG_LENGTH]}'
                )
        except (TimeoutError, asyncio.CancelledError):
            logging.info(f'[IPTVLiveStream] FFmpeg for "{channel.name}" stopped. (pid={process.pid})')
