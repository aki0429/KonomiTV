# KonomiTV Go 並行版: catalog-only refresh

## 成果物・範囲

標準ライブラリのみの `refresh_catalog.py`、合成SQLiteだけを使う `test_refresh_catalog.py`、同じディレクトリ配置用の互換 `refresh_db.sh`。本番DBを変更せず、並行版の録画カタログを更新するための運用ツール。検証済みであることと実機へ適用済みであることは区別する。

既定sourceは `/opt/KonomiTV/server/data/database.sqlite`、destinationは実行ユーザーの `~/konomitv-go/server/data/database.sqlite`。必ず **akki** として実行する。root側cutover scriptへは配置しない。

## 安全設計

- sourceは `mode=ro` と `query_only` で開く。SQLite backup APIを用いてWALのコミット済みデータを含む一貫したstagingを作り、`integrity_check == ok` を要求する。稼働中WALでの `immutable=1` や単純cpは使用しない。
- source/destinationは `os.path.samefile` で同一inodeを拒否する。同path、symlink、hardlinkとも対象で、identityを取得できなければfailclosed。最初のlock/staging前と、停止・書込前（rollback含む）に確認し、`--check` でも拒否する。
- 同期対象は `channels`, `programs`, `series`, `series_broadcast_periods`, `recorded_programs`, `recorded_videos` の6テーブルだけ。全列・全行を型/長さつきSHA-256でfingerprintする。statusだけの変更、NULL/BLOB/実数、重複行も反映する。row順序には依存しない。
- CREATE TABLE SQL、table_xinfo、FK、index metadata/SQL、trigger metadataを厳密比較する。schema差は変更無しでもfailclosed。カタログ上のtriggerは副作用を避けるため一致していても拒否する。
- destinationのschema、全テーブルのFK検査、6テーブルfingerprintは単一のread transactionから取得する。変更無し判定も同じviewを使い、FK違反は内容一致でも拒否する。正常に内容が一致すればPM2を一度も呼ばず、destination DBを変更しない。
- 変更時はsource snapshot、destination previous backup、merge候補のtransaction/integrity/FK検証を**全て停止前**に完成させる。停止後にフルbackupしない。
- destinationを置き換えず `BEGIN IMMEDIATE` でカタログ6テーブルだけDELETE/INSERTする。FK cascadeはこの専用connectionのみ無効化し、commit前に**全テーブルの**foreign_key_checkで参照整合性を検証する。sqlite authorizerでlocal-owned tableのDMLを拒否する。
- users/account links/twitter/bluesky/captures/capture_bookmarksその他未知テーブルには書き込まない。停止直前のlocal書込も現DBに残る。sourceの認証テーブルをdestinationへ上書きしない。カタログ更新中にlocal参照先が消える場合はfailclosedし、bookmark/captureをcascadeで消さない。
- 稼働済みの `konomitv-go` のみ更新する。PM2 stopのexit codeに加えて `jlist` の `stopped`/PID=0を確認。停止後、transaction内でschema/fingerprintを再照合し、カタログの競合書込があれば中止する。
- previous backupのschema/fingerprintも書込前の `BEGIN IMMEDIATE` 内でexpectedと照合する。`current == expected == previous` が成立しなければ、ABAでliveが元に戻っていても書き込まない。rollback時も**実際にINSERTするattached previousの同一transaction view**をexpected baselineと比較し、途中で変わったbackupは使わない。
- start成功だけでは完了とせず、PM2 online/PIDと `http://127.0.0.1:7002/api/videos?page=1` のHTTP 200を確認。start/verification失敗は再停止確認後、**カタログだけ**previousへrollbackする。初回再起動中のlocal user/capture書込もフルDB復元で消さない。
- COMMIT所有の根拠はmemory trackerの3状態（`before_commit` / `unknown` / `committed`）だけ。COMMIT呼出前にunknown、正常return後にcommittedを記録する。COMMIT前の競合失敗ではcatalog rollbackを行わずサービスだけ復帰する（`rollback_ok=true` はrollback不要も含む）。COMMIT結果不明では書込rollbackせずpending/backupを保持する。`contents == incoming` は別プロセスも作れるので所有証拠にしない。永続witness tableはmigrationを壊すため追加しない。
- live DB/WAL/SHMファイルをunlink/rename/replaceしない。SQLite自身のtransaction rollbackを使う。
- destination横の `.catalog-refresh.lock` にLinux flock / Windows msvcrtの非blocking lockを保持する。lock inodeは削除しない。二重実行はexit 75。
- `.catalog-recovery/previous.sqlite` と `pending.json` を停止前に作成。復旧まで完了した時のみstagingを削除する。復旧失敗・強制終了後は既存recoveryによりcronをfailclosedさせる。
- stdoutは状態/固定error codeのみ。DB行、認証情報、PM2出力、HTTP body、例外本文をloggingしない。stagingディレクトリは0700、互換shellはumask077。backupはprivateデータを含むためrepo/添付/commit禁止。

## ローカル単体テスト

リポジトリの `server-go/tools/catalog_refresh/` を作業ディレクトリにする。
Windowsは `python`、Linuxは `python3` を使う。

```bash
python -B -X utf8 -m unittest -v
```

合成SQLite fixtureとinjectable FakeRunner/health-checkだけを使う。PM2や実機不要。テストのtemporary directoryは同じツールディレクトリ配下に限定し、テスト終了時に破棄する。

