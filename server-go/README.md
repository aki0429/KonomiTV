# server-go: KonomiTV サーバー Go 版

KonomiTV のバックエンドを Go で実装したサーバーです。
Go単独運転では`-no-proxy`を指定します。実行時のPython、FastAPI、Zendriver、OpenCVは不要です。
既存のPython版と並行して比較するためのプロキシモードも保持しています。

## 現在の移行状況

現行本家との差分と実機で確認した範囲は [MIGRATION.md](MIGRATION.md) に記録しています。
Go 単独運転 (`-no-proxy`) では、Python を起動せずに DB を作成できます。
起動前に設定の型・保存先・バックエンド接続・同梱エンコーダー・ポートを検査し、失敗時は DB を開きません。
Python と併走する移行モードでは、型と保存先を検証し、使用中の Python 側ポートなどの実機検査だけを省略します。
設定 API の更新はファイルへ保存され、再起動後に反映します。録画フォルダのネットワーク巡回間隔は分単位で指定できます。
既存 DB には本家と同名のテーブル・追加列・索引のみを補い、既存の録画・ユーザーや独自テーブルを保持します。
本家の全列が確認できない DB では追加をロールバックし、起動を停止します。更新前には DB をバックアップしてください。

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
| `/api/twitter/*` (Cookie認証・タイムライン・検索・投稿・画像・返信・いいね等) | Go実装。Chrome / Braveを直接操作する。実ブラウザーとローカル模擬ページで検証、実アカウントへの投稿は未実施 |
| `GET /api/channels/{channel_id}` (チャンネル情報、現在/次の番組を含む) | ✅ Go 実装済み |
| `GET /api/channels/{channel_id}/logo` (チャンネルロゴ) | ✅ Go 実装済み (IPTV の配信元ロゴも Go で取得) |
| `GET /api/channels/{channel_id}/jikkyo` (ニコニコ実況 WebSocket URL) | ✅ Go 実装済み (ニコニコアカウント連携時のニコ生セッション取得・トークン更新含む) |
| `GET /api/channels` (チャンネル一覧、現在/次の番組・IPTV 疑似チャンネル含む) | ✅ Go 実装済み |
| `GET /api/programs/timetable` (番組表) | ✅ Go 実装済み (EDCB の録画中・無効・競合状態、EID 不一致時の時間照合、EDCB/Mirakurun の詳細文の継続を含む) |
| `GET /api/programs/toru-counts`・`GET /api/programs/toru-ranking` | Go 実装済み (本家 TorneClient の合成実行結果と照合。初回 EPG 更新後と 3 時間ごとに取得) |
| `/api/captures/tags`・`/api/captures/tags/{tag_id}`・`.../items`・`/api/captures/favorites` | Go 実装済み (本人のタグ CRUD、タグ付け、お気に入り。一括追加は全 ID 検証後に変更) |
| `POST /api/programs/search` (番組検索) | Go 実装 (`-no-proxy` 時は EDCB SearchPg 1025、EDCB 以外は本家同様422。プロキシモードも保持) |
| `/api/recording/reservations`・`/api/recording/conditions`・`/api/recording/presets` | Go 実装 (EDCBの予約・自動予約条件・録画設定) |
| `GET /api/series`・`GET /api/series/search`・`GET /api/series/{series_id}` (シリーズ番組) | ✅ Go 実装済み (録画番組・録画ファイル・チャンネルまで展開) |
| `GET /api/iptv/channels`・`/countries`・`/groups`・`/playlist.m3u` (IPTV チャンネル一覧・国・グループ・プレイリスト出力) | ✅ Go 実装済み (実データ 11152 チャンネルで Python 版と完全一致を検証済み) |
| `GET`/`POST`/`DELETE /api/iptv/sources`・`POST /api/iptv/sources/enabled` (プレイリストソース管理・有効/無効切替) | ✅ Go 実装済み |
| `GET`/`POST`/`DELETE /api/iptv/tvui` (テレビ視聴 UI 登録) | ✅ Go 実装済み (呼び出し元ごとに分離) |
| `GET /api/iptv/proxy` (IPTV ストリームプロキシ、HLS プレイリストの書き換え含む) | ✅ Go 実装済み |
| `GET /api/iptv/logo` (IPTV チャンネルロゴプロキシ) | ✅ Go 実装済み (取得できない場合は既定のロゴ) |
| `GET /api/streams/live` (ライブストリーム一覧) | ✅ Go 実装済み |
| `GET /api/streams/live/{display_channel_id}/{quality}` (ライブストリームの状態) | ✅ Go 実装済み |
| `GET /api/streams/live/{display_channel_id}/{quality}/events` (状態の Server-Sent Events) | ✅ Go 実装済み |
| `GET /api/streams/live/{display_channel_id}/{quality}/mpegts` (ライブ MPEG-TS ストリーム) | ✅ Go 実装済み |
| `GET /api/streams/live/{display_channel_id}/{quality}/psi-archived-data` (PSI/SI アーカイブデータ) | ✅ Go 実装済み (EDCB / Mirakurun バックエンドのみ。IPTV では 500 を返す) |
| `GET /api/streams/video/{video_id}/{quality}/playlist`・`PUT .../keep-alive`・`GET .../buffer` (録画 HLS プレイリスト・セッション維持・バッファ範囲 SSE) | ✅ Go 実装済み (セッション管理は `internal/videostream/session*.go`) |
| `GET /api/streams/video/{video_id}/{quality}/segment` (録画 HLS セグメント) | Go 実装。サーバー起動時に同梱FFmpeg/QSVEncC等のエンコーダーを登録する |
| `GET /api/streams/video/{video_id}/{quality}/offline-stream` (オフライン保存) | ✅ Go 実装済み (`KTVODLP` 本体をネイティブ生成。proxy 有効時は検証後に Python 版へ転送) |
| `GET /api/videos`・`GET /api/videos/search` (録画番組一覧・検索) | ✅ Go 実装済み (応答は Python 版と完全一致を検証済み) |
| `GET /api/videos/{video_id}` (録画番組情報) | ✅ Go 実装済み |
| `GET /api/videos/{video_id}/thumbnail`・`/thumbnail/tiled` (サムネイル画像、ETag / 304 対応) | ✅ Go 実装済み |
| `PUT /api/videos/{video_id}/thumbnail` (同じ放送回のキャプチャで代表画像を変更) | ✅ Go 実装済み |
| `GET /api/videos/{video_id}/download` (録画ファイルのダウンロード、Range 対応) | ✅ Go 実装済み |
| `GET /api/videos/{video_id}/jikkyo` (ニコニコ実況の過去ログコメント) | ✅ Go 実装済み |
| `DELETE /api/videos/{video_id}` (録画ファイルの削除、管理者のみ) | ✅ Go 実装済み |
| `POST /api/videos/{video_id}/reanalyze`・`/thumbnail/regenerate` | Go 実装 (番組情報・シーク位置・画像採点と生成) |
| `GET /api/settings/client`・`PUT /api/settings/client` (クライアント設定の取得・更新) | ✅ Go 実装済み (51 項目の型・選択肢・入れ子の検証と同期時刻の照合を含む) |
| `GET /api/settings/server`・`PUT /api/settings/server` (サーバー設定の取得・更新) | ✅ Go 実装済み (PUT は config.yaml をコメントを保持したまま行単位で書き換える) |
| `GET /api/maintenance/logs/{log_type}` (サーバーログ・アクセスログの SSE 配信) | ✅ Go 実装済み |
| `POST /api/maintenance/restart`・`POST /api/maintenance/shutdown` (再起動・終了) | ✅ Go 実装済み (ローカルホストからのアクセスは認証不要) |
| `POST /api/maintenance/update-database` (データベース更新) | Go 実装 (EDCB / Mirakurun / IPTV。プロキシモードも保持) |
| `POST /api/maintenance/run-batch-scan`・`/run-background-analysis` | Go 実装 (録画一覧・背景解析と状態API) |
| `/api/maintenance/capture-scan/*`・`/database-vacuum/*` | Go 実装 (キャプチャ走査・DB最適化と状態API) |
| `GET /api/captures`・`GET /api/captures/{filename}`・`POST /api/captures`・`DELETE /api/captures/{filename}` (キャプチャ一覧・取得・アップロード・削除) | ✅ Go 実装済み |
| `GET`/`POST`/`PUT`/`DELETE /api/captures/folders` (キャプチャフォルダ管理) | ✅ Go 実装済み |
| `GET`/`POST`/`DELETE /api/captures/folders/{folder_id}/captures` (フォルダ内キャプチャ操作) | ✅ Go 実装済み |
| `/assets/*`・`/` (client/dist の静的配信、SPA フォールバック) | ✅ Go 実装済み |
| CORS (Starlette 互換) | ✅ Go 実装済み |
| `/api/bluesky/*`・`/api/niconico/*` | Go 実装 (認証・アカウント連携・画像変換等) |
| 未知の API | `-no-proxy` では404、比較用プロキシモードではPythonへ転送 |

