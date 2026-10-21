# 第2チェックポイント (2026-10-22)

全機能同等・完成・本番配備可能とは未判定。親からの途中報告要求で区切る。
作業コピーと基点は第1チェックポイントのまま (`work/go-standalone-memory`, e412b23a)。
元ソース・task-3・本番設定/サービス/録画/認証・ネットワーク・GPU は未変更、公開なし。

## 今回の実装

- 常駐録画監視: `metadata/watch.go`。5秒周期で directory を ReadDir(128) で逐次処理。
  全ファイルの先取り収集・ソートをしない。DB 削除照合は id keyset 256件ずつ。
  新規・変更・録画完了・削除を反映し、startup scan と独立に開始する。
- `metadata.Service` を Server 内で共有し、録画状態がリクエストごとに消える問題を解消。
  固定128本のファイルロックで scan/watch/API の同一ファイル操作を直列化。
  watcher は処理中ファイルを次周期へ回し、startup の重い解析を待って停止しない。
- Recording が永続的に残る早期 return を修正。無更新15秒・同じサイズで最終解析して Recorded に戻す。
- Windows のファイル時刻(100ns)と DB 時刻(μs)の比較を正規化し、無変更の再解析を防止。
- キャンセルを AnalysisFailed として保存しない。
- startup scan も読取成功した root だけで削除を照合。切断・権限失敗・mount identity変化時は既存DBを保持。
- EDCB local pipe: 新規 `internal/edcbpipe`。Windows SendTSTCP pipe、Linux FIFO の port0～29/index0～1 探索、
  flock による index 排他、期限・キャンセル、read待ちの close 解除を実装。
- EDCB 制御通信: Linux は Unix stream socket、Windows は overlapped named pipe。
  分割ヘッダー/本文を読み切り、応答サイズ・短い本文・キャンセルを検証する。
- tuner 接続待ちを handoff/disconnect/close で解除し、遅れて開いた旧所有者の pipe は閉じる。

## ルートと依存

[API_IMPLEMENTATION_AUDIT.md](API_IMPLEMENTATION_AUDIT.md) に Python Router 115 route の登録先・主依存を記録。
ルート登録と package テスト成功だけで全 route の正常系・実機互換性を保証しない。
既定 no-proxy の切替は第1チェックポイントの変更であり、未完成機能の実装の代わりにはしていない。

## 検証

- Windows OS実パイプ: 受信、Read中Close、制御の1byte分割応答、短い本文、不正サイズ、キャンセル成功。
- tuner mock: named pipe受信、EOF判定、handoff中キャンセル、close後の遅い接続回収成功。
- Windows/Linux監視: 新規→成長→完了→無変更skip→削除、root切断時DB保持、停止成功。
- Linux実FIFO: 使用中indexを飛ばして次indexを選ぶ、受信、Close、writer不在deadline成功。
- Linux Unix socket: 分割応答・キャンセル・応答サイズ成功。
- Windowsの全テストで startup EPG検証の合成DB read/commit競合が発生し、モックの到達通知とworker終了で
  待ってからDBを確認するテストへ修正。繰り返し5回の targeted test で通過。
- 最終全体の実行結果は作業コピーの隣 `phase2-checkpoint-tests.txt` / `phase2-checkpoint-vet.txt`。
  Linux targeted 結果は `watch-pipe-linux-tests.txt`、Windows targeted は `watch-pipe-tests.txt`。

## 実再生メモリ (前後各2回を完了)

録画9654、同一FFmpeg/libx264引数、1080p、12.012～42.042秒、アプリHLS経路、約1x速30秒。
本番DBはreadonlyで一件のメタデータを参照し、一時DBのみ更新。背景処理なし。
第1チェックポイントの前後バイナリをそのまま使い、今回追加したwatcherは起動していない。

| Run | アプリ平均PSS | 子プロセス平均PSS | process tree CPU |
| --- | --- | --- | --- |
| 前1 | 85.63 MiB | 129.81 MiB | 195.36% |
| 前2 | 85.74 MiB | 125.92 MiB | 191.54% |
| 後1 | 88.28 MiB | 129.80 MiB | 195.93% |
| 後2 | 85.82 MiB | 126.01 MiB | 194.99% |

実再生PSSの改善は確認できない。先読み生成数とCPU/遅延も単純比較で優劣を断定しない。
全4回で取得した5セグメントのサイズ・SHA256を照合した。結果と生ログは隣の `hls-*-run*.txt`。

afterの30秒allocation profileは約954MiB。主な割当元は pesParser.push 45.9%、
encodingAttempt.appendPES 31.2%、packetizePES 7.9%、packet受信 6.0%。
保持メモリの削減は未実装。次はPES/TS packet生成時のコピーを減らす候補を、出力一致と各2回の再測定で評価する。
profile は `hls-before.pprof` / `hls-after.pprof` と `hls-after-alloc-top.txt` に保存。

## 未完了と再開手順

- Twitter既定backendはまだ UnavailableBackend。CDP実装途中の6ファイルを本体のビルドから外し、
  隣の `twitter-backend-wip/` へ保全した。認証/Cookie投入/実投稿は一度も実行していない。
- WIP は CDP command/event配送、Chrome/Brave検出・専用profile・所有プロセス回収、webpack hook bootstrap、
  GraphQL呼出し、Cookie書出し等の途中実装。**未コンパイル・未テストで統合不可**。
  `Account.LoadCookies`・環境override・GraphQL Ordered JSON変換・screenshot helper・Compose UI・テストが未接続。
- WIP が使う `github.com/gorilla/websocket v1.5.3` は取得済みだが、active本体の go.mod/go.sum からは外した。
  続行時に依存追加とモックCDP/隔離ページのテストを行う。ユーザーの実認証/投稿は行わない。
- 常駐監視のI/O制限、symlink alias削除/再接続、録画中再起動、同時rename/scan等の追加監査を続ける。
  NAS/SMBはpolling対応だが、大規模実機の負荷・遅延の受入れは未実施。
- EDCB実機(local pipe/TCP)・外部アカウント・全API個別正常系は未受入れ。
- 旧コメント/未登録の死ハンドラーを整理し、READMEの全段落の整合を確認する。
- 新規DB初期化/installerは別コピーの88cd8b53912eece72f105b705914e4e17ebd2c71。
  統合境界は **main.go に加えて今回 server.go**。他コピーを編集せず親が統合する。
- Git作者未設定のためコミットは作らず、stage/tree/patchで保存する。

第1チェックポイントは `go-app-checkpoint.patch`。
第2チェックポイントの cumulative は `go-app-phase2-checkpoint.patch`、第1からの差分は
`go-app-phase2-incremental.patch`。両方を重ねて適用しないこと。
