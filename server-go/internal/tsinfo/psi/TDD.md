# PSI 独立レビュー修正 — TDD 検証記録

編集・実行範囲: `internal/tsinfo/psi/` のみ。標準ライブラリ・独自 synthetic fixture のみ。実番組 TS/EPG/元 fixture の持ち込みなし。Go 1.26.2 windows/amd64 (`C:/Program Files/Go/bin/go.exe`)。

## 手順

1. scratch の `review_regression_test.go` / `review_recovery_test.go` / `review_table_constraints_test.go` を読んで公開契約違反を確認。
2. 回帰を repository に機能 slice ごとに移入/補強。
3. **実際に repository の対象 test を実行し、意図した assertion FAIL を確認してから** production を最小修正。
4. 各 GREEN で同 package 全 unit を再実行。公開 func/type/fields/sentinel の変更なし。

修正前 baseline は `go test ./internal/tsinfo/psi -count=1` exit 0。既存 suite の PASS だけでは今回の回帰は検出できなかった。

## 実 RED

件数は `--- FAIL: Test...` / `--- PASS: Test...` のトップレベルのみ（subtest 重複を含めない）。各対象コマンドは `go test ./internal/tsinfo/psi -run '<regex>' -count=1 -v`。

| RED slice | exit | FAIL | PASS |
| --- | --- | ---: | ---: |
| PCR RED | 1 (FAIL) | 3 | 0 |
| multiple errors RED (compiled) | 1 (FAIL) | 6 | 0 |
| malformed packet RED | 1 (FAIL) | 3 | 0 |
| reader error RED | 1 (FAIL) | 3 | 1 |
| PAT duplicates RED | 1 (FAIL) | 1 | 0 |
| PAT length RED | 1 (FAIL) | 1 | 0 |
| PMT section numbers RED | 1 (FAIL) | 1 | 0 |
| PMT length RED plus EIT/PID guard | 1 (FAIL) | 1 | 2 |
| joined EOF reader error RED | 1 (FAIL) | 1 | 0 |
| joined EOF reader RED with compatibility guard | 1 (FAIL) | 1 | 1 |

- PCR: updated-PCR duplicate が `ErrContinuity`、pending が消えて continuation の section=0。非 PCR bytes 差分 case でも ErrCRC/ErrSection が ErrContinuity を上書き。
- 複合 error: 二つの不正 CRC sections が InvalidSections=1。gap+CRC が ContinuityErrors=0。pointer 前の CRC/未完と次 section の CRC が一件に潰れ、CRC 後の overlong header も前の error を消していた。
- 不正 packet: 3/4/187/189 bytes で PID=0。PID 0x123 の短 packet 後に continuation が section を誤 emit。識別不能 header でも他 PID の pending/PCR が残った。
- reader: bytes+error を一回の Read で観測したのに MaxPackets/MaxSections で nil。visitor/context と同時観測した reader error も失われた。
- table: program 0 を含む PAT 重複、PAT section_length=1025、PMT section numbers 0/1・1/1、PMT section_length=1022 を受理。

`multiple errors RED` の初回は write_file の安全な overwrite 拒否により未使用 import の **build failure**。これは RED 証拠に数えず、patch で test 追加完了後に再実行した `multiple errors RED (compiled)` の assertion FAIL を確認してから修正した。編集 tool の単独 .go vet 警告は他ファイルの型/helper を見ないためのもの。同 package の実 test/vet は GREEN。

重複を除いた実 RED トップレベル回帰: **19 tests**（同じ PCR negative test の再 FAIL は二重計上しない）。

## GREEN と修正方針

- PCR: 検証済 packet の同 CC 再送を全 bytes 比較し、有効 PCR 6 bytes だけの差分も許可。payload/CC/pending/元 snapshot は不変で新 PCR のみ時計へ反映。PCR flag/構造/stuffing/OPCR/payload 不一致は認めない。
- ParsePacket/Push: 0x47 と PID の 2 bytes が読めれば不正長でも PID/Offset/Raw を返す。壊れた PID の pending/clock を ResetPID。他 PID は維持。sync/3 bytes がない場合は全 Reset。
- Scan: 終了処理で観測済 reader error を保存。context/visitor error を primary として Join、全 error の `errors.Is` を維持。limit は error を消さず Stats.LimitReached と共存。visitor が error と cancel を同時に返す時は visitor が優先。EOF+bytes は正常終了。追補 RED で `errors.Join(io.EOF, failure)` も nil に潰れることを再現し、EOF-only error tree の判定で非 EOF sibling を保存（`isOnlyEOF` は私有）。
- 複合 error/count: `errors.Join` を各 framing/CRC event ごとに積み、私有 `assemblyProblemCounts` が Unwrap tree の leaf のみ加算。親 Join の再加算や同 sentinel の潰し込みなし。公開追加 type/field は不要。
- PAT/PMT: 個別 decoder で 1021 section_length、PAT program_number uniqueness、PMT section/last=0。汎用 ValidateSection/Assembler の 4096 枠を維持。