### EDCB 番組検索

- `-no-proxy` (`s.proxy == nil`) 時は通常の CtrlCmd `SearchPg` (1025) を Go から送信する。native 経路には Python / ariblib の実行時依存はない。
- プロキシ有効時は検索条件の検証を含め既存 Python 転送を維持する。native を無条件に proxy へ戻す変更ではない。
- `service_ranges` の省略・null は `ChSet5.txt` の三つ組を重複排除して対象にする。空配列は対象なしのまま送信する。
- UTF-16LE の検索条件と EventInfo を送受信し、視聴可能チャンネルの ONID/TSID/SID、イベント共有副側、終了済み番組を絞り込む。
- 検索失敗・不正 frame は Python 版と同じ `{"total":0,"programs":[]}`。ChSet5 取得失敗は従来の 500。
- TCP native 検索専用 client は HTTP キャンセルと期限、既定 15 秒のタイムアウト、64 MiB の応答上限を ChSet5 を含め適用する。旧 URL client は既存サイズ範囲を維持し、SearchPg 1025 のみ 64 MiB に制限する。本文は申告長を先取り確保せず実受信に応じて増やす。
- `TestPG3*` は実 TCP の合成 peer のみを使用する。本番 EDCB・予約サービス・SSH へは接続していない。
- 既存 `TestProgramSearchProxiedForEDCB` は assertion 不変で通過。no-proxy native 正常系と旧 proxy 構成を併存させる。
- Windowsの実パイプとLinuxのFIFO / UNIXソケットで期限・部分Read・中止を検証した。
  稼働中のEDCB接続はTCPを維持しており、EDCBアプリとのローカルパイプ接続は未確認。
  詳細文や入力検証の比較範囲は[MIGRATION.md](MIGRATION.md)を参照。

