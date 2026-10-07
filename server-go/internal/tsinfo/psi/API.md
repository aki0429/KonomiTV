# psi 公開 API / MPEG-TS 録画解析契約

この独立パッケージは標準ライブラリだけを使用する。ARIB 文字列は `[]byte` のまま返し、metadata / arib パッケージには依存しない。以下は親 ProgramAnalyzer 向けの公開契約。第三者の実装コードをコピーせず、MPEG-TS / PSI のフィールド定義に基づき独自実装・独自 synthetic fixture で検証する。

## パケット・セクション

- `const PacketSize = 188`, `MaxSectionSize = 4096`（3-byte header と CRC を含む総長）
- `ParsePacket(raw []byte, offset int64) (Packet, error)`
- `Packet`: `PID uint16`, `Offset int64`, `PUSI bool`, `ContinuityCounter uint8`, `HasPayload bool`, `Discontinuity bool`, `Payload []byte`, `PCR *PCR`, `Raw []byte`。Raw/Payload は入力を参照する。188 bytes、sync、TEI、scrambling、adaptation control/length、optional adaptation fields の長さと PCR を検証。エラー時も Offset/Raw を保持し、sync=0x47 と先頭 3 bytes があれば不正長（188 未満/超過）でも PID を返す。sync 不一致または 3 bytes 未満では PID は不明（ゼロ値）で、Push は caller の Packet.PID を信用せず全 state/PCR を Reset する。
- `PCR{Base uint64, Extension uint16}`、`(PCR).Ticks() uint64`（27 MHz）、`PCRDelta(from, to PCR) uint64`（33-bit base wrap の前進差）。`PCRModulus = (1<<33)*300`。
- `PCRSnapshot{PID uint16, PacketOffset int64, Value PCR, UnwrappedTicks uint64}`。不連続後は別の epoch とし初期値から再開する。
- `NewAssembler(opts AssemblerOptions) *Assembler`、`(*Assembler).Push(packet Packet) ([]Section, error)`、`Reset()`、`ResetPID(pid uint16)`。Push は Packet.Raw と Offset を正としてパケットを再検証する（手作り Packet で検証を回避できない）。
- `AssemblerOptions{PIDs []uint16, PCRPID *uint16}`。nil PIDs は全 PID、空の非 nil slice はセクションを取得しない。PCR は全 PID から追跡し、PCRPID 指定時はその PID のみ snapshot に使う。payload の同 CC 再送判定とその CC/前 packet の記録も PIDs filter と独立に維持する。filter 外の section は解析/emit せず、filter 外の CC gap/競合再送は `ErrContinuity` / `ScanStats.ContinuityErrors` に計上しない。adaptation-only は payload 再送の重複判定対象にせず、同 CC / byte-identical でも正規の PCR 観測として更新する。
- `Section{PID uint16, PacketOffset int64, PCRAtPUSI *PCRSnapshot, Raw []byte}`。PacketOffset は開始 bytes を含む PUSI packet の絶対 byte offset。Raw と snapshot は独立所有。pointer による前セクション完了は元の offset/PCR を保ち、同じ PUSI payload 内の各新セクションは同じ snapshot を持つ。
- PUSI pointer による前セクション完了・複数セクション・header 分割に対応。continuation だけから新セクションを開始しない。0xff stuffing は無視する。
- Payload の CC は mod16 increment、adaptation-only は increment 不要。byte-identical 同 CC の再送、および有効 PCR の 6 bytes（Raw[6:12]）だけ異なる同 CC 再送を重複として扱う（ITU-T H.222.0 §2.4.3.3）。PCR flag・adaptation length/flags・その他の全 bytes は一致必須。PCR だけ更新された再送は payload を二重投入せず、保留 section/開始時 snapshot/CC を維持して最新 PCR clock だけ更新する。byte-identical 再送は新しい clock 観測としない。同 CC の異なる packet / CC gap は保留セクションを破棄し `ErrContinuity`（新しい PUSI から復帰可能）。discontinuity は期待 CC / 保留セクション / 当該 PID の PCR epoch をリセット。TEI・scrambled・壊れた packet の後も scanner は当該 PID をリセット。全体の同期喪失 / 窓移動では全 state をリセット。
- `CRC32MPEG2(data []byte) uint32`（poly 0x04c11db7、init 0xffffffff、non-reflected、xorout 0）、`ValidateSection(raw []byte) error`。長さ一致 / 4096 上限 / syntax header / CRC を検証。syntax sections と TOT は CRC 必須。unknown non-syntax section は長さのみ検証。無効 section は emit しない。
- Error sentinels: `ErrPacket`, `ErrTransport`, `ErrScrambled`, `ErrContinuity`, `ErrSection`, `ErrCRC`, `ErrUnsupportedTable`, `ErrLimit`, `ErrOptions`。`errors.Is` で識別可能。Push は復旧可能な error と、その packet で得られた有効 Sections を同時に返すことがある。複数の問題は `errors.Join` で保持し、continuity + CRC/framing の両方を `errors.Is` で識別可能。同じ sentinel も失敗した section ごとに別 leaf として保持する。単一 sentinel との `==` 比較でなく `errors.Is` を使う。

