# Go 単独運用の実装チェックポイント (2026-10-22)

この時点では全機能同等・完成・本番配備可能とは判定していません。
親スレッドのチェックポイント要求で作業を区切り、次の担当が続けられる状態を保存しています。

## 作業の保護・担当境界

- 作業コピー: `C:\Users\aki_0\Documents\Codex\2026-10-22\task-4\KonomiTV-app`
- branch: `work/go-standalone-memory`、基点 `e412b23a`。
- 元ソース `J:\github\KonomiTV` は変更していません。
- 元ソース HEAD は `e412b23a`、親は配備バイナリの基点 `f8814d6f`、さらに親は公開基点 `3ae15005`。
  配備バイナリに未コミット変更が含まれるため、バイナリとの差分を完全に復元したという意味ではありません。
- 元ソースの未コミット変更は installer/Installer.py、Python の VideoEncodingTask.py / VideoStream.py / TSKeyFrameSeeker.py。
  これらを隣の `source-local-changes/` に別保存しました。元ソースの未追跡ファイルも削除・変更していません。
- task-3 のインストーラー専用コピーと、新規 DB 初期化は未変更です。
- `cmd/konomitv-go/main.go` はアプリ起動・定期更新接続・proxy 既定変更のため編集しました。
  インストーラーとの統合時にこのファイルを照合してください。
- 本番へのデプロイ・サービス再起動・本番設定/録画/認証/ネットワーク/GPU 変更・push/tag/Release は未実施。
  無効化済み healer / refresh_db.sh / recfix も変更していません。

## 依存・優先度と実際の状態

| 優先度 | 機能/API | コードで確認した状態 | 残り・依存 |
| --- | --- | --- | --- |
| P0 | サーバー設定保存 | 行置換に複数行値の残骸バグ。今回 YAML ノード更新へ修正 | 型・起動制約検証、固有 temp、sync、置換、並列更新をテスト。プロセス間ロックは未実装 |
| P0 | 録画 HLS segment | `main.go` で SessionSegmentEncoderFactory 登録済み。README が古い | FFmpeg/HWEncC・tsreadex。合成実エンコードで検証、実録画比較を別記 |
| P0 | offline-stream | KTVODLP の Go 実装と起動時登録あり。旧 proxy 有効時だけ転送 | 実 FFmpeg で全セグメントの再デコードが通過 |
| P0 | EPG 自動更新 | native EDCB/Mirakurun 更新 API は存在したが startup/periodic 接続なし | 今回 startup/定期接続。実バックエンドの更新は未実行、合成 peer テストのみ |
| P0 | IPTV 更新・取得・logo | native。`channels/{id}/logo` も IPTV を直接処理 | 今回 30 秒周期で TTL を確認する startup worker 接続。IPTV 無効化仕様は task-3 と統合 |
| P1 | 録画監視・新規録画反映 | 手動一括スキャンは native。常駐 watcher の起動接続なし | 未完了。既存ファイルの全件収集・ソートを導入しないこと。Recording 完了遷移・削除・排他も要検証 |
| P1 | 再解析・thumbnail/regenerate・run-background-analysis | native route と Go エンジンあり。旧 proxy ハンドラーは未登録の死コード | FFprobe/FFmpeg。TS 188 byte 解析あり。Python 同等の I/O 制限、異常解析時 API 結果、PSC/MP4 等の互換性を要監査 |
| P1 | Twitter | API/GraphQL 整形層はあるが既定は `NewUnavailableBackend` | 実ブラウザ起動・CDP・Cookie・Web App 経由実行・投稿 UI が未実装。通常機能の 500 を解消したとは言えない |
| P1 | EDCB ライブ | TCP RelayViewStream 実装済み | Windows 名前付きパイプ受信が未実装。EDCB 自体の再実装は範囲外 |
| P1 | 番組検索 | no-proxy 時 SearchPg native。明示 proxy 時は既存転送 | 多数の合成 peer テストあり、実機・Pydantic 細部の互換性は継続検証 |
| P1 | 録画予約/自動予約/プリセット | native route 登録済み | EDCB 外部 API。テストあり、実予約の作成変更は未実施 |
| P1 | Bluesky / ニコニコ | native API 実装あり | 外部サービス依存。実ユーザー投稿・認証変更はしていない。実機完全同等とは未判定 |
| P2 | その他読み取り・ユーザー・設定・キャプチャ・BML | native route 登録済み | README とコードの整合を更新中。BML の HTTP proxy は外部 Web のための機能で Python アプリ依存とは別 |
| 別担当 | DB 初期化・インストーラー | task-3 で作業中 | このコピーでは変更しない |