### FastAPI の 422 検証エラー互換 (`internal/api/fastapi_validation.go`)

- Python 版の 422 は大きく 2 種類ある。Pydantic の検証エラーは `{"detail":[{...}]}` の**配列**、
  依存 (ValidateVideoID / ValidateQuality など) が投げる HTTPException は `{"detail":"..."}` の**文字列**。
  クライアント (`client/src/services/APIClient.ts`) は `Array.isArray(detail)` で表示を分岐するため、
  文字列で返すと UI の表示が変わる。
- Pydantic はパラメータごとに最初の失敗で止めず、**全エラーを宣言順に 1 つの配列**へまとめる。
  パスパラメータ (`loc: ["path", name]`) が先、クエリパラメータ (`loc: ["query", name]`) が続く。
- FastAPI はサブ依存を解決してからエンドポイント自身のクエリを検証する。依存が例外を投げると
  その応答が確定し、その時点で蓄積済みの検証エラーは捨てられる
  (実機 Python: `/api/streams/video/abc/9999p/playlist` は品質エラー、`/api/streams/video/abc/720p/buffer`
  は検証エラー配列) 。
- サブ依存自身のパラメータが不正な場合はその依存は呼ばれない
  (`video_id=abc` のとき録画 ID の存在確認は走らないが、`quality` は `str` なので品質検証は走る) 。
