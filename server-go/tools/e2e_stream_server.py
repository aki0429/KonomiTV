"""ライブストリーミングの E2E 検証用に、テスト用の MPEG-2 TS を約 1x 速で無限に配信する HTTP サーバー。

使い方:
    python server-go/tools/e2e_stream_server.py <テスト用 TS ファイル> [ポート]

ファイルの終端まで到達したら先頭に戻って配信を続ける (ライブストリームを模擬する) 。
"""

import http.server
import socketserver
import sys
import time
from pathlib import Path

# 1x 速に近い配信レート (bytes/sec)
BYTES_PER_SECOND = 900 * 1024
# 1 回の書き込みで送るバイト数
CHUNK_SIZE = 64 * 1024


class StreamHandler(http.server.BaseHTTPRequestHandler):
    """テスト用の MPEG-2 TS を配信するハンドラー。"""

    # 配信するファイルの内容
    ts_data = b''

    def do_GET(self) -> None:  # noqa: N802
        if self.path.split('?')[0] != '/test.ts':
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header('Content-Type', 'video/mp2t')
        self.send_header('Cache-Control', 'no-cache')
        self.end_headers()

        position = 0
        started_at = time.monotonic()
        sent = 0
        try:
            while True:
                chunk = self.ts_data[position:position + CHUNK_SIZE]
                position += len(chunk)
                if position >= len(self.ts_data):
                    position = 0
                self.wfile.write(chunk)
                self.wfile.flush()
                sent += len(chunk)
                # 1x 速になるように待機する
                expected = sent / BYTES_PER_SECOND
                elapsed = time.monotonic() - started_at
                if expected > elapsed:
                    time.sleep(expected - elapsed)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def log_message(self, format: str, *args: object) -> None:
        pass


def main() -> None:
    ts_path = Path(sys.argv[1])
    port = int(sys.argv[2]) if len(sys.argv) > 2 else 7099
    StreamHandler.ts_data = ts_path.read_bytes()
    socketserver.ThreadingTCPServer.allow_reuse_address = True
    with socketserver.ThreadingTCPServer(('127.0.0.1', port), StreamHandler) as httpd:
        print(f'Serving {ts_path} ({len(StreamHandler.ts_data)} bytes) on http://127.0.0.1:{port}/test.ts', flush=True)
        httpd.serve_forever()


if __name__ == '__main__':
    main()