TDD: no-change未実装、status-only未更新、schema未拒否、restart verification未rollback、source failureの未sanitization、capture_bookmarks参照で停止前failclosedしていない、catalog triggerによるlocal消失、停止中競合、二重実行、偽stop成功、stagingを停止後に作る、復旧失敗でbackup消失、部分stop失敗、意図的stopから勝手にstart、KeyboardInterrupt、local DMLガード、read-only CLIを、それぞれ失敗出力確認後に修正した。補助回帰テストはtransaction失敗、WAL未commit除外、AUTOINCREMENT、generated columnも確認する。

今回の独立レビュー5問題は、repo版に合成回帰を先行追加し、各RED実行後に個別修正した。元scratch版・実機版・Go・互換shellは編集していない。

| 問題 | 修正前の実出力（対象テストのみ） | 直後の全suite GREEN実出力 |
| --- | --- | --- |
| 同path/hardlink/identity不明（refresh/check両方） | `Ran 4 tests` / `FAILED (failures=6, skipped=1)` | `Ran 33 tests` / `OK (skipped=1)` |
| 外部incoming誤rollback、COMMIT前後の結果不明 | `Ran 3 tests` / `FAILED (failures=3)` | `Ran 38 tests` / `OK (skipped=1)` |
| previousのABA/停止中変更 | `Ran 2 tests` / `FAILED (failures=2)` | `Ran 40 tests` / `OK (skipped=1)` |
| WAL書込のtable間torn read、schema/hash異view | `Ran 2 tests` / `FAILED (failures=2)` | `Ran 42 tests` / `OK (skipped=1)` |
| unchanged/checkのFK違反（ID999、local captureも） | `Ran 5 tests` / `FAILED (failures=5)` | `Ran 47 tests` / `OK (skipped=1)` |

追加の停止前/停止中alias（`Ran 2 tests` / `FAILED (failures=2)`）、COMMIT後previous変更とrollback前alias（各 `Ran 1 test` / `FAILED (failures=1)`）もRED→GREENを確認。最終Windows再検証の実出力は `Ran 49 tests in 24.721s` / `OK (skipped=1)`。symlink fixtureだけWindows権限不足（winerror 1314）でskipし、実hardlink・同pathは実行した。修正版のLinux合成テストと新しい独立レビューは親の別工程。

## 親が行うread-only検証ハンドル

配置後、akkiの環境で次を実行する。`--check` はサービスを止めず実行できる。

```bash
python3 /home/akki/konomitv-go/refresh_catalog.py \
  --source /opt/KonomiTV/server/data/database.sqlite \
  --destination /home/akki/konomitv-go/server/data/database.sqlite --check
```

`--check` は同源を拒否し、DBをreadonly transactionで開き、schema/integrity、**全テーブルのforeign_key_check**とカタログ内容を照合する。PM2、destination write、staging、lockを作らない。正常時の出力は `unchanged` 又は `change_available` とchanged table名のみ。各DBは別々の時点の一貫したread snapshotなので、稼働中のsource更新との差を完全な同時刻一致とは解釈しない。read-only SQLiteでもWAL共有メモリの取扱はSQLite/OSに依存する。

親の配置先候補は既存cron入口 `/home/akki/konomitv-go/refresh_db.sh` とその隣のPython。guard解除/cron再開は**親のレビュー・実機readonly確認後のみ**。旧shellへPython内容を埋め込まない。

## 失敗・手動復旧の限界

- schema比較は安全優先でSQL文字列表現まで厳密。同値でも書式が違えば拒否する。schema migration/trigger互換の自動修復はしない。
- capture_bookmarks等の既存FKが、新sourceカタログにないIDを参照していれば更新を拒否する。FK宣言のない論理参照は推測・修復できない。
- SHA-256比較は暗号学的衝突の可能性をゼロにはしない。row hashのsort用メモリは最大テーブルの行数に比例し、フルbackupもDB全体分の空き容量が必要。
- source高頻度更新/ストレージ異常時のbackupにはSQLiteの再試行が入り得る。全体の硬い実行時間上限は未実装。cronの二重更新はlockで阻止する。
- SIGTERM/強制終了/電源断/kill -9では自動的にサービスを復帰させることは保証できない。memory trackerは永続のCOMMIT証拠ではなく、COMMIT直後の割込も結果不明として手動復旧に回す。SQLiteのatomic transactionと停止前backup/markerが復旧材料。markerのfsyncは行うが、directoryのfsyncやOS/機器障害全体への保証はない。
- samefile照合は停止・書込前の境界検査であり、悪意あるfilesystem inode差替えとの無制限なTOCTOU競合を保証しない。運用中はsource/destination/stagingを外部からrename/replaceしないこと。stagingもprivateディレクトリ内で維持する。
- 再起動後のlocal参照が旧カタログと不整合になる場合、rollbackもFKチェックで拒否し、localデータと現カタログを保持して手動復旧を要求する。新しいlocal行を消すための強制rollbackはしない。
- 復旧失敗で残った `database.sqlite.catalog-recovery/` は自動削除しない。親がservice状態/marker/現在のreadonly fingerprint/previous backupを確認すること。バックアップ**全体**をlive DBに戻すと再起動後local書込を失うため禁止。必要な復旧は停止確認後 `sync_catalog` 相当のカタログ限定transactionで行い、schema/FK整合性が成立しない場合はfailclosedを維持する。marker解除は親が正常状態を確認してから。
- 今回実行したのはWindowsの合成SQLite単体テストのみ。本番DB/認証情報は読まず、実PM2停止・復帰や適用は行っていない。親の新しい独立レビュー・Linux合成テスト・実機read-only確認後まで旧scriptのpause/適用guardを維持する。fake成功を運用適用成功と扱わない。