- 文字列 → int の変換は前後の空白・符号・アンダースコア区切り・ゼロ小数部 (`1.0`) を許容し、
  小数 (`1.5`) や指数 (`1e2`) は `int_parsing` になる (`int_from_float` ではない) 。

録画・シリーズ一覧の`page`は本家同様、0以下でも先頭ページを返す。
旧キャプチャフォルダAPIでは本家の`ge=1`制約に従い、1未満は検証エラー配列を返す。

## 使い方

常駐サービスの登録・更新・削除は[SERVICE.md](SERVICE.md)を参照。

Go単独運転では、`config.yaml`と設定したバックエンド、同梱エンコーダーを用意し、
`server-go/`から実行します。配信画面のビルド済みファイルは`client/dist/`に含まれます。
以下の`.exe`はWindows用の名前です。Linuxでは拡張子を外し、ワーカーだけ`.elf`にします。

```powershell
# 開発中の単独起動
go run ./cmd/konomitv-go -no-proxy

# ビルドして実行
go run ./cmd/thumbnail-assets
go build -o ../server/go-native/FFmpeg-Thumbnail/thumbnail-worker.exe ./cmd/thumbnail-worker
go build -o ../kgo.exe ./cmd/konomitv-go
../kgo.exe -no-proxy
```

通常サムネイルの抽出には上記の専用 Go ワーカーが必要です。
Linux ではワーカーの出力名を `thumbnail-worker.elf` にします。
`thumbnail-assets` は固定した配布物の SHA-256 を検証し、デコード用の共有ライブラリだけを取得します。
実行時の Python は不要で、既存の FFmpeg / QSVEncC のファイルは変更しません。
Go 版専用のファイルは `server/go-native/` に配置します。

### Twitter連携用のChrome

既存のChrome / Braveがあれば優先して利用します。専用のChromeを置く場合は、Goだけで導入できます。
公式Chrome for Testing `157.0.8103.78`のWindows amd64 / Linux amd64・arm64配布ZIPをSHA-256で照合し、
展開後の全ファイルと共有ライブラリのリンク先を`assets-manifest.json`に記録します。
同じコマンドの再実行では全ファイルを再検証します。不明な既存ディレクトリや異なる版は上書きしません。

```powershell
go run ./cmd/browser-assets
# 取得済みZIPも固定したSHA-256で検証する
go run ./cmd/browser-assets -archive /path/to/chrome.zip
```

Ubuntuで不足する共有ライブラリと日本語フォントをアプリ内に置く場合は、次を指定します。
`apt-get download`と`dpkg-deb -x`だけを使い、OSへのパッケージ導入や設定変更は行いません。
`-apparmor-profile chrome`は既存の同名プロファイルを利用する指定で、新しいプロファイルは作成しません。
Chromeのサンドボックスは有効なままです。

```sh
go run ./cmd/browser-assets -ubuntu-libraries -apparmor-profile chrome
```

Linuxは`server/go-native/Chrome/chrome`、Windowsは`server/go-native/Chrome/chrome-win64/chrome.exe`を検出します。
取得元、ライセンス、実機検証の範囲は[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)と[MIGRATION.md](MIGRATION.md)を参照。

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

### Twitter のブラウザー処理

`internal/twitter/BrowserBackend` が本家の `TwitterScrapeBrowser` 相当を担当する。
既定でインストール済みの Chrome / Brave を探し、見つからなければ `server/go-native/Chrome/chrome`
(Windows は `chrome.exe`) も探索する。起動時は毎回専用の一時プロファイルを作り、保存済み Cookie と
Cookie 採取時のブラウザー環境を復元する。本家の `zendriver_setup.js` を Go 実行ファイルへ同梱している。
Python / Zendriver は実行時に使用しない。ブラウザーが必要になるまで Chrome は起動しない。

投稿は本家のモーダル・paste・fileInput・Ctrl+Enter の経路を使い、画像 FINALIZE と CreateTweet 応答を
CDP で監視する。投稿待ちの間は自動停止を無効にし、終了後に再予約する。
サーバー終了・セットアップ失敗・接続断後の再起動では、専用ブラウザーの子プロセスと一時プロファイルも回収する。
エラー画面は `server/data/twitter-debug-screenshots` に保存し、7日を過ぎた PNG を削除する。

