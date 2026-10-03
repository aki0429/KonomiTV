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
| `GET /api/channels/{channel_id}` (チャンネル情報、現在/次の番組を含む) | ✅ Go 実装済み |
| `GET /api/channels/{channel_id}/logo` (チャンネルロゴ) | ✅ Go 実装済み (IPTV 疑似チャンネルは Python 版へプロキシ) |
| `GET /api/channels/{channel_id}/jikkyo` (ニコニコ実況 WebSocket URL) | ✅ Go 実装済み (ニコニコアカウント連携時のニコ生セッション取得・トークン更新含む) |
| `GET /api/channels` (チャンネル一覧、現在/次の番組・IPTV 疑似チャンネル含む) | ✅ Go 実装済み |
| `GET /api/programs/timetable` (番組表) | ✅ Go 実装済み (EDCB バックエンドの予約情報は未対応) |
| `POST /api/programs/search` (番組検索) | ✅ Go 実装済み (EDCB 以外は 422 を返し、EDCB 時は Python 版へプロキシ) |
| `GET /api/series`・`GET /api/series/search`・`GET /api/series/{series_id}` (シリーズ番組) | ✅ Go 実装済み (録画番組・録画ファイル・チャンネルまで展開) |
| `GET /api/iptv/channels`・`/countries`・`/groups`・`/playlist.m3u` (IPTV チャンネル一覧・国・グループ・プレイリスト出力) | ✅ Go 実装済み (実データ 11152 チャンネルで Python 版と完全一致を検証済み) |
| `GET`/`POST`/`DELETE /api/iptv/sources` (プレイリストソース管理) | ✅ Go 実装済み |
| `GET`/`POST`/`DELETE /api/iptv/tvui` (テレビ視聴 UI 登録) | ✅ Go 実装済み (呼び出し元ごとに分離) |
| `GET /api/iptv/proxy` (IPTV ストリームプロキシ、HLS プレイリストの書き換え含む) | ✅ Go 実装済み |
| `GET /api/iptv/logo` (IPTV チャンネルロゴプロキシ) | ✅ Go 実装済み (取得できない場合は既定のロゴ) |
| `GET /api/streams/live` (ライブストリーム一覧) | ✅ Go 実装済み |
| `GET /api/streams/live/{display_channel_id}/{quality}` (ライブストリームの状態) | ✅ Go 実装済み |
| `GET /api/streams/live/{display_channel_id}/{quality}/events` (状態の Server-Sent Events) | ✅ Go 実装済み |
| `GET /api/streams/live/{display_channel_id}/{quality}/mpegts` (ライブ MPEG-TS ストリーム) | ✅ Go 実装済み |
| `GET /api/streams/live/{display_channel_id}/{quality}/psi-archived-data` (PSI/SI アーカイブデータ) | ✅ Go 実装済み (EDCB / Mirakurun バックエンドのみ。IPTV では 500 を返す) |
| `GET /api/videos`・`GET /api/videos/search` (録画番組一覧・検索) | ✅ Go 実装済み (応答は Python 版と完全一致を検証済み) |
| `GET /api/videos/{video_id}` (録画番組情報) | ✅ Go 実装済み |
| `GET /api/videos/{video_id}/thumbnail`・`/thumbnail/tiled` (サムネイル画像、ETag / 304 対応) | ✅ Go 実装済み |
| `GET /api/videos/{video_id}/download` (録画ファイルのダウンロード、Range 対応) | ✅ Go 実装済み |
| `GET /api/videos/{video_id}/jikkyo` (ニコニコ実況の過去ログコメント) | ✅ Go 実装済み |
| `DELETE /api/videos/{video_id}` (録画ファイルの削除、管理者のみ) | ✅ Go 実装済み |
| `POST /api/videos/{video_id}/reanalyze`・`/thumbnail/regenerate` | 🔁 Python 版へプロキシ |
| `GET /api/settings/client`・`PUT /api/settings/client` (クライアント設定の取得・更新) | ✅ Go 実装済み |
| `GET /api/settings/server`・`PUT /api/settings/server` (サーバー設定の取得・更新) | ✅ Go 実装済み (PUT は config.yaml をコメントを保持したまま行単位で書き換える) |
| `GET /api/maintenance/logs/{log_type}` (サーバーログ・アクセスログの SSE 配信) | ✅ Go 実装済み |
| `POST /api/maintenance/restart`・`POST /api/maintenance/shutdown` (再起動・終了) | ✅ Go 実装済み (ローカルホストからのアクセスは認証不要) |
| `POST /api/maintenance/update-database` (データベース更新) | ✅ Go 実装済み (IPTV バックエンドはプレイリストの再取得、EDCB / Mirakurun は Python 版へプロキシ) |
| `POST /api/maintenance/run-batch-scan`・`/run-background-analysis` | 🔁 Python 版へプロキシ |
| `GET /api/captures`・`GET /api/captures/{filename}`・`POST /api/captures`・`DELETE /api/captures/{filename}` (キャプチャ一覧・取得・アップロード・削除) | ✅ Go 実装済み |
| `GET`/`POST`/`PUT`/`DELETE /api/captures/folders` (キャプチャフォルダ管理) | ✅ Go 実装済み |
| `GET`/`POST`/`DELETE /api/captures/folders/{folder_id}/captures` (フォルダ内キャプチャ操作) | ✅ Go 実装済み |
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
- `internal/logging/`: Python 版 (uvicorn) 互換のログ出力と日次ローテーション (`server/app/utils/LogRotation.py` の Go 版) 。
- `internal/exif/`: EXIF の最小限のパーサー (キャプチャ画像の XPComment と回転情報の読み取り) 。
- `internal/captures/`: キャプチャ画像のスキャン・メタデータ抽出・サムネイル生成 (`server/app/routers/CapturesRouter.py` の一部) 。
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

