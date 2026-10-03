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
| `GET /api/channels` (チャンネル一覧、現在/次の番組・IPTV 疑似チャンネル含む) | ✅ Go 実装済み (視聴者数のみ常に 0) |
| `GET /api/programs/timetable` (番組表) | ✅ Go 実装済み (EDCB バックエンドの予約情報は未対応) |
| `POST /api/programs/search` (番組検索) | ✅ Go 実装済み (EDCB 以外は 422 を返し、EDCB 時は Python 版へプロキシ) |
| `GET /api/series`・`GET /api/series/search`・`GET /api/series/{series_id}` (シリーズ番組) | ✅ Go 実装済み (録画番組・録画ファイル・チャンネルまで展開) |
| `GET /api/iptv/channels`・`/countries`・`/groups`・`/playlist.m3u` (IPTV チャンネル一覧・国・グループ・プレイリスト出力) | ✅ Go 実装済み (実データ 11152 チャンネルで Python 版と完全一致を検証済み) |
| `GET`/`POST`/`DELETE /api/iptv/sources` (プレイリストソース管理) | ✅ Go 実装済み |
| `GET`/`POST`/`DELETE /api/iptv/tvui` (テレビ視聴 UI 登録) | ✅ Go 実装済み (呼び出し元ごとに分離) |
| `GET /api/iptv/proxy` (IPTV ストリームプロキシ、HLS プレイリストの書き換え含む) | ✅ Go 実装済み |
| `GET /api/iptv/logo` (IPTV チャンネルロゴプロキシ) | ✅ Go 実装済み (取得できない場合は既定のロゴ) |
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
- `internal/iptv/`: IPTV (M3U プレイリスト) の取り込みと配信 (`server/app/utils/IPTVUtil.py` の Go 版) 。
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
- `internal/tsinfo/testdata/subchannel_parent_ids.json`: Python 版 `TSInformation.calculateSubchannelParentServiceID()` の全サービス ID 分の期待値。
- `internal/api/testdata/timetable_sort.json`: Python 版 `GetTimeTableChannelSortKey()` によるチャンネル並び替え結果の期待値。
- `internal/iptv/testdata/iptv_parity.json`: Python 版 `IPTVUtil` の M3U 解析結果 (M3U のパース・相対 URL の解決・HLS プレイリストの
  書き換え・画質の抽出・ストリーム形式の判定・プレイリスト出力・国/グループ集計) の期待値。

フィクスチャは `server-go/tools/` のスクリプトで再生成できます (`server/` ディレクトリで実行) 。

```powershell
cd server
uv run python ../server-go/tools/generate_iptv_parity_fixture.py
```

チャンネル一覧 API は、実スキーマの DB と実際の HTTP サーバーを使った E2E 検証もできます。

```powershell
python server-go/tools/e2e_channels_setup.py <一時コピーした server/data/database.sqlite>
./konomitv-go.exe -no-proxy -listen 127.0.0.77:7005 -server-dir <server ディレクトリ>
python server-go/tools/e2e_channels_verify.py <同じ database.sqlite> http://127.0.0.77:7005
```

実データ (iptv-org のプレイリスト約 1.1 万チャンネル) を使った完全一致の検証もできます。

```powershell
cd server
uv run python ../server-go/tools/generate_iptv_real_fixture.py   # server-go/tmp_real_iptv/ に生成
cd ../server-go
$env:KONOMITV_IPTV_REAL_FIXTURE = 'tmp_real_iptv'
go test ./internal/iptv/ -run TestRealPlaylistParity -v
```

```powershell
go test ./...
go vet ./...
```

実際のデータベースや設定ファイルが存在する環境では、それらを使った互換性テストも実行されます
(存在しない場合は自動的にスキップされます) 。

## 今後の予定

1. ストリーミング (LiveStream / VideoStream) とエンコーダー制御の Go 化 (視聴者数もここで実装する)
2. 書き込み系 API (予約・設定など) の移行
3. 完全移行後に `-listen 127.0.0.77:7010` で Python 版を置き換え