通常の試験は外部へ投稿しない。実ブラウザーのローカル模擬ページ試験は明示して実行する。

```powershell
$env:KONOMITV_TWITTER_BROWSER_TEST = '1'
go test ./internal/twitter -run 'TestNativeTwitterBrowser' -count=1 -timeout=120s
```

### 録画フォルダの継続監視

- `-no-proxy` の Go 単独運転では、起動時から設定済みの録画フォルダを 10 秒間隔で観測する。Python 併走モードでは二重監視を避けるため開始しない。
- Go 単独運転では、起動時と `general.program_update_interval` 分ごとにチャンネル・番組を更新し、実況勢いを 30 秒ごとに更新する。各更新はサーバー終了時にキャンセルする。
- フォルダごとに一つの解析 worker を持ち、発見した順にキューへ入れる。全ファイルを先に収集・ソートしない。直近 24 時間の録画と監視開始後の変更を優先し、過去分の取り込みが新規録画を塞がないようにする。
- サイズ・更新日時・録画完了の安定化を検出する。リンク解決前後の除外パスも既存設定に従う。OS の削除・改名通知と巡回を併用し、消えた録画済みファイルは Missing として一週間保持する。
- 空になった保存先は、削除通知とディレクトリの同一性を確かめてから反映する。切断などで別の空ディレクトリが見えた場合や観測に失敗した場合には保持する。保存先の再作成後は監視を張り直す。
- 一覧のメタデータ登録と重いサムネイル / CM 解析を分ける。後者は取り込み時の通知と毎分の巡回を受ける別 worker で処理するため、未生成の画像は登録より遅れて反映される。
- 手動一括スキャンは HTTP 接続のキャンセルではなくサーバーの生存期間に従う。ブラウザーを閉じても継続し、サーバー終了時には中断する。実行中の二重要求は従来どおり 429。
- 一括スキャンは旧 `key_frames` を 50 件単位で `segment_map` へ移す。正常な既存キャッシュと再生中の追加位置は保持し、隣接セグメントが同じ映像を繰り返す旧キャッシュは変換し直す。変換できない行は元の情報を保持する。
- 録画の移動・改名は、同じサイズ・更新日時の誤差 2 秒以内・内容ハッシュで照合する。元の番組 ID・動画 ID・編集情報・サムネイル・CM 区間・シーク情報を保持して保存先を更新する。末尾がゼロ埋めされた TS は有効範囲のハッシュでも照合する。
- ドライブ間のコピーは、録画フォルダ内に元ファイルが見える間は登録を保留する。コピー途中の重複行を整理し、元ファイルが消えると監視から照合し直す。保留は手動スキャンと常駐監視で共有し、旧ドライブを設定から外した場合は実体が残っていても付け替える。
- `TestRecordingMonitor*`、`TestMetadataOnlyDefersBackgroundAnalysis`、`TestBatchScanSurvivesCanceledRequest` で新規検出・録画完了・解析分離・切断後の継続を検証する。

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
- `EDCBTuner` (`edcb_tuner.go`): EDCB バックエンドのチューナー制御。NetworkTV モードの EpgDataCap_Bon を
  起動して放送波の MPEG-2 TS を受信し、`RelayViewStream` (CMD 301) で取得した TCP 接続をストリームとして
  扱う。NetworkTV ID は 500 から採番し、Idle / Starting / Active / Cancelling の状態機械、`_isOwner()` 相当の
  制御権限ガード、チューナーの再利用 (`handoff`) とロック/アンロックは Python 版と同じ意味論で実装している。
  チューナーの確保 → チャンネル設定 (失敗時 0.5 秒間隔で最大 5 秒リトライ、`E-02E`) → 接続 (失敗時 `E-03E`) →
  読み取り → 終了処理 (切断・チューナー解放) の順に動作する。