### ライブストリーミング

`internal/stream/` が Python 版 `server/app/streams/` 相当のエンコード処理を担当する。

- 画質 (16 種類 + `original`) と追加エンコードオプション (`-10bit` / `-24fps`) の解釈、および
  FFmpeg / QSVEncC / NVEncC / VCEEncC / rkmppenc のエンコード引数の組み立て
  (Python 版と引数レベルで一致することをフィクスチャで検証している) 。
- `LiveStream` / `LiveStreamClient`: チャンネル+画質ごとのストリーム管理、クライアントの接続・切断、
  視聴者数、`Offline` / `Standby` / `ONAir` / `Idling` / `Restart` の状態遷移、アイドリングによる自動停止。
- `liveEncodingTask`: IPTV のストリーム (`BuildTSConversionArguments` で MPEG-2 TS に変換) または
  Mirakurun / mirakc の Service Stream API から放送波を受信し、tsreadex → エンコーダー → MPEG-TS 出力を
  クライアントへ分配する。エンコーダーの進捗ログからの状態遷移、フリーズ検出による再起動 (最大 10 回) 、
  停波・チューナー不足などのエラー判定も Python 版と同じメッセージを返す。
- `LivePSIDataArchiver`: psisiarc を起動してデータ放送用の PSI/SI アーカイブデータを配信する
  (IPTV の疑似チャンネルでは起動しない) 。

**未対応**: EDCB バックエンドのチューナー制御 (`EDCBTuner`) 。EDCB バックエンドで放送波を受信する場合、
Go 版では `EDCB バックエンドは Go 版サーバーでは未対応です。(E-02E)` を返して Offline になる
(IPTV バックエンドと Mirakurun / mirakc バックエンドは Go 版で動作する) 。

### サーバー設定 API

`GET /api/settings/server` は `config.yaml` を Python 版 `ServerSettings` のデフォルト値で補完して返す
(応答は Python 版と完全に一致することを検証済み) 。
`PUT /api/settings/server` は Pydantic 相当のバリデーション (列挙値・数値の範囲・URL のスキーム) を行ったうえで、
`config.yaml` を**コメントを保持したまま行単位で書き換える** (ruamel.yaml の代替) 。

