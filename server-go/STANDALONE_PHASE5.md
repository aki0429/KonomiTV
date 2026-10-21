# EDCB ロゴ・旧キャッシュ移行 (第5チェックポイント)

ベース: e412b23a、作業コピー task-4/KonomiTV-app、branch work/go-standalone-memory。
第4 tree `dbf4fe6136d701d3de0423ff401633376258f2b2` からの増分を保存する。
本番 DB/録画/設定/サービス・実機 EDCB・実アカウントを操作せず、公開していない。

## EDCB 未同梱ロゴ取得

`GET /api/channels/{channel_id}/logo` は同梱ロゴ優先の後、EDCB `FileCopy2` で `LogoData.ini` と `LogoData\\*.*` を取得し、NID/SIDから logo ID を求める。ロゴ種別は Python と同じ5→2→4→1→3→0の優先順。PNG/BMP、ETag/304に対応する。
索引のファイル名は形式を検証し、パスの区切りや traversal を含む値を拒否。context/5秒期限と既存 CtrlCmd transport の64MiB上限を適用する。ロゴが無い、バックエンド不通などは既定画像へフォールバックする。
チューナー・録画・予約操作は送らない。テストはモックと独立した localhost CtrlCmd peer の command2060/version5/UTF-16 vector/構造体応答で実施。実機受入れは未実施。

## key_frames 自動移行と保護

起動時/手動の一括スキャンから `Store.MigrateKeyFrameCaches` を呼ぶ。スキーマ変更・新規DB初期化はしない。

- Recorded の ID を50件ずつ取り、一度に1録画の JSON を処理する。各旧 JSON は16MiBまで。範囲外・壊れた入力・不正な timing は変更せず skipped と診断へ残す。
- timing の安全範囲は frame rate 0.001〜1000、duration は0超〜6000000秒。極小の破損フレームレートによる有理数ゼロ除算も防ぐ。これらの制約で移行しない場合も元キャッシュは保持し、Pythonへ転送しない。
- MPEG-TS: 既存の Go planner で segment_map を生成する。正常な既存 map は保持。壊れた旧 map は旧 key_frames から修復するか、旧キーが無ければ空へ戻してオンデマンド探索へ委ねる。
- MPEG-4等: moov の同期サンプル表を再生時に読むため、旧 key_frames のみ整理する。
- DB更新前に、移行前後のキャッシュ値・録画ID/path/hashを JSON へ退避。既定先は DB 横の `keyframe-migration-backups/`。一時ファイルの Sync/Close、同一ディレクトリへの atomic rename、Unixディレクトリ同期 / Windows write-through rename を行う。保存失敗時はDBを更新しない。
- 更新は録画ID/path/hash/旧キャッシュ/timingが読み取り時と一致する場合だけトランザクションで実行する。競合・書き込み失敗・キャンセルは元の値を保持する。録画メディア本体は読み書きしない。
- 変換済み重複位置の検索を集合に変更し、大きい移行で二次時間の走査を避けた。12時間・25fps・2秒GOPの7200セグメントについて入力位置/DTSを独立式で照合。これは実再生 PSS 改善の測定結果ではない。

初回移行後も SQLite のファイルサイズを自動的に縮小する VACUUM は行わない。バックアップは自動削除しないため、旧キャッシュと移行後キャッシュの JSON (引用符等の増分を含む) 相当の空き容量が必要。バックアップには録画パス/ハッシュが含まれるため公開物へ含めない。

## 復元用 Go 保守ツール

`cmd/keyframe-cache` を Windows/Linux 向けにビルド済み。Python や Go ツールチェーンは実行時不要。
アプリ用DBの初期化処理を呼ばず、既存の通常ファイルを mode=rw でのみ開く。存在しないDBは作らない。

```text
keyframe-cache -database <既存DB> -restore <keyframe-migration-backups/video-ID-SHA256.json>
```

