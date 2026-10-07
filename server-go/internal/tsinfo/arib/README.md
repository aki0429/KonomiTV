# ARIB STD-B24 テキストデコーダ

公開 API:

```go
func Decode(data []byte) string
```

入力は放送メタデータの ARIB 8-bit 文字列、出力は Unicode UTF-8。
呼び出し毎に独立した状態を持ち、入力を変更しない。標準ライブラリのみで動作し、
Python/ariblib、cgo、ネットワーク、追加 Go module は本番に不要。

## 対応

- メタデータ初期状態: G0=漢字、G1=英数、G2=ひらがな、G3=カタカナ、GL=G0、GR=G2。
- ESC の G0–G3 1/2-byte designation、DRCS designation、LS0/LS1/LS2/LS3、
  LS1R/LS2R/LS3R、SS2/SS3（次の GL/GR graphic 1 文字だけ）。
- JIS X 0208 漢字・記号、英数、ひらがな・カタカナ・句読点、各 proportional set、
  JIS X 0201 カタカナの全角表現。
- 追加漢字・記号の Unicode 対応データ。`[字][解][多]` 等の番組マークは検索しやすい文字列。
- 英数・空白を半角化しない。必要な正規化は親の metadata formatString 段階で行う。
- APR/APD は改行、SP/APF は全角空白。
- PAPF/APS、SZX/COL/FLC/CDC/POL/WMM/HLC/RPC、CSI、TIME のパラメータを消費し、
  表示属性を本文に混入させない。MACRO 定義は終端までスキップして実行しない。
- 欠損・未定義 graphic・不正 byte・末尾切れは U+FFFD `�`。
  二バイト文字の破損で後続の制御を食べない。ループは必ず入力位置を進める。

## 意図的な未対応

- 字幕描画エンジンではない: 色・大きさ・縦書き・座標・消去・囲み・下線・合成・
  concealed text の描画効果を再現しない。RPC の繰返し、PAPF/APS の位置移動も展開しない。
- DRCS グリフ定義/画像認識、mosaic、任意/default macro の実行。
  対応しない文字集合は designation の 1/2-byte 幅を保持した上で `�`。
- JIS 互換漢字面1/面2（F=0x39/0x3a）、Latin/ABNT、UCS/UTF-8 モード。
- Unicode PUA の専用フォント文字・複数字形による追加記号の完全な再現。
- 非間隔文字の合成や規格 Annex E による combining mark 化。
- CS/APB/APU などはプレーンテキスト上の過去出力を書換えず無視する。

## データ再生成

Windows 環境では `python` を使う（`python3` は不要）。このディレクトリで:

```sh
python generate_tables.py
"C:/Program Files/Go/bin/gofmt.exe" -w jis_table.go additional_table.go
```

JIS 表は CPython stdlib `euc_jp` の変換結果。ARIB 追加表は出典・ライセンス付き
`additional_symbols.json` からネットワーク不要で生成する。詳細は
`THIRD_PARTY_NOTICES.md`。生成物をチェックイン対象として配布し、本番は Go のみ。

## 単体検証

server-go ディレクトリで:

```sh
"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/arib -count=1
"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/arib -count=1 -cover -v
"C:/Program Files/Go/bin/go.exe" test ./internal/tsinfo/arib -run="^$" -fuzz=FuzzDecode -fuzztime=5s -parallel=1
```

fixture は synthetic のみ。TDD の RED/GREEN と最終実出力は `TDD.md` に記録。
既存 metadata・psi・go.mod/go.sum はこの担当では変更しない。