**未対応**: Python 版のカスタムバリデーターのうち、環境に依存する検証 (EDCB / Mirakurun への接続確認、
ポートの使用状況、エンコーダーの対応状況) は Go 版では行わない。

### 録画番組 API (`/api/videos`)

録画番組の一覧・検索・詳細・サムネイル画像・録画ファイルのダウンロード・削除を Go で実装している
(応答は Python 版 `schemas.RecordedProgram` と完全に一致することを検証済み) 。

- 一覧と検索は Python 版と同じ生 SQL (recorded_programs + recorded_videos + channels の結合) を発行する。
  `order=ids` の場合は指定された ID の順序を維持する (ページングも Python 版と同じ挙動) 。
- サムネイル画像は `server/data/thumbnails/<file_hash>.webp` を配信し、存在しない場合は
  `server/static/thumbnails/default.webp` をキャッシュ無効で返す。
  `ETag` / `Last-Modified` による 304 応答 (If-None-Match / If-Modified-Since) にも対応する。
- 録画ファイルのダウンロードは `http.ServeFile` で配信するため Range リクエスト (シーク) に対応する。
- 削除は管理者のみ実行でき、データベースのレコード・サムネイル画像・補助ファイル (.ts.program.txt / .ts.err) ・
  録画ファイル本体を Python 版と同じ順序で削除する。
- ニコニコ実況の過去ログコメントは過去ログ API から取得して DPlayer の形式に変換する
  (コメントの色・位置・サイズの解釈は Python 版と一致することをフィクスチャで検証済み) 。

**未対応**: メタデータ再解析 (`POST /api/videos/{video_id}/reanalyze`) とサムネイル画像再生成
(`POST /api/videos/{video_id}/thumbnail/regenerate`) は Go 版では未実装のため Python 版へプロキシする
(FFmpeg / psisiarc を使った録画ファイルの解析処理が必要なため) 。
また、MPEG-4 コンテナの録画番組で `.psc` ファイルがある場合のコメント時刻の補正は行わない。

### メンテナンス API (`/api/maintenance`)

- `GET /api/maintenance/logs/{log_type}` はサーバーログ (`server`) またはアクセスログ (`access`) を
  Server-Sent Events で配信する。初回接続時に `initial_log_update` で全行を送信し、以降は
  `log_update` で新しい行を 1 行ずつ送信する (sse_starlette と同じく 15 秒間隔で ping を送信) 。
- ログは Python 版 (uvicorn) と同じ形式 (`[2026/09/22 12:53:30.229] INFO:     message`) で
  `server/logs/KonomiTV-Server.log` と `server/logs/KonomiTV-Access.log` に出力する。
  JST 基準で日次ローテーションし、過去のログは `server/logs/archives/` に移動する (保持期間は 30 日) 。
  なお、Go 版のログには構造化ログの属性が `key=value` 形式で付加される (Python 版にはない追加情報) 。
- `POST /api/maintenance/restart` は `server/data/restart_required.lock` を作成してからサーバーを終了する
  (KonomiTV.py などのスーパーバイザーがこのファイルを確認して再起動する) 。
  `POST /api/maintenance/shutdown` はロックファイルを作成せずにサーバーを終了する。
  どちらも管理者ユーザーまたは `Host: 127.0.0.77:<server.port + 10>` (内部ポート) からのアクセスで実行できる。
- `POST /api/maintenance/update-database` は IPTV バックエンドではプレイリストを再取得するだけで、
  Python 版と同じくチャンネル情報・番組情報をデータベースに保存しない。
  EDCB / Mirakurun バックエンドのチャンネル情報・番組情報の更新は Go 版では未実装のため Python 版へプロキシする。

**未対応**: 録画フォルダの一括スキャン (`POST /api/maintenance/run-batch-scan`) と
バックグラウンド解析 (`POST /api/maintenance/run-background-analysis`) は、
録画ファイルのメタデータ解析・CM 区間検出・サムネイル生成が必要なため Python 版へプロキシする。

