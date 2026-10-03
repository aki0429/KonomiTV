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

フィクスチャは `server-go/tools/` のスクリプトで再生成できます (`server/` ディレクトリで実行) 。

```powershell
cd server
uv run python ../server-go/tools/generate_iptv_parity_fixture.py
uv run python ../server-go/tools/generate_stream_options_fixture.py
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

1. ビデオストリーミング (`/api/streams/video`、録画番組の再生) と録画番組 API (`/api/videos`) の Go 化
2. EDCB バックエンドのチューナー制御 (`EDCBTuner`) の Go 化
3. 書き込み系 API (予約・設定など) の移行
4. 完全移行後に `-listen 127.0.0.77:7010` で Python 版を置き換え
