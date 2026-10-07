# 第三者データとライセンス

## 独自実装

`decode.go`、`program_symbols.go`、生成スクリプト、synthetic テストは本タスクで記述した。
リポジトリの MIT ライセンスに従う。ariblib のコード・テーブルはコピーしておらず、
GPL ソフトウェアを依存に追加していない。Python/ariblib は Go 実行時に使用しない。

## JIS X 0208 → Unicode

- 生成元: CPython 3.14.7 標準ライブラリ `euc_jp` codec。
- 生成方法: 94×94 の区点コードの両 byte に 0x80 を OR して decode。
  未定義の区点は Go 配列のゼロ値として残す。codec のソースコード自体はコピーしない。
- データ出力を保守的に CPython 由来として扱い、PSF copyright とライセンスを保持する。
- Copyright (c) 2001 Python Software Foundation; All Rights Reserved.
- CPython の歴史的ライセンスを含む全文を `licenses/CPython.txt` に同梱。
- 変更要約: codec の二バイト文字変換結果を独立した Go の区点索引配列へ変換した。
  CPython 本体・codec 実装は変更していない。
- 根拠: https://docs.python.org/3/license.html
- LICENSE 原文: https://github.com/python/cpython/blob/v3.14.0/LICENSE
- 実装出典: https://github.com/python/cpython/blob/v3.14.0/Modules/cjkcodecs/_codecs_jp.c
- 文字表出典: https://github.com/python/cpython/blob/v3.14.0/Modules/cjkcodecs/mappings_jp.h

## ARIB 追加文字 → Unicode

`additional_symbols.json` と `additional_table.go` は以下の文字対応データに由来する。

- libaribcaption revision `6c3b4f361bc8f65b34497ba0d123c5aea6f9ed84`
- https://github.com/xqq/libaribcaption/blob/6c3b4f361bc8f65b34497ba0d123c5aea6f9ed84/src/decoder/b24_gaiji_table.hpp
- ファイルヘッダー: ISC の許諾文。プロジェクト全体: MIT。
- 変更要約: `kAdditionalSymbolsTable_Unicode` を区点キー JSON と Go map に変換。
  未定義 U+FFFD および Unicode PUA のフォント専用グリフを除外した。
  番組マークは独自の bracketed label で上書きする。
- 両方の著作権・許諾文を以下に保持する。生成 Go ファイルにも ISC notice を保持する。

### 元ファイルの ISC notice

```
Copyright (C) 2021 magicxqq <xqq@xqq.im>. All rights reserved.

Permission to use, copy, modify, and distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
```

### プロジェクトの MIT notice

```
MIT License

Copyright (c) 2022 magicxqq

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## 規格の参照のみ

ARIB STD-B24 Volume 1 Part 2 Chapter 7 の designation、invocation、制御構造を参照し、
Go の解析ロジックを独自に記述した。規格 PDF 自体、図表画像、本文は配布物に含めない。

https://www.arib.or.jp/english/html/overview/doc/6-STD-B24v5_1-1p3-E2.pdf

Google の古い `emoji4unicode/arib.ucm` は調査時に注意書きを確認しただけで、
本実装のデータ生成には使用していない。