EDCB のローカルモード (`edcb_url: tcp://edcb-namedpipe/`) は、Windows の
`\\.\pipe\SendTSTCP_{port}_{processID}` と Linux の
`/var/local/edcb/SendTSTCP_{port}_{processID}_{index}.fifo` を本家と同じ順番で探索する。
Linux は `flock` で同時利用の index を選ぶ。接続待ちをキャンセルでき、切断で読み取り待ちも解除する。
制御通信には Windows の双方向パイプと Linux の UNIX ドメインソケットを使う。
独立した OS の実パイプで検証しており、実際の EDCB アプリとのローカル接続は未確認。
稼働中の実機設定は TCP モードを維持している。

### クライアント設定 API

本家 `ClientSettings` の 51 項目を型・選択肢・配列内の要素まで検証する。
真偽値や数値の文字列は Pydantic と同じ規則で変換し、複数のエラーをフィールドの定義順と要素の位置付きで返す。
未指定項目は既定値で補完し、未知の項目は無視する。不正な入力や古い同期データでは保存値を変更しない。
ジャンル色・マイリスト・視聴履歴内の辞書は入力順を保持する。整数の精度、DB 保存と応答の数値表現も分けて扱う。
取得時には保存値を再検証し、不正な値を既定値として隠さない。

### サーバー設定 API

`GET /api/settings/server` は `config.yaml` を既定値で補完して返す。
既存フィクスチャは以前の Python 版が対象であり、現行本家のモデルとの照合は継続中。
`PUT /api/settings/server` は Pydantic 相当のバリデーション (列挙値・数値の範囲・URL のスキーム) を行ったうえで、
`config.yaml` を**コメントを保持したまま行単位で書き換える** (ruamel.yaml の代替) 。

**未対応**: Python 版のカスタムバリデーターのうち、環境に依存する検証 (EDCB / Mirakurun への接続確認、
ポートの使用状況、エンコーダーの対応状況) は Go 版では行わない。

### 録画番組 API (`/api/videos`)

録画番組の一覧・検索・詳細・サムネイル画像・録画ファイルのダウンロード・削除を Go で実装している。
応答互換性はフィクスチャと API 試験で確認する。機能全体の移植完了は `MIGRATION.md` の未完了項目も含めて判断する。

- 一覧と検索は Python 版と同じ生 SQL (recorded_programs + recorded_videos + channels の結合) を発行する。
  `order=ids` の場合は指定された ID の順序を維持する (ページングも Python 版と同じ挙動) 。
- サムネイル画像は `server/data/thumbnails/<file_hash>.webp` を配信し、存在しない場合は
  `server/static/thumbnails/default.webp` をキャッシュ無効で返す。
  `ETag` / `Last-Modified` による 304 応答 (If-None-Match / If-Modified-Since) にも対応する。
- 代表サムネイルの変更は、ネットワーク ID・サービス ID・イベント ID・開始時刻が一致するキャプチャに限る。
  読める保存場所と内容ハッシュを確認して 480×270 の WebP を作成する。同じ内容の録画の `representative.revision` も進める。
  自動再生成も DB 上の現在の番号から進め、生成中に入力のハッシュが変わった場合は情報を書き戻さない。
- 録画ファイルのダウンロードは `http.ServeFile` で配信するため Range リクエスト (シーク) に対応する。
- 削除は管理者のみ実行でき、データベースのレコード・サムネイル画像・補助ファイル (.ts.program.txt / .ts.err) ・
  録画ファイル本体を Python 版と同じ順序で削除する。
- ニコニコ実況の過去ログコメントは過去ログ API から取得して DPlayer の形式に変換する
  (コメントの色・位置・サイズの解釈は Python 版と一致することをフィクスチャで検証済み) 。