FFmpeg / QSVEncC / NVEncC / VCEEncC / rkmppenc、tsreadex、psisiarc、psisimux、EDCB、Mirakurun は
Python アプリとは別の外部依存です。Go 単独運用という依頼をこれらの再実装には拡大していません。

ルートの機械抽出では Python 115、Go の literal mux 登録 111。
差分として現れた BML 2 ルートは dispatcher、Bluesky の path converter 4 ルートは可変パス処理を
使っているため、数だけで未実装とは判定できません。ルート一覧は作業コピーの隣に保存しました。

## 今回のコード変更

1. `internal/config/settings_save.go` と platform 別 replace に構造化保存を実装。
   コメント・未知のキー・既存順序を維持し、複数 document/重複キー/型不正を拒否。
   旧値を値ノードごと置換し、一時ファイルの固有名、既存 mode、sync、同一ディレクトリでの置換を使用。
   Windows は MoveFileEx(REPLACE_EXISTING | WRITE_THROUGH)。symlink はリンク先を更新。
2. `internal/iptv/quality.go`: チャンネル全件分の待機 goroutine を作らず worker 数を既存の 8 に固定。
   キャッシュを 16384 URL に制限。追い出した URL は再検出するので画質情報を省略しない。
   キャンセル時に新規処理投入を止め、キャンセル結果を恒久キャッシュしない。
3. `internal/videostream/encoding_task.go`: feeder の約 1.8 MiB チャンクを同期処理後に再利用。
   入力・出力・エンコーダー引数・画質・先読み量は変更していない。
4. `internal/api/background.go`: Go 単独時の EPG startup/定期更新と IPTV TTL 更新を接続。
   各 worker は逐次実行、期限とキャンセルを持ち、main は DB close 前に終了を確認。
5. 既定を `-no-proxy=true` に変更。`-no-proxy=false` または明示 `-python-backend` で旧構成へ opt-in。
   明示 `-no-proxy=true` は転送先指定より優先。転送先の自己参照拒否は維持。

## 測定

Windows amd64 / Go 1.26.2 / Ryzen 5 7430U。合成データ・同じコード経路で前後を測定。

| 部品測定 | 前 | 後 | 解釈 |
| --- | --- | --- | --- |
| 11152 ch、8 HTTP を停止させた時の追加 goroutine | 11153 | 9 | 検出結果数・通信並列数は同じ |
| 同条件 StackInuse | 91619328 bytes | 約 0.3 MiB | RSS/PSS そのものではない |
| 録画 feeder 32 MiB、確保量/操作 | 約 34.16 MiB | 約 1.80 MiB | 約 94.7% 減。入力・出力を削減していない |
| 同 feeder 時間、3 回 | 22.70/22.98/26.15 ms | 9.82/12.20/12.87 ms | 合成ファイル読込。実再生遅延とは別 |

alloc profile では旧 feeder の `readFileChunk` が約 99% を占めたため修正対象にした。
IPTV 全件検出の時間はばらつきがあり、cache 上限追加後の 3 回は 73.1/92.5/87.6 ms。
高速化を一律に主張せず、主な効果は待機スタックの削減と寿命上限である。