追加契約 guards: 同 CC conflict+二つ CRC で continuity=1/invalid=2、nested/wrapped error tree leaf count、malformed PCR flag/extension、snapshot 独立所有、次の valid section 復帰、PAT/PMT valid 最大境界、4096-byte EIT decode/assembly、8192 PID pending bound。

cycle 1 の子検証 unit: **57 PASS / 0 FAIL**（トップレベル Test）。subtests **31 PASS / 0 FAIL**。既存+追加 fuzz は seed mode **5 targets / 17 seeds PASS**。`TestReview*` は **29 PASS / 0 FAIL**（subtests 15 PASS）。各 GREEN の package suite も exit 0。ただしその後の fresh 独立レビューで filtered PCR 再送と100回目の空 Read 内 cancel の契約違反が実再現され、cycle 1 は不合格。子の suite PASS は受入れを意味しない。

`go vet ./internal/tsinfo/psi`: exit 0、出力なし。
`gofmt -l internal/tsinfo/psi/*.go`: exit 0、未整形ファイルなし。
編集した production の公開宣言・sentinel 値を変更前 snapshot とプログラムで比較: **54 declarations checked / diff 0**。

## 状態付き sequence fuzz

`review_sequence_fuzz_test.go` に一般 multi-packet sequence、PCR-only 重複/CC wrap/PCR wrap、破損長 PID/全 reset と PUSI 復旧の 3 targets を追加。同じ assembler に複数 packet を渡すため、単発 fuzz が見逃した pending/clock/duplicate 回帰を確認できる。

各コマンド: `go test ./internal/tsinfo/psi -run '^$' -fuzz '^<target>$' -fuzztime=3s -parallel=1`。以下は実測。一般 byte mutation は短時間で深い状態へ届きにくいため、専用 sequence properties を別 target にした。

| target | 指定時間/並列 | 実 execs | 結果 |
| --- | --- | ---: | --- |
| `FuzzPacketSequence` | 3s / 1 worker | 9 | PASS / exit 0 |
| `FuzzPCRDuplicateSequence` | 3s / 1 worker | 19952 | PASS / exit 0 |
| `FuzzPacketRecoverySequence` | 3s / 1 worker | 27576 | PASS / exit 0 |

## 再実行

```bash
"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/psi -count=1 -v
"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/psi -run '^TestReview' -count=1 -v
"C:/Program Files/Go/bin/go.exe" vet ./internal/tsinfo/psi
"C:/Program Files/Go/bin/gofmt.exe" -l internal/tsinfo/psi/*.go
"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/psi -run '^$' -fuzz '^FuzzPacketSequence$' -fuzztime=3s -parallel=1
"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/psi -run '^$' -fuzz '^FuzzPCRDuplicateSequence$' -fuzztime=3s -parallel=1
"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/psi -run '^$' -fuzz '^FuzzPacketRecoverySequence$' -fuzztime=3s -parallel=1
```

## 最終 cycle 2 — filtered PCR / no-progress context の2根因のみ

scope は `assembler.go` / `scan.go` / 新規 `review_filtered_pcr_test.go` / 新規 `review_no_progress_test.go` / `API.md` / `TDD.md`。元の独立 review scratch は read-only のまま。`independent_cycle1_test.go` の `TestIndependentPCRDuplicateOutsideSectionFilter`、`independent_filter_repro_test.go` の epoch test、`independent_extra_negative_test.go` の Scan/context tests を読んで synthetic 回帰を移入した。

1. PCR slice: `haveCC/cc/lastPacket` の保存前に filter 外 PID から return していたため、同 CC retransmission が判別できず、byte-identical 再送が新しい PCR 観測になった。同 discontinuity flag の PCR-only 更新でも duplicate=false のため epoch がリセットされた。RED の実値は offset **188（期待0）**、更新後 ticks **302（期待2576980377902 = PCRModulus+302）**。Scan は chunks **1/7/187/188/376/制限なし** すべてで同じ epoch 不一致を再現。production は payload bookkeeping を filter return の前に維持し、continuity error の判定と section parsing だけを `selected` に限定した。duplicate classifier / discontinuity / PCR 更新 / adaptation-only のロジックと公開 API は変更していない。
2. no-progress slice: 100回目の `Read` が cancel して `(0,nil)` を返した時、直接 `io.ErrNoProgress` を返していた。RED は `calls=100` / 全 stats=0 / `multiple Read calls return no data or error`（期待 context canceled）。threshold 終了の直前だけ `ctx.Err()` を確認し、cancel なら既存 `finish(err)` へ。無 cancel の対照は RED 前後とも `io.ErrNoProgress` のまま。finish/reader/visitor/EOF/limit の契約拡大なし。

