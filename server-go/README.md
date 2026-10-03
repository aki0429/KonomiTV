# server-go: KonomiTV サーバー Go 版

KonomiTV のバックエンド (Python / FastAPI) を段階的に Go へ移行するためのサーバー実装です。
未実装の API は既存の Python 版サーバーへリバースプロキシするため、Python 版と共存させたまま
ルーター単位で少しずつ移行できます (ストラングラーパターン) 。

## 現在の移行状況

| API | 状態 |
| --- | --- |
| `GET /api/version` | ✅ Go 実装済み |
| `POST /api/users/token` (アクセストークン発行) | ✅ Go 実装済み |
| `GET /api/users`・`GET /api/users/me`・`GET /api/users/{username}` | ✅ Go 実装済み |
| `GET /api/users/me/icon`・`GET /api/users/{username}/icon` | ✅ Go 実装済み |
| `GET /api/data-broadcasting/request/{url}`・`POST /api/data-broadcasting/request/{url}` (web-bml プロキシ) | ✅ Go 実装済み |
| `GET /api/data-broadcasting/internet-status` | ✅ Go 実装済み |
| `POST /api/users` (登録) ・`PUT`/`DELETE /api/users/me`・`PUT`/`DELETE /api/users/{username}` (更新・削除) | ✅ Go 実装済み |
| `PUT /api/users/me/icon` (アイコン更新、512x512 PNG へ変換) | ✅ Go 実装済み |
| `POST`/`DELETE /api/users/me/account-links` (Twitter / Bluesky 紐付け) | ✅ Go 実装済み |
| `GET /api/channels/{channel_id}` (チャンネル情報、現在/次の番組を含む) | ✅ Go 実装済み (視聴者数のみ常に 0) |
| `GET /api/channels/{channel_id}/logo` (チャンネルロゴ) | ✅ Go 実装済み (IPTV 疑似チャンネルは Python 版へプロキシ) |
| `GET /api/channels/{channel_id}/jikkyo` (ニコニコ実況 WebSocket URL) | ✅ Go 実装済み (ニコニコアカウント連携時のニコ生セッション取得・トークン更新含む) |
| `GET /api/channels` (チャンネル一覧) | 🔁 Python 版へプロキシ (視聴者数と IPTV 疑似チャンネルが Python プロセスのメモリに依存) |
| `/assets/*`・`/` (client/dist の静的配信、SPA フォールバック) | ✅ Go 実装済み |
| CORS (Starlette 互換) | ✅ Go 実装済み |
| その他の全 API | 🔁 Python 版へプロキシ |

## 使い方

Python 版サーバー (`server/` で `uv run task serve` / `uv run task dev`) を起動した状態で、
`server-go/` ディレクトリから実行します。

```powershell
# 開発中の起動 (Python 版の 127.0.0.77:7010 へプロキシ)
go run ./cmd/konomitv-go

# ビルドして実行
go build -o konomitv-go.exe ./cmd/konomitv-go
./konomitv-go.exe
```

デフォルトでは `127.0.0.77:7002` でリッスンします。Python 版 (Akebi 経由で 7000) とは別ポートなので
共存できます。動作確認は HTTP で直接行ってください (API のみなら HTTPS は不要です) 。

```powershell
curl http://127.0.0.77:7002/api/version
```

### 主なフラグ

| フラグ | デフォルト | 説明 |
| --- | --- | --- |
| `-listen` | `127.0.0.77:7002` | Go 版サーバーのリッスンアドレス |
| `-python-backend` | `http://127.0.0.77:<server.port+10>/` | 未移行 API の転送先 |
| `-no-proxy` | `false` | プロキシを無効化し、未実装 API は 404 を返す |
| `-server-dir` | 自動検出 | Python 版サーバーのディレクトリ (`<repo>/server`) |
| `-debug` | `false` | デバッグログを出力 (config.yaml の `general.debug` より優先) |
| `-version` | | バージョンを表示して終了 |

## アーキテクチャ

- `cmd/konomitv-go/`: エントリーポイント。設定・DB の初期化と HTTP サーバーの起動。
- `internal/config/`: `config.yaml` の読み込み (`server/app/config.py` の Go 版、必要な項目のみ) 。
- `internal/constants/`: バージョン・パス・タイムゾーンなどの定数 (`server/app/constants.py` 相当) 。
- `internal/database/`: SQLite へのアクセス (`modernc.org/sqlite`、CGO 不要) 。読み取りは読み取り専用接続、書き込みは専用接続で行う。
- `internal/tsinfo/`: 放送波 (MPEG-TS) のユーティリティ (`server/app/utils/TSInformation.py` 相当、地域識別の逆引きなど) 。
- `internal/jikkyo/`: ニコニコ実況のチャンネル対応表 (`server/app/utils/JikkyoClient.py` の一部) 。`server/static/jikkyo_channels.json` を読み込む。
- `internal/api/`: HTTP ハンドラー。Go 実装済みルートと、Python 版へのプロキシ・静的配信。

### 互換性のための約束事

- SQLite は Python 版 (Tortoise ORM) が管理する `server/data/database.sqlite` をそのまま使う。
  日時は `datetime.isoformat(" ")` 形式 (例: `2025-09-22 15:47:00.123456+09:00`) で保存されている。
- 読み取りは読み取り専用接続 (`mode=ro` + `query_only`) 、書き込みは専用接続 (`busy_timeout` + `foreign_keys`、接続数 1 で直列化) で行う。スキーマは変更せず、マイグレーションは Python 版の Aerich に任せる。
- JWT アクセストークンは Python 版 (`python-jose`, HS256) と完全に互換。`server/data/jwt_secret.dat` を共有するため、どちらのサーバーが発行したトークンでも認証できる。
- パスワードのハッシュ化・検証は bcrypt (passlib 互換、コスト12) で行う。
- 浮動小数点数は Pydantic v2 と同じ形式 (`1800.0`) で出力する。
- エラーレスポンスは FastAPI 互換の `{"detail": "..."}` 形式で返す。
- CORS は Starlette の CORSMiddleware と同じヘッダーを返す。
- 日時の JSON 表現は Pydantic v2 と同じ (`2026-09-22T15:11:58.874290+09:00`) 。

## テスト

`go test ./...` で実行する。Python 版との互換性は、Python 側で生成したフィクスチャとの照合で検証している。
- `internal/tsinfo/testdata/regions.json`: Python 版 `TSInformation.getRegionNamesFromNetworkID()` の全ネットワーク ID 分の期待値。
- `internal/jikkyo/testdata/jikkyo_resolution.json`: Python 版 `JikkyoClient` の実況チャンネル解決結果 1267 件分の期待値。

```powershell
go test ./...
go vet ./...
```

実際のデータベースや設定ファイルが存在する環境では、それらを使った互換性テストも実行されます
(存在しない場合は自動的にスキップされます) 。

## 今後の予定

1. IPTV ルーター (`IPTVRouter` / `IPTVUtil`) の移行 (チャンネル一覧の視聴者数・IPTV 疑似チャンネルも含む)
2. 読み取り系 API (`Programs` / `Series`) の移行
3. 書き込み系 API (予約・設定など) の移行
4. ストリーミング (LiveStream / VideoStream) とエンコーダー制御の Go 化
5. 完全移行後に `-listen 127.0.0.77:7010` で Python 版を置き換え