## テーブル decode

`DecodeSection(section Section) (any, error)` は `*PAT`, `*PMT`, `*SDT`, `*NIT`, `*EIT`, `*TOT` を返す。CRC/長さを再検証し、unknown table は `ErrUnsupportedTable`。個別 `DecodePAT`, `DecodePMT`, `DecodeSDT`, `DecodeNIT`, `DecodeEIT`, `DecodeTOT` はそれぞれ `(Section) (*Type, error)`。Raw の所有権が必要なら Section を独自保持すること（各 table の Section.Raw は入力 Section と共有）。

- `Descriptor{Tag uint8, Data []byte}`。`DecodeDescriptors(raw []byte) ([]Descriptor, error)`。順序・未知 tag・重複を保持し、Data は入力からコピー。既知 descriptor の便利フィールドも raw descriptor を失わない。
- `TableHeader{TableID uint8, Extension uint16, Version uint8, Current bool, SectionNumber uint8, LastSectionNumber uint8}`。
- PAT (0x00): `PAT{Section, Header TableHeader, TransportStreamID uint16, Programs []PATProgram}` / `PATProgram{ServiceID uint16, PMTPID uint16}`。program 0 は `NetworkPID *uint16` として分離。同一 section 内の `program_number` 重複は program 0 を含めて拒否（同一 PID への重複も不可）。`section_length <= 1021`（総長 <= 1024 bytes）必須。別 section/version 間の集約・重複判定は親側。
- PMT (0x02): `PMT{Section, Header, ServiceID uint16, PCRPID uint16, Descriptors []Descriptor, Streams []ElementaryStream, VideoPIDs []uint16, AudioPIDs []uint16}` / `ElementaryStream{StreamType uint8, PID uint16, Descriptors []Descriptor}`。video types 0x01/02/10/1b/24/42、audio 0x03/04/0f/11/81/87 を分類。private 0x06 は AC-3/EAC3/DTS/AAC descriptor (0x6a/7a/7b/7c) または registration AC-3/EAC3/DTS1/2/3 で分類。未知 ES も Streams に保持する。`section_number=last_section_number=0` と `section_length <= 1021`（総長 <= 1024 bytes）必須。PAT/PMT の規格制約は各 decoder だけで適用し、`ValidateSection`/Assembler の 4096-byte 枠や EIT を狭めない。
- SDT actual (0x42): `SDT{Section, Header, TransportStreamID uint16, OriginalNetworkID uint16, Services []Service}` / `Service{ServiceID uint16, EITSchedule bool, EITPresentFollowing bool, RunningStatus uint8, FreeCA bool, Descriptors []Descriptor, ServiceType uint8, ProviderName []byte, ServiceName []byte}`。service descriptor 0x48 のみ便利フィールド化。
- NIT actual (0x40): `NIT{Section, Header, NetworkID uint16, Descriptors []Descriptor, TransportStreams []TransportStream}` / `TransportStream{TransportStreamID uint16, OriginalNetworkID uint16, Descriptors []Descriptor, RemoteControlKeyID uint8, HasRemoteControlKeyID bool, Name []byte}`。TS information descriptor 0xcd のリモコン・TS名と内包 loop 長を検証。
- EIT p/f actual (0x4e): `EIT{Section, Header, ServiceID uint16, TransportStreamID uint16, OriginalNetworkID uint16, SegmentLastSectionNumber uint8, LastTableID uint8, Events []Event}` / `Event{EventID uint16, StartTime time.Time, StartTimeValid bool, Duration time.Duration, DurationValid bool, RunningStatus uint8, FreeCA bool, Descriptors []Descriptor}`。未確定/不正時刻は bool=false で保持し、イベントそのものを失わない。
- TOT (0x73): `TOT{Section, Clock time.Time, ClockValid bool, Descriptors []Descriptor}`。放送の MJD+BCD 値を JST の wall clock として扱う（UTC からの +9h 変換ではない）。
- `DecodeJSTTime(raw []byte) (time.Time, bool)` は正確に 5 bytes MJD+HHMMSS、`DecodeDuration(raw []byte) (time.Duration, bool)` は正確に 3 bytes HHMMSS。全 0xff は未確定、不正 BCD / 時分秒範囲は false。時刻 hours 0..23、duration hours 0..99、minutes/seconds 0..59。JST 固定 +09:00、MJD epoch 1858-11-17。false はゼロ値。

