# 最終安全監査・第4チェックポイント

作業コピーは task-4/KonomiTV-app、branch work/go-standalone-memory、ベース e412b23a。
第3 tree `2ae11fa67548305750a1ae131185025ce19395c3` からの増分を別パッチに保存する。
本番への配備、サービス再起動、実アカウントの認証/投稿、実 EDCB 操作、push/tag/Release は行っていない。

## 安全修正

- メタデータの重いファイル解析・後処理・手動サムネイル再生成を、全 Service 共有の同時1件へ制限。視聴エンコーダーはこの枠に含めない。監視は枠が busy なら次の pass へ回し、手動/起動時処理は context 付きで待つ。長い手動背景解析はファイルごとに枠を譲る。
- ディレクトリ取得は全スキャン合計で `ReadDir(128)` を100回/秒以下へ平準化。これはディレクトリ読み取りバッチの制限で、すべての stat や帯域の上限を保証するものではない。
- symlink は正規化後の設定済み録画 root 内のみ辿る。設定範囲外の target は自動探索しない。外部 target を使う場合は、その target を録画フォルダとして明示設定する。明示 root 自体が symlink の構成は利用できる。
- Windows junction の追加テストで `filepath.EvalSymlinks` だけでは正規化されない target を検出したため、Windows は共有を妨げない read-attributes handle の `GetFinalPathNameByHandle` で実 target へ解決する。共通の録画パス正規化にも反映し、junction root の登録/欠落照合を維持する。
- 既に辿った正規化ディレクトリを再帰しないため、ループと複数 alias による重複探索を防ぐ。深さは64未満、既知ディレクトリは65536まで。上限到達は不完全スキャンとして扱い、削除照合をしない。録画ファイル全件のパス集合やソートは作らない。
- root と子ディレクトリの同一性を保持して検証。入れ子のストレージが差し替わった場合、権限失敗、壊れた link、キャンセルなどは削除照合へ進めない。欠落ファイルの親ディレクトリが消失/差し替えされている場合も DB 行を残す。
- 未登録の旧 reanalyze/thumbnail proxy stub 2個を削除。登録先は既に Go metadata handler であり、動作変更はない。

## 監視の既知制約

- ディレクトリの消失/交換がストレージ切断か意図的削除かを一般に判別できないため、保守的に DB 行を残す。この場合はログとストレージ状態を確認し、復旧後のスキャン、または意図的変更を確認した後の監視再起動で基準を取り直す必要がある。自動的に本番サービスを再起動しない。
- 起動前から mount が空ディレクトリへ戻っていた場合や、同じ inode/device を再利用する特殊な FS の状態まで完全には判別できない。実 NAS/SMB の切断・再接続受入れは未実施。
- OS の同期 filesystem syscall 自体が固まるケースを context だけで強制中断できない。アプリ側の探索深さ・状態サイズ・並行実行は制限するが、OS/ストレージ障害時の完了時間は保証しない。
- 上限を超える構成は不完全として安全停止するため、深さ64以上・65536超ディレクトリへの対応は残課題。上限で消えたファイルとして登録データを削除しない。
- 重い解析は全ドライブ共通で1件という保守的な制限。ドライブ別に並行度を増やす最適化は未実施。範囲外 symlink を利用する既存構成では明示 root の追加が必要。

## 115 route の静的監査

`internal/api/route_audit_test.go` が Python router ソースから115 routeを抽出し、Go AST の登録と dispatcher、分類台帳を照合する。全115件が分類済み。主な実装経路の区分は Go/API・DB 53、外部HTTP31、EDCB12、encoder/backend10、CDP9。区分は主経路であり、API・DB区分にも設定依存の外部取得はある。

**既定の no-proxy 起動で、Python アプリへの依存または常に未実装エラーとなる登録ルートは確認されなかった。**
これは全正常系の完全互換性や実サービスの成功を保証する結論ではない。下記の機能差は残っている。