復元対象は **キャッシュ2列だけ**であり、DB全体や録画メディアのバックアップではない。
元のファイル名と内容を保持する。SHA256、録画ID/path/hash、Recorded状態、移行後キャッシュを検証し、後続のキャッシュ更新や録画中の行は上書きせず拒否する。バックアップ内容の改変も拒否する。
`-migrate [-backup-dir <保存先>]` で既存DBの明示移行も可能。実DBへの実行は別途の保守計画で判断し、今回は合成DBだけで検証した。
配布する場合は補助ツールもパッケージに含めるか、同等の復元手順を用意する必要がある。既存 installer ファイルは変更していない。

## 未実装と未検証の区別

今回の調査範囲で、Go単独の通常経路を常に失敗させる未実装を追加で特定していない。115件のGo登録/分類検査は維持し、フル互換の証明とはしない。

| 項目 | 状態 |
| --- | --- |
| EDCB 未同梱ロゴ取得 | 今回実装済み。mock/実wire peer検証、実EDCBは未検証 |
| 旧key_frames自動移行 | 今回実装済み。自動配線、成功/修復/失敗/復元/再実行を一時DBで検証 |
| 動的 analytics filter 取得 | 追加 filter 取得は未移植。固定 patterns の hook を使う。通常 GraphQL/CDP 操作の実装自体はある |
| join_logo_scp 自動CM検出 | Python側も未実装。既存チャプター解析はGo実装済み |
| 実Twitter/Bluesky/Nico等の認証・Web App変化 | 実装あり、実サービス受入れ未実施。実アカウントは未操作 |
| EDCB/Mirakurunライブ・予約、GPU、NAS/SMB、全録画形式 | 実装あり、全条件の実機受入れは未実施。第4の監視安全制約を維持 |
| 未登録encoder/offline/thumbnail factory時のエラー | 依存注入なしの防御経路。通常main/APIはGo実装を登録する |
| 既定logo asset欠落時の501 | 異常な配布/配置。静的assetを含む packaging 検証が必要 |

旧明示 proxy 構成の3転送分岐と未知API catch-allは削除していない。Go経路が存在し、`-no-proxy` は既定true、Python URLは既定空。起動flagのASTテスト、115 route AST検査、既存のnative/legacy peerテストで保護する。

## 検証・配布評価

ログは task-4 の `phase5-targeted-tests.txt`、`phase5-final-targeted-tests.txt`、`phase5-final-cache-safety-tests.txt`、`phase5-final-cache-tests.txt`、`phase5-linux-migration-tests.txt`。
全Goテストとvetは `phase5-all-tests.txt` / `phase5-vet.txt`、最終再実行は `phase5-final-all-tests.txt` / `phase5-final-all-vet.txt`。最後のRecorded復元保護は対象テスト/CLIテストでも再確認する。
全 suite は2回成功 (API124.142秒 / 最終133.478秒)、全 vet 成功。Recorded状態・チェックサム・極小fpsの最終保護も対象テスト/CLIテスト/vetで成功 (`phase5-final-cache-safety-tests.txt`、`phase5-latest-safety-tests.txt`、`phase5-latest-vet.txt`)。Linux の同期/移行/復元/自動スキャン配線も隔離テスト成功。本番データを使っていない。
長時間fixture初回は30fpsを厳密6秒と仮定して失敗。既存のPython互換正規化は30を30000/1001へ寄せるため、厳密6秒となる25fpsへfixtureを修正し通過した。正規化仕様は変更していない。

評価は引き続き **既知制約を明示する限定ベータ候補**。installer統合後の全テスト、asset/外部ツール/復元ツール同梱と初回起動検証が必要。フル互換・実機検証済み・本番置換承認済みとはしない。
今回メモリの実再生再測定はしていない。第2以降の「実再生PSS改善未確認」を維持する。

統合境界: runtime `main.go` / `internal/api/server.go` / config / installer の追加変更なし。新規 `cmd/keyframe-cache`、EDCB logo、metadata cache移行、plannerの移行用重複判定とテストが追加差分の中心。

保守バイナリ (task-4直下、公開なし):

- `keyframe-cache-windows-amd64.exe`: SHA256 `97B63178C83F3C0205CB910A1F3622EA21E7A64D44817AD371034C98B49D468E`
- `keyframe-cache-linux-amd64`: SHA256 `AC3A42014F3CFE79E0FF4F9378AC334936257930AC41C59315822AE9A37768A5`