### キャプチャ API (`/api/captures`)

サーバー設定 `capture.upload_folders` で指定されたフォルダ内の JPEG / PNG を扱う。

- 一覧は保存先フォルダをスキャンし、ファイルの更新日時順 (order=desc/asc) にソートして
  ページネーション (1 ページ 36 件) を適用する。
- キャプチャメタデータは EXIF の XPComment タグ (0x9C9C) に格納された UTF-16LE の JSON を読み取る
  (`internal/exif` に自前の最小限の EXIF パーサーを実装) 。
  応答は Python 版 `schemas.Capture` と完全に一致することを検証済み。
- 検索 (`search`) はファイル名・EXIF の番組名・チャンネル名 (channels テーブルの network_id / service_id と突き合わせ) の
  いずれかで部分一致する。
- `thumbnail=true` を指定すると、EXIF の回転情報を適用したうえで長辺 400px に縮小した JPEG (品質 80) を返す。
  縮小後のサイズは Pillow の `Image.thumbnail()` と同じ計算で求め、Pillow と一致することを検証済み。
- アップロードは magic bytes で JPEG / PNG かを判定し、同名ファイルが存在する場合は連番を付与して保存する。
  空き容量が 10MB 未満のフォルダはスキップして次の保存先フォルダを探す。
- キャプチャフォルダ (capture_folders / capture_bookmarks テーブル) はログインユーザーごとに管理し、
  他のユーザーのフォルダにはアクセスできない (404) 。

**未対応**: 特になし (Python 版と同じ挙動を実装済み) 。

## テスト

`go test ./...` で実行する。Python 版との互換性は、Python 側で生成したフィクスチャとの照合で検証している。
- `internal/tsinfo/testdata/regions.json`: Python 版 `TSInformation.getRegionNamesFromNetworkID()` の全ネットワーク ID 分の期待値。
- `internal/jikkyo/testdata/jikkyo_resolution.json`: Python 版 `JikkyoClient` の実況チャンネル解決結果 1267 件分の期待値。
- `internal/tsinfo/testdata/subchannel_parent_ids.json`: Python 版 `TSInformation.calculateSubchannelParentServiceID()` の全サービス ID 分の期待値。
- `internal/api/testdata/timetable_sort.json`: Python 版 `GetTimeTableChannelSortKey()` によるチャンネル並び替え結果の期待値。
- `internal/iptv/testdata/iptv_parity.json`: Python 版 `IPTVUtil` の M3U 解析結果 (M3U のパース・相対 URL の解決・HLS プレイリストの
  書き換え・画質の抽出・ストリーム形式の判定・プレイリスト出力・国/グループ集計) の期待値。
- `internal/stream/testdata/stream_options.json`: Python 版 `LiveEncodingTask` の FFmpeg / HWEncC の
  エンコード引数 (画質 16 種類 × チャンネル種別 × フル HD × リトライ回数 × エンコードオプション) の期待値。
- `internal/jikkyo/testdata/jikkyo_comments.json`: Python 版 `JikkyoClient` のコメント整形処理
  (色・位置・サイズの解釈、運営コマンドの判定、過去ログ API のレスポンスの変換) の期待値。
- `internal/config/server_settings_defaults.json`: Python 版 `ServerSettings` のデフォルト値と、
  リポジトリの `config.yaml` を Python 版 `LoadConfig()` で読み込んだ結果の期待値
  (`GET /api/settings/server` はこのデフォルト値で `config.yaml` を補完して返す) 。
- `internal/captures/testdata/capture_exif.jpg`・`capture_no_exif.png`・`expected.json`:
  Pillow で生成した EXIF XPComment 付き JPEG と EXIF なし PNG、およびそのメタデータの期待値
  (キャプチャ API のメタデータ抽出・サムネイル生成の検証に使う) 。

フィクスチャは `server-go/tools/` のスクリプトで再生成できます (`server/` ディレクトリで実行) 。