メタデータ再解析 (`POST /api/videos/{video_id}/reanalyze`) とサムネイル画像再生成
(`POST /api/videos/{video_id}/thumbnail/regenerate`) も Go で実行する。
代表画像は 480x270 の候補を Go で採点し、人物・アニメの顔がある候補を優先する。
輝度、コントラスト、鮮明さ、輪郭、情報量、黒帯・単色・画面端の減点、アニメの色分布を評価する。
顔検出には本家と同じ HAAR / LBP モデルを実行ファイルへ埋め込む。実行時の Python / OpenCV は不要。
番組ジャンルと録画余白を DB から取得し、採点した画像を再シークせずに WebP へ保存する。
画像採点の浮動小数点集約と色分布の Lab 丸め・k-means の固定乱数には OpenCV との差がある。
31 画像の本家比較では人物・アニメ・補助人物の検出矩形、白黒変換、輪郭、黒帯が一致し、
総合点の許容差は通常 0.003、アニメ 0.08 点で検証する。区間内に候補がなければ本家同様に無作為に選ぶ。
映像 PID が途中で変化する MPEG-TS は、同梱 tsreadex のサービス選択・PID 固定を通して一度だけ順次デコードする。
I フレームの整数 PTS と time_base から時刻を算出し、本家と同じ 1 ms の境界判定で直前の画像を選ぶ。
末尾に届かない候補は最後の I フレーム、取得画像がない場合は黒画像で補完する。期限は 600 秒で、キャンセル時も子プロセスを回収する。
通常経路の backward I フレーム選択・連続失敗時の打ち切りは専用Goワーカーで処理する。
MPEG-4 は同名の `.psc` から放送情報を復元し、番組名・チャンネル・録画時刻を解析する。
番組情報がない場合も書庫から得た録画時刻をファイル名のフォールバックへ引き継ぐ。
192バイトのBDAVは追加ヘッダーを解析時だけ除去し、SITからチャンネル・番組情報と録画時刻を取得する。
元動画のバイト列・ハッシュ・物理的な保存位置は保持する。
同梱モデルと画像処理のライセンスは [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) を参照。

採点の比較データは固定した本家の `ThumbnailGenerator.py` の実メソッドで生成する。
`server/` から次を実行する (`SOURCE.py` は対象 revision の本家ファイル、
`astronaut.png` は同 notices に記載した scikit-image v0.25.2 の NASA 写真):

```powershell
uv run --no-project --python 3.13 --with opencv-contrib-python-headless==5.0.0.93 --with numpy==2.5.4 python ../server-go/tools/generate_thumbnail_scoring_reference.py SOURCE.py astronaut.png
```

順次抽出の比較データは `tools/generate_thumbnail_tsreadex_reference.py` で生成する。
制御したフレーム 14 ケースのほか、`--normalized-media` に実 tsreadex の出力、`--media-input` に元の TS を渡すと、
固定した本家の実メソッドを PyAV 19.0.2 で実行する。`--with av==19.0.2` を上記の依存指定に追加する。
付属の `thumbnail_tsreadex_input.mpegts` は 640x360 / 25fps / MPEG-2 / B フレーム2枚 / GOP25 の赤・青各6秒を連結した合成データで、
サービス101の映像PIDが256→512へ変化する。元動画・前処理出力の SHA-256 と本家が選んだ色を比較 JSON に記録する。
実機試験は `KONOMITV_TEST_FFMPEG` と `KONOMITV_TEST_TSREADEX` を指定して有効にする。

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
  EDCB / Mirakurun バックエンドのチャンネル情報・番組情報は Go で取得して更新する。

録画スキャン・バックグラウンド解析・キャプチャスキャン・DB 最適化は Go で実行する。
各処理の `/run` と `/status` に対応し、HTTP 切断後も処理を続ける。DB 最適化には管理者認証が必要。
欠落した録画は一週間保持し、保存先が読めない場合や大量欠落時には期限削除を見送る。

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

### Bluesky の添付画像

- 2,000,000 バイト以下の添付画像は元の内容を送る。上限を超える場合は同梱 FFmpeg の libwebp で変換する。
- 元の解像度で品質 85・80・70・60・50 を試し、収まらなければ長辺を 1920・1600・1280・1024・800 の順に縮小する。最初に上限内へ収まった結果を使い、小さい画像を拡大しない。
- 透過情報と GIF の論理画面を保持し、最終的に送る画像の寸法を `aspectRatio` に設定する。
- 外部への投稿を伴わないローカルの受け口と、実機同梱の FFmpeg で変換・blob 送信・投稿レコードを検証する。

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