## bounded scan

- `Scan(ctx context.Context, r io.Reader, opts ScanOptions, visit func(Section) error) (ScanStats, error)`
- `ScanWindow(ctx context.Context, r io.ReadSeeker, offset int64, opts ScanOptions, visit func(Section) error) (ScanStats, error)` は absolute Seek(offset, io.SeekStart) を行い、offset を Section に反映する。任意 byte offset から sync recovery する。窓ごとの独立 state（別窓の CC/PCR を繋がない）。
- `ScanOptions{MaxBytes int64, MaxPackets int64, MaxSections int64, PIDs []uint16, PCRPID *uint16}`。MaxBytes>0 は必須。MaxPackets/MaxSections は 0=無制限（MaxBytes は常に有限）、negative は ErrOptions。
- `ScanStats{BytesRead int64, Packets int64, Sections int64, InvalidPackets int64, InvalidSections int64, ContinuityErrors int64, DiscardedBytes int64, LimitReached bool}`。エラー未観測での上限到達は正常終了 (nil error + LimitReached=true)。reader/callback/context errors は返す。EOF だけの error tree（wrapper/複数 EOF Join も含む）は正常終了。`errors.Join(io.EOF, failure)` の非 EOF leaf は無視せず元の error tree を返す。`InvalidSections` は packet 数でなく検証失敗した section 数（CRC/framing 各 leaf、および pointer overflow/前 section 未完の framing event は各 1）。`ContinuityErrors` は選択 PID の payload CC gap/競合再送 event 数で、同 packet の section error と独立に数える。gap で破棄した保留 section や窓末尾の未完 section は別に InvalidSections に加算しない。`Sections` は visitor が nil を返して受理した数。最後の不完全 packet/section は emit しない。
- 読み込みは MaxBytes を決して超えず、保持メモリは読み込み buffer / PID state / 各 4096-byte セクションで bounded。visitor を使い出力全件を保持しない。自動 table decode はしない（親が必要な table だけ DecodeSection する）。
- 同期未確定時は 188 間隔の 2 sync bytes を確認（EOF/上限まで 1 packet しかない場合は有効 header の 1 packet を許可）。途中の sync 喪失で再探索する。
- `Read` が bytes と非 EOF error を同時に返した場合、返された bytes は limit/context/visitor が許す範囲で処理し、その観測済み error は MaxPackets/MaxSections 早期終了でも保存する。優先順位は (1) options/Seek の事前失敗、(2) callback 前に観測した context cancellation、(3) 呼び出した visitor 自身の error、(4) visitor 成功直後/終了時の context cancellation、(5) reader error、(6) 正常 limit/EOF。visitor error と reader error、または context error と reader error は primary error を先に `errors.Join` し、双方の `errors.Is` を維持。visitor が cancel と error を同時に返せば visitor error を primary として扱う。limit 自体は `ErrLimit` を返さず、同時に error があっても実際に上限判定へ到達した MaxPackets/MaxSections の LimitReached=true は保持する。通常の MaxBytes は読込上限へ進んだ時に flag が立つため、reader error/context が先に終了させた場合は BytesRead==MaxBytes でも flag が false の場合がある。未実行の次の Read にある error は観測しない。
- context は read と packet/callback 処理の間に確認。連続 100 回の `Read` が `(0, nil)` なら `io.ErrNoProgress`。100 回目の Read 内で cancel された場合も終了時の context cancellation を優先し、`io.ErrNoProgress` を併記しない。cancel がなければ従来どおり `io.ErrNoProgress`。同期 io.Reader の既に実行中の blocking Read 自体は中断できないため、必要なら呼び出し側で deadline/close 対応 reader を使う。