実録画の隔離 HLS 比較は `playback_measure_test.go`。録画 9654 のメタデータを
本番 DB から readonly で一件取得し、書き込みは合成 DB にだけ行う。
FFmpeg/libx264 同じ引数、1080p、sequence 2～6 (12.012～42.042 秒)、約 1x 速、30 秒。
250 ms ごとにアプリの RSS/PSS、子プロセス PSS、process tree CPU、HTTP 遅延と segment SHA256 を採る。
背景更新・Python アプリ・本番バイナリは使わない。

前後バイナリと一時 DB は `/tmp/konomi-app-memory-onUDzU` のみ。
1 回目の前後比較の結果は隣の `hls-before-run1.txt` / `hls-after-run1.txt` を参照。
両方 PASS、5 セグメントのバイト数と SHA256 は完全一致。

| 録画 9654、各 30 秒の 1 回目 | 前 | 後 |
| --- | --- | --- |
| アプリ平均 RSS | 85.63 MiB | 88.29 MiB |
| アプリ平均 PSS | 85.63 MiB | 88.28 MiB |
| アプリ最大 PSS | 135.34 MiB | 144.88 MiB |
| 子プロセス平均 PSS | 129.81 MiB | 129.80 MiB |
| process tree CPU (100%=1 core) | 195.36% | 195.93% |
| 最初の要求 HTTP 遅延 | 5.62 秒 | 5.24 秒 |

**今回の 1 回の再生測定ではアプリ常駐メモリの低下は確認できない。**
部品の確保量削減と実再生の RSS/PSS は区別する。前後で先読み生成数にも差があり、
CPU/遅延の優劣も断定しない。次は各 2 回の再現と profile により、セグメント保持等を評価する。

比較用バイナリの SHA256:

- before: `90baaff709cb9a61ced9dc2037bd2c81a6219873a82438b4c9aaf88c9ea3b2da`
- after: `627151a3a95c5d5f4af293185b178e28e107775ad7658b33b7d168be71b33bda`

親からチェックポイント要求を受けたため、予定していた各 2 回の再現確認は未完了。
過去の Python 484 MiB vs Go 79 MiB との比較とはハーネスが違うため混同しない。

## 検証・未完了

- ベースの全 Go テストは通常権限で通過 (隣の baseline-test-results.txt)。
- sandbox では localhost 接続・ファイル rename が拒否され、多数の基準テストが失敗。
  合成 HTTP/TCP peer と temp DB のテストを通常権限で実行して確認した。auto-review 拒否は無し。
- 設定保存: 5 行の残骸、literal/folded/引用付きの複数行、改行/引用符、コメント、未知キー、並列部分更新、
  不正入力で元ファイル不変をテスト。
- 実 FFmpeg 合成映像の offline-stream は生成・終端・全 3 segment の映像/音声再デコードが通過。
- EPG startup の合成 peer テストで一時 DB を更新。初回テストは同期読み取りの SQLITE_BUSY を
  即時失敗させていたため、更新完了を期限内に再読込するテストへ修正。
- 既定 proxy 切替で既存自己転送拒否テストが失敗し、明示 backend opt-in の扱いを修正。
- 修正後 `go test ./...` と `go vet ./...` は両方成功。全テスト結果は隣の checkpoint-test-results.txt、
  vet (診断なし) は checkpoint-vet-results.txt を参照。gofmt 適用・git diff --check も成功。
- README は主要な古い状態表を修正。未登録の旧ハンドラー・古いソースコメントの全面整理は未完了。
- Linux の設定保存/権限/symlink/atomic failure の追加検証、race 検証、常駐背景処理ありの長時間 RSS/PSS、
  実機の EDCB/Mirakurun/Twitter 互換性・同時視聴・シーク等は未完了。

親への送信 API は thread not found で失敗した。送信は実行されていない。
本チェックポイントの最終応答を親が受信して再開する。