| 残る Python 転送 | 条件 | Go 単独時 |
| --- | --- | --- |
| POST /api/programs/search | 明示した旧 proxy + EDCB | Go CtrlCmd SearchPg |
| POST /api/maintenance/update-database | 明示した旧 proxy + 非IPTV | Go EDCB/Mirakurun 更新 |
| GET /api/streams/video/{video_id}/{quality}/offline-stream | 明示した旧 proxy | Go KTVODLP 保存 |
| 未登録 /api/* | 明示した旧 proxy の catch-all | 404。115件の既知 route は登録/dispatcherで捕捉 |

API の Server メソッド呼び出しを AST で辿り、既知 route の転送箇所が上記3件から増減したらテストを失敗させる。これらの no-proxy 実装には既存の合成 peer/API/FFmpeg テストがある。
この静的検査は、将来 callback や別層へ転送を移す変更まで自動で証明できるものではない。

## 未実装/部分対応として残るもの

| 項目 | 状態と影響 |
| --- | --- |
| EDCB 未同梱チャンネルロゴ取得 | CtrlCmd のロゴ取得は未実装。同梱ロゴがなければ既定ロゴへフォールバックする。GET /api/channels/{channel_id}/logo の部分対応であり、単なる実機未検証ではない |
| 一括スキャンの旧 key_frames→segment_map DB 自動移行 | 未移植。既存データは保持する。旧DB全形式の再生/移行互換性は別途監査が必要 |
| 動的 analytics filter 取得 | Python と同じ埋め込み hook の固定 patterns を使う。追加 filter の取得は未移植 (第3記録参照) |
| join_logo_scp による CM 検出 | Python 側も未実装。Go固有の欠落として数えない。既存チャプター解析は実装済み |

防御用の未登録エラーは、segment encoder、offline factory、thumbnail renderer、明示 UnavailableBackend に残る。通常の main/API 構築は実装を登録する。IPTV の既定 logo asset が存在しない場合の501も残るが、asset 欠落という異常構成であり、正常機能の未実装ではない。配布時は static/client/外部実行ファイルを含む packaging 検証が必要。

## 外部環境での受入れが必要なもの (実装あり・未検証)

| 対象 | 残る受入れ |
| --- | --- |
| EDCB / Mirakurun / mirakc | 実ライブ、handoff、Windows pipe・Unix FIFO/control socket、EPG/予約/検索。合成 peer は検証済み。本番チューナーを操作していない |
| FFmpeg / QSVEncC 等 | 実 GPU と全品質。FFmpeg 合成/実録画 HLS と保存は限定条件で検証済み |
| Twitter | 本人 Cookie、実 Web App の webpack/DOM と応答。モックと localhost実Chrome CDP は検証済み、実サービス未操作 |
| Bluesky / NicoNico / BML / IPTV外部配信 | 実認証・外部配信/HTML差異。既存モックあり、全サービスの実受入れは未実施 |
| 録画保存先・解析 | 大規模 NAS/SMB、再接続、全放送局/録画形式・ARIB descriptor。TS情報解析の Go 実装とfixtureはあるが全互換は未証明 |

## テストとベータ評価

Windows: metadata全テスト、探索/監視/I/O制限、route AST監査が成功。端末に symlink 作成特権がないため、既存ディレクトリ target は junction で追加検証する。壊れた symlink は Windows で skip、Linux で実検証する。
Windows junction テストの初回失敗は `phase4-windows-junction-tests.txt` と診断ログへ保持し、修正後の成功は `phase4-windows-junction-final-tests.txt` に保存する。
Linux: akki ユーザーの既存隔離 `/tmp/konomi-app-memory-onUDzU` 内の合成ファイル/DBだけを使い、symlink・深さ・権限失敗・DB保持・I/O上限・録画ライフサイクル・背景解析を検証。権限テストは skip せず成功。本番データは読み書きしていない。
ログは task-4 の `phase4-metadata-tests.txt`、`phase4-metadata-final-tests.txt`、`phase4-final-targeted-tests.txt`、`phase4-route-audit-tests.txt`、`phase4-linux-safety-final-tests.txt`。
`go test ./... -p 1 -count=1 -timeout=180s` は全パッケージ PASS (API 108.686秒)、`go vet ./...` 成功。`phase4-all-tests.txt` と出力なしの `phase4-vet.txt` に保存。
Windows junction 共通パス修正後も全 suite を再実行し PASS (API 93.748秒)、全 vet 成功。最終結果は `phase4-final-all-tests.txt` / `phase4-final-all-vet.txt`。追加 root 回帰テストは `phase4-linked-root-tests.txt` と最終 Linux ログでも PASS。

評価: **既知制約を明記した限定ベータの候補**。フル互換版・実機検証済み版としての配布判定はしない。installer統合版については、マージ後の全テストと配布物の静的 asset/外部ツール/初回起動検証を経て親で判断する。本番置換を承認済みとは扱わない。
今回メモリの追加最適化・再生測定は実施していない。前回の「実再生PSS改善未確認」を維持し、探索安全上限を省メモリ実測の成果として扱わない。

installer境界: 今回 `main.go` / `server.go` へ追加差分なし。`internal/api/metadata.go`、`videos.go` と metadata 内の修正・テスト・台帳が中心。別コピーや installer ファイルは編集していない。