```go
// effectiveEnd は zero-padding を除いた有効 TS byte size。
offset := (effectiveEnd / 5 / psi.PacketSize) * psi.PacketSize
stats, err := psi.ScanWindow(ctx, file, offset,
    psi.ScanOptions{MaxBytes: 188 * 10000, PIDs: []uint16{0x12, 0x14}},
    func(s psi.Section) error {
        table, err := psi.DecodeSection(s)
        // *psi.EIT / *psi.TOT を親が処理する。
        _ = table
        return err
    })
_ = stats
_ = err
```

## 親 adapter への注意・残る制約

- PCRAtPUSI は開始時点までに観測した最新 PCR。補間・未来の PCR からの逆算はしない。未観測なら nil。複数 program の TS では PMT.PCRPID を PCRPID option に指定すること（未指定では別 PID の最新 PCR が選ばれ得る）。PCRDelta は forward modulo 差で、discontinuity flag のない逆行を真の wrap と区別しない。複数 wrap をまたぐ観測空白にも対応しない。
- 窓ごとの PCR epoch は独立。TOT clock は日本の JST wall clock 前提。PCR/TOT の録画開始終了推定は親側で行う。local_time_offset_descriptor は raw のまま保持し、海外 UTC TOT を補正しない。
- EIT は actual p/f 0x4e、SDT は actual 0x42 のみ。時刻/duration の false は未確定と不正 BCD を区別しない。service/name descriptor 不在は空 bytes、壊れた loop/既知 service descriptor は section decode error。ARIB 文字復号、descriptor による番組詳細統合、複数 table/version の集約は親側。
- section の上限は header/CRC を含む 4096 bytes。continuity gap / TEI / scrambling / 同期喪失では保留 section を捨て、次の正常 PUSI まで待つ。header の長さ自体が壊れた場合の packet 内の猜測再同期はしない（CRC が壊れただけで長さが正常なら次の section へ進む）。窓末尾の未完 section は通知せず破棄する。
- Scan は選択 PID から section を emit するだけで PAT→PMT の動的 PID discovery はしない。通常は PIDs に PSI/SI PID と既知 PMT PID を指定する。nil の全 PID scan は PES payload も PSI として試行し、InvalidSections が増え得る。PCR の追跡は PIDs filter と独立。
- MaxPackets/MaxSections は処理/visitor 上限。先読み済み bytes は Stats.BytesRead に含まれる（MaxBytes は絶対に超えない）。Scan は現在 reader 位置を相対 offset=0 とみなし、絶対 offset が必要なら ScanWindow を使う。
- テストは独自 synthetic のみ。実番組 TS での相互運用性・metadata adapter 接続はこのパッケージでは未検証。

## 独立レビュー回帰と再現手順

`review_regression_test.go` / `review_recovery_test.go` / `review_table_constraints_test.go` に scratch レビューの必要 synthetic 回帰を移入し、各不具合を repository 上の実 FAIL → 最小修正 → package PASS の順で検証。`review_scan_errors_test.go` は reader/limit/context/visitor の競合契約を補強する。`review_sequence_fuzz_test.go` は一般 packet sequence と PCR-only 再送/破損 PID 復旧の状態付き sequence properties を検証する。最終 cycle 2 の `review_filtered_pcr_test.go` は filter 外の payload PCR 再送/epoch/Scan chunk 分割、非 nil 空 filter、adaptation-only、filter 外 section/CC error 非計上、および `FuzzFilteredPCRDuplicateSequence` を検証する。`review_no_progress_test.go` は100回目の空 Read 内での cancel と無 cancel の対照を検証する。単一 packet ごとの新規 assembler だけでは continuity/pending 回帰を検出できないため、同じ assembler に複数 packet を投入する。詳細と実行結果は `TDD.md`。

```bash
"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/psi -count=1
"C:/Program Files/Go/bin/go.exe" vet ./internal/tsinfo/psi
"C:/Program Files/Go/bin/gofmt.exe" -l internal/tsinfo/psi/*.go
# 短時間のみ。対象は同 package に限定。
"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/psi -run '^$' -fuzz '^FuzzPCRDuplicateSequence$' -fuzztime=3s -parallel=1
```

公開 func/type の signature/fields と sentinel は維持。metadata/cmd/全体 build、実機 TS/EPG の repository 追加、実機操作はこの変更の検証範囲に含めない。

## 範囲外

192/204-byte TS、PES/media decode、ARIB 文字復号、複数 section/version の table 集約、service 選択、番組選択、録画時間推定、PCR extrapolation、PSI archive (.psc)、メタデータ保存/API wiring は行わない。bounded な sample に欲しい PSI が含まれない場合の追加窓選択は呼び出し側の責務。
