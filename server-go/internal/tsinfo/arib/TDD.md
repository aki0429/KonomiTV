# TDD と実行証跡

## 手順

既存 production code のない独立パッケージで、ひらがな 1 スライスから開始。
最初は Decode 未定義のコンパイル失敗を確認した後、空戻り値の API stub を置き、
実際の assertion failure（空文字 != あいう）を確認してから最小実装した。
後続も垂直スライス単位に test 追加 → 実際の失敗 → 最小実装 → 全パッケージ GREEN を反復。
安全性・外部 API テストは完成実装への追加検証であり、後付けの機能実装に使っていない。

実行対象は常に `./internal/tsinfo/arib` のみ。repo-wide build/test、commit/push は未実行。

## 最初の RED（実出力）

```text
--- FAIL: TestDecodeInitialHiragana (0.00s)
    decode_test.go:7: Decode(initial GR=G2) = "", want "あいう"
FAIL
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	1.045s
FAIL
```

初回 GREEN:

```text
ok  	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.647s
```

## 後続サイクル（実出力から抜粋）

### LS1 RED observed

```text
--- FAIL: TestDecodeLS1FullwidthAlphanumeric (0.00s)
decode_test.go:19: Decode(LS1) = "", want "ＡＢＣ１２３￥￣"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	1.023s
```

### Initial Kanji RED

```text
--- FAIL: TestDecodeInitialKanji (0.00s)
decode_test.go:25: Decode(initial G0) = "あ", want "日本語あ"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.683s
```

### LS0 RED

```text
--- FAIL: TestDecodeLS0RestoresKanji (0.00s)
decode_test.go:31: Decode(LS0) = "ＡＦ｜", want "Ａ日"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.906s
```

### Single shifts RED

```text
--- FAIL: TestDecodeSingleShifts (0.00s)
decode_test.go:37: Decode(SS2/SS3 one graphic) = "���", want "あア日"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.887s
```

### ESC locking shifts RED

```text
--- FAIL: TestDecodeEscapeLockingShifts (0.00s)
decode_test.go:50: Decode(1b6e2224) = "遐�", want "あい"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.884s
```

### ESC one-byte designation RED

```text
--- FAIL: TestDecodeEscapeSingleByteDesignation (0.00s)
decode_test.go:66: Decode(1b28302224) = "唖�", want "あい"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	1.125s
```

### ESC two-byte designation RED

```text
--- FAIL: TestDecodeEscapeDoubleByteDesignation (0.00s)
decode_test.go:79: Decode(0e411b24420f467c) = "ＡＢ日", want "Ａ日"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.803s
```

### Program gaiji RED

```text
--- FAIL: TestDecodeProgramGaiji (0.00s)
decode_test.go:86: Decode(program marks) = "�����", want "[字][解][多][新][終]"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	1.032s
```

### Complete program labels RED

```text
--- FAIL: TestDecodeAllProgramGaiji (0.00s)
decode_test.go:100: program 7a50 = "�", want "[HV]"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.777s
```

### Additional symbols RED

```text
--- FAIL: TestDecodeAdditionalSymbols (0.00s)
decode_test.go:113: Decode(75217522752f7647) = "����", want "㐂𠅘𠮷髙"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.897s
```

### DRCS and unsupported sets RED

```text
--- FAIL: TestDecodeUndefinedDRCS (0.00s)
decode_test.go:132: Decode(1b28204121221b284a41) = "Ａ", want "��Ａ"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.692s
```

### Whitespace/newlines RED

```text
--- FAIL: TestDecodeWhitespaceAndNewlines (0.00s)
decode_test.go:146: Decode(a20da4) = "あい", want "あ\nい"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.859s
```

### Single shift GR RED

```text
--- FAIL: TestDecodeSingleShiftIsOneGraphicInEitherArea (0.00s)
decode_test.go:158: Decode(1da2a4) = "あい", want "アい"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.871s
```

### Control operands RED

```text
--- FAIL: TestDecodeControlOperandsAreNotText (0.00s)
decode_test.go:175: control 1643 leaked: "ＡＣＢ", want "ＡＢ"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.898s
```

### Opaque macro definition RED

```text
--- FAIL: TestDecodeMacroDefinitionIsNotExecuted (0.00s)
decode_test.go:187: Decode(0e419540601b283022954f42) = "Ａ＠｀＂ＯＢ", want "ＡＢ"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.877s
```

### Damage recovery RED

```text
--- FAIL: TestDecodeDamagedInputUsesReplacement (0.00s)
decode_test.go:220: Decode(a0ffa2) = "あ", want "��あ"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.889s
```

### Malformed designation RED

```text
--- FAIL: TestDecodeMalformedDesignationKeepsFollowingControl (0.00s)
decode_test.go:232: malformed designation 1b280e41 = "�", want "�Ａ"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.878s
```

### Additional-only set RED

```text
--- FAIL: TestDecodeAdditionalDesignationDoesNotUseBaseJIS (0.00s)
decode_test.go:238: additional-only set = "日[字]", want "�[字]"
FAIL	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	0.833s
```

## 最終単体テスト実出力

コマンド: `"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/arib -count=1`

```text
ok  	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	1.144s
```

Coverage コマンド: `go test ./internal/tsinfo/arib -count=1 -cover -v`

```text
PASS
coverage: 99.5% of statements
ok  	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	1.344s	coverage: 99.5% of statements
```

Fuzz コマンド: `go test ./internal/tsinfo/arib -run="^$" -fuzz=FuzzDecode -fuzztime=5s -parallel=1`

```text
fuzz: elapsed: 0s, gathering baseline coverage: 0/6 completed
fuzz: elapsed: 0s, gathering baseline coverage: 6/6 completed, now fuzzing with 1 workers
fuzz: elapsed: 3s, execs: 456 (152/sec), new interesting: 3 (total: 9)
fuzz: elapsed: 6s, execs: 456 (0/sec), new interesting: 3 (total: 9)
PASS
ok  	github.com/aki0429/KonomiTV/server-go/internal/tsinfo/arib	6.579s
```

全 byte pair と deterministic synthetic random/truncation の検証も PASS。
最終 coverage は実行時の計測値であり、規格全体への準拠率ではない。
JIS / 追加文字テーブルは generator 出力を gofmt したものと完全一致することを確認。

## 発生した問題

- 句読点スライスに末尾の「・」が欠け、RED→GREEN中に panic を検出。期待表を規格に照合して修正し、全テストを再実行。
- 最初の番組マーク試験で 0x7a6e を [終] と誤った。全ラベルの規格照合で 0x7a6d=[終]、0x7a6e=[生] を訂正して GREEN。
- ファイル単体の自動 vet は同パッケージの sibling symbols を渡さず undefined: Decode/jis0208 等と報告した。Go package test の実行結果を合否の根拠とした。
- file write の partial-read 保護と一時的な patch ツールエラーが発生。production 変更前に実際の RED を再確認し、成功を推測して進めていない。
- ARIB PDF の web extraction timeout は公式サイトからの取得とローカル抽出で代替。PDF/調査メモは scratch のみ。

未対応の仕様は README.md、ライセンス根拠・出典は THIRD_PARTY_NOTICES.md。