以下は実行 stdout をプログラムで集計（トップレベル `Test` と `Fuzz` を分離）。PCR の RED→最小修正→package GREEN を完了してから no-progress の RED→最小修正→package GREEN を実行した。guards は既存契約の対照で、PASS を RED の証拠には数えない。

| slice / command | exit | top-level Test PASS / FAIL | fuzz target PASS / FAIL |
| --- | ---: | ---: | ---: |
| PCR RED (`-run '^(TestReview(PCRDuplicateOutsideSectionFilter\|FilteredPCR.*)\|FuzzFilteredPCRDuplicateSequence)$' -count=1 -v`) | 1 | 2 / 3 | 0 / 1 |
| 同 PCR GREEN | 0 | 5 / 0 | 1 / 0 |
| PCR package GREEN (`-count=1 -json`) | 0 | 62 / 0 | 6 / 0 |
| no-progress RED (`-run '^TestReview(ContextPriorityAtNoProgress\|NoProgressWithoutCancellation)$' -count=1 -v`) | 1 | 1 / 1 | 0 / 0 |
| 同 no-progress GREEN | 0 | 2 / 0 | 0 / 0 |
| 最終 package (`-count=1 -json`) | 0 | **64 / 0** | **6 / 0** |

最終 package の subtests は **42 PASS / 0 FAIL**、fuzz seed mode は **19 seeds PASS / 0 FAIL**。PCR RED の subtests は1 PASS / 10 FAIL、property seeds は0 PASS / 2 FAIL。新しい5 PCR testsと2 no-progress tests以外の既存 tests も全件保持して実行した。filter 外 section の malformed pointer/CRC/途中 section は解析/保持/emit せず、CC gap も計上しない guard、nil/filtered/空 filter、PCR value wrap、PUSI snapshotの保持、adaptation-only 同 CC（byte-identicalでも毎回正規観測）の guards を含む。

`FuzzFilteredPCRDuplicateSequence` は一つの assembler へ original→identical→section開始→PCR-only更新→identical更新→continuation→新PUSI を投入し、nil/filtered/空 filterを比較。PCR wrap/extension/CC wrap/discontinuity/PUSIの組合せと offset 不変を検証する。`-run '^$' -fuzz '^FuzzFilteredPCRDuplicateSequence$' -fuzztime=3s -parallel=1` は **14980 execs / PASS / exit 0**（2 seed baseline、new interesting=0）。

`go vet ./internal/tsinfo/psi`: exit 0、stdout/stderrなし。scope の4 Go filesに `gofmt -w`、同 package の `gofmt -l` はexit 0/出力なし。変更した2 production filesの公開func/type/fieldを変更前 snapshot と比較し、**11 declarations checked / 変更0**。`errors.go` も変更前hashと一致、sentinels維持。単独ファイル editor vet の sibling symbol undefined は package のREDではなく、実 test/vet は成功。

子証跡は私有 scratch の `psi-cycle2-proof/` に保存: `pcr_red.out` / `pcr_green.out` / `pcr_package_green.out` / `no_progress_red.out` / `no_progress_green.out` / `final_package_green.out`（JSONL）/ `filtered_pcr_fuzz.out` / `vet.out` / `gofmt.out` / `summary.json`。個人の絶対パスや実番組データは repository に含めない。専用scratchは元reviewと別で、元reviewを修正/再実行していない。子の実測のみでは受入れとしない。

最終受入れ: 親が repository の **64 unit / 6 fuzz seed targets PASS** と vet / gofmt を再確認し、fresh 独立レビューも security / logic concerns が空で合格。独立コピーの追加 probe を含む **78 unit / 8 fuzz seed targets PASS** を親が再実行した。29 repository files の review manifest と SHA-256 が一致することを確認してから、この検証記録の個人パス除去と受入れ記述だけを更新した。受入れ範囲は PSI package の synthetic 契約のみで、metadata adapter、全体 integration、実機配布の合格を意味しない。

## 検証の限界

- synthetic のみ。実放送 TS 相互運用性、metadata adapter 接続、cmd/全体 build は未実行。
- fuzz は各 3s の短時間。すべての packet sequence の正しさを証明しない。
- blocking Read 自体は中断不可。未観測 reader error、複数窓の state/clock 接続、table/version 集約は呼び出し側の責務。
- PAT/PMT 以外の規格 tightening、reserved fields 拡張、ARIB 全角符号方針は対象外。
- git commit/push/reset/checkout/stash/clean、実機 service/予約停止などは行わない。