```powershell
cd server
uv run python ../server-go/tools/generate_iptv_parity_fixture.py
uv run python ../server-go/tools/generate_stream_options_fixture.py
uv run python ../server-go/tools/generate_server_settings_fixture.py
uv run python ../server-go/tools/generate_capture_fixture.py
```

### E2E 検証

実サーバーを一時ディレクトリで起動して検証するスクリプトも `server-go/tools/` にあります
(`server/` ディレクトリで実行) 。

```powershell
# メンテナンス API (再起動 API を実行するとサーバーが終了するため、最後に実行する)
uv run python ../server-go/tools/e2e_maintenance_verify.py http://127.0.0.77:7008 <作業ディレクトリ> 1 2

# キャプチャ API (事前に e2e_captures_setup.py で環境を構築する)
uv run python ../server-go/tools/e2e_captures_setup.py <作業ディレクトリ>
uv run python ../server-go/tools/e2e_captures_verify.py http://127.0.0.77:7009 <作業ディレクトリ> 1
```

チャンネル一覧 API は、実スキーマの DB と実際の HTTP サーバーを使った E2E 検証もできます。

```powershell
python server-go/tools/e2e_channels_setup.py <一時コピーした server/data/database.sqlite>
./konomitv-go.exe -no-proxy -listen 127.0.0.77:7005 -server-dir <server ディレクトリ>
python server-go/tools/e2e_channels_verify.py <同じ database.sqlite> http://127.0.0.77:7005
```

ライブストリーミングは、実際の FFmpeg / tsreadex を使ったエンコードパイプラインを含めて E2E 検証できます。
ローカルの MPEG-2 TS を約 1x 速で配信する簡易サーバーを IPTV の疑似チャンネルとして取り込み、
実際にストリームを受信して状態遷移・視聴者数・Server-Sent Events・アイドリングによる自動停止を確認します。

```powershell
python server-go/tools/e2e_stream_setup.py <作業ディレクトリ>
python server-go/tools/e2e_stream_server.py <作業ディレクトリ>/test.ts 7099   # 別のターミナルで実行
go build -o konomitv-go.exe ./cmd/konomitv-go
./konomitv-go.exe -no-proxy -listen 127.0.0.77:7006 -server-dir <作業ディレクトリ>/server   # 別のターミナルで実行
python server-go/tools/e2e_stream_verify.py http://127.0.0.77:7006
```

サーバー設定 API は、実際の `config.yaml` を使った E2E 検証もできます
(`PUT` は一時ディレクトリの `config.yaml` に対してのみ実行され、リポジトリの `config.yaml` は変更されません) 。

```powershell
cp config.yaml <作業ディレクトリ>/config.yaml
./konomitv-go.exe -no-proxy -listen 127.0.0.77:7007 -server-dir <作業ディレクトリ>/server
cd server
uv run python ../server-go/tools/e2e_settings_verify.py http://127.0.0.77:7007 <作業ディレクトリ>/config.yaml <管理者ユーザーの ID>
```

録画番組 API は、実スキーマの DB と実際の録画ファイルを使った E2E 検証もできます
(検証スクリプトは録画番組を 1 件削除するため、実行前に毎回 setup を実行し直してください) 。

```powershell
cd server
uv run python ../server-go/tools/e2e_videos_setup.py <作業ディレクトリ>
cd ../server-go
./konomitv-go.exe -no-proxy -listen 127.0.0.77:7008 -server-dir <作業ディレクトリ>/server
cd ../server
uv run python ../server-go/tools/e2e_videos_verify.py http://127.0.0.77:7008 <作業ディレクトリ> <管理者ユーザーの ID>
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

1. ビデオストリーミング (`/api/streams/video`、録画番組の再生) の Go 化
2. EDCB バックエンドのチューナー制御 (`EDCBTuner`) の Go 化
3. 残りの書き込み系 API (予約・Twitter / Bluesky / ニコニコ連携・キャプチャなど) の移行
4. 完全移行後に `-listen 127.0.0.77:7010` で Python 版を置き換え
