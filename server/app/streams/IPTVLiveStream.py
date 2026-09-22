"""
IPTV のストリームを FFmpeg で MPEG-TS に変換し、KonomiTV のテレビ視聴 UI 向けに配信するモジュール。

KonomiTV のテレビ視聴画面は、放送波 (EDCB / Mirakurun) のライブストリームと同様に
`/api/streams/live/{display_channel_id}/{quality}/mpegts` から MPEG-TS を受け取って再生する。
そのため IPTV のストリーム (主に HLS) を、再生前に MPEG-TS へ変換しておく必要がある。

ここでは FFmpeg を 1 ストリームにつき 1 プロセス起動し、
- 映像は再エンコードせずそのまま (copy)
- 音声はブラウザで再生可能な AAC (ステレオ) に変換
して MPEG-TS を標準出力へ流す。

IPTV のストリームは配信元が停止していることが珍しくないため、最初のデータが届くまで待って、
まったくデータが得られなかった場合は「ストリームを開けなかった」ことを呼び出し側に伝える
(空のストリームを返すと、ブラウザ側が MSE のデマルチプレクサエラーで無限に再試行してしまう) 。
"""

import asyncio
from collections.abc import AsyncIterator

from app import logging
from app.config import Config
from app.constants import LIBRARY_PATH
from app.utils.IPTVUtil import IPTVChannel


# FFmpeg から一度に読み取るデータサイズ (バイト)
READ_CHUNK_SIZE = 64 * 1024

# 最初の MPEG-TS データが届くまで待つ最大時間 (秒)
OPEN_TIMEOUT_SECONDS = 25.0

# FFmpeg のエラー出力をログへ残す際の最大長
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


class IPTVMPEGTSStream:
    """
    IPTV の 1 ストリームを FFmpeg で MPEG-TS に変換して配信するクラス。

    open() で FFmpeg を起動して最初のデータが届くまで待ち、
    iterate() で残りのデータを yield し、close() で FFmpeg を確実に終了させる。
    """

    def __init__(self, channel: IPTVChannel) -> None:
        """
        Args:
            channel (IPTVChannel): 配信する IPTV チャンネル
        """

        # 配信する IPTV チャンネル
        self.channel = channel

        # 起動した FFmpeg のプロセス (open() で設定される)
        self._process: asyncio.subprocess.Process | None = None

        # FFmpeg の終了を待ち、標準エラー出力を回収するタスク
        ## ジェネレーターがキャンセルされても、このタスクがプロセスを回収する
        self._reaper_task: asyncio.Task[tuple[int, str]] | None = None

    async def open(self, timeout: float = OPEN_TIMEOUT_SECONDS) -> bytes | None:
        """
        FFmpeg を起動し、最初の MPEG-TS データが届くまで待つ。

        Args:
            timeout (float): 最初のデータを待つ最大時間 (秒)

        Returns:
            bytes | None: 最初の MPEG-TS データ (取得できなかった場合は None)
        """

        self._process = await asyncio.create_subprocess_exec(
            *BuildFFmpegArguments(self.channel),
            stdin = asyncio.subprocess.DEVNULL,
            stdout = asyncio.subprocess.PIPE,
            stderr = asyncio.subprocess.PIPE,
        )
        logging.info(f'[IPTVLiveStream] Started FFmpeg for "{self.channel.name}" (pid={self._process.pid})')

        # FFmpeg の終了待ちを別のタスクで行う
        self._reaper_task = asyncio.ensure_future(self._collect_stderr_and_wait())

        # 最初のデータが届くまで待つ
        assert self._process.stdout is not None
        try:
            first_chunk = await asyncio.wait_for(self._process.stdout.read(READ_CHUNK_SIZE), timeout=timeout)
        except TimeoutError:
            logging.warning(f'[IPTVLiveStream] Timed out while opening the stream for "{self.channel.name}".')
            await self.close()
            return None

        # データが得られなかった場合は FFmpeg が起動直後に終了している (配信元が停止しているなど)
        if not first_chunk:
            returncode, stderr_text = await self._collect_result()
            logging.warning(
                f'[IPTVLiveStream] Failed to open the stream for "{self.channel.name}". '
                f'(rc={returncode}) {stderr_text[:MAX_STDERR_LOG_LENGTH]}'
            )
            self._process = None
            return None

        return first_chunk

    async def iterate(self) -> AsyncIterator[bytes]:
        """
        残りの MPEG-TS データを yield する非同期ジェネレーター。

        Yields:
            bytes: MPEG-TS のストリームデータ
        """

        if self._process is None or self._process.stdout is None:
            return
        while True:
            chunk = await self._process.stdout.read(READ_CHUNK_SIZE)
            # 空のチャンクはストリームの終端 (FFmpeg が終了した) を意味する
            if not chunk:
                break
            yield chunk

    async def close(self) -> None:
        """FFmpeg を確実に終了させ、終了コードとエラー出力をログに残す。"""

        process = self._process
        if process is None:
            return

        # FFmpeg を終了させる
        if process.returncode is None:
            try:
                process.kill()
            except ProcessLookupError:
                pass

        # 回収タスクから終了コードとエラー出力を取得してログに残す
        ## ジェネレーターがキャンセルされている場合は wait_for が即座に CancelledError になるため、
        ## その場合でも kill() は実行済みなのでプロセスは残らない (回収タスクがバックグラウンドで終了させる)
        try:
            returncode, stderr_text = await asyncio.wait_for(
                asyncio.shield(self._reaper_task), timeout=5.0,
            ) if self._reaper_task is not None else (process.returncode, '')
            logging.info(f'[IPTVLiveStream] FFmpeg for "{self.channel.name}" stopped. (pid={process.pid}, rc={returncode})')
            # 異常終了していた場合は、原因調査のためにエラー出力をログに残す
            if returncode not in (0, -9) and stderr_text != '':
                logging.warning(
                    f'[IPTVLiveStream] FFmpeg failed for "{self.channel.name}": '
                    f'{stderr_text[:MAX_STDERR_LOG_LENGTH]}'
                )
        except (TimeoutError, asyncio.CancelledError):
            logging.info(f'[IPTVLiveStream] FFmpeg for "{self.channel.name}" stopped. (pid={process.pid})')

    async def _collect_stderr_and_wait(self) -> tuple[int, str]:
        """FFmpeg の終了を待ち、標準エラー出力を回収する。"""

        process = self._process
        assert process is not None

        stderr_text = ''
        if process.stderr is not None:
            stderr_text = (await process.stderr.read()).decode('utf-8', 'replace').strip()
        returncode = await process.wait()
        return returncode, stderr_text

    async def _collect_result(self) -> tuple[int | None, str]:
        """回収タスクの結果 (終了コードと標準エラー出力) を取得する。"""

        if self._reaper_task is None:
            return (self._process.returncode if self._process is not None else None), ''
        try:
            return await asyncio.wait_for(asyncio.shield(self._reaper_task), timeout=5.0)
        except (TimeoutError, asyncio.CancelledError):
            return (self._process.returncode if self._process is not None else None), ''
