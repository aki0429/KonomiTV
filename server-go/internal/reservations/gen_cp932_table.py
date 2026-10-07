"""cp932 (Shift_JIS) デコードテーブルを Go ソース (cp932_table.go) として生成する。

Go 標準ライブラリには cp932 のデコーダがなく、追加依存 (golang.org/x/text) を使わずに
Python 版 EDCBUtil.convertBytesToString() と同じデコード結果を得るために、
Python の cp932 codec からテーブルを機械的に抽出して埋め込む。

使い方 (テーブルを作り直すときだけ手動で実行する。通常のビルドでは実行しない) :
    python internal/reservations/gen_cp932_table.py
"""

import os

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'cp932_table.go')

LEADS = list(range(0x81, 0xA0)) + list(range(0xE0, 0xFD))
TRAILS = list(range(0x40, 0x7F)) + list(range(0x80, 0xFD))
assert len(LEADS) == 60 and len(TRAILS) == 188

INVALID = 0xFFFF


def single_table():
    """1 バイトで完結する文字のマッピング (未定義は INVALID) 。"""
    values = []
    for byte in range(256):
        try:
            decoded = bytes([byte]).decode('cp932')
        except UnicodeDecodeError:
            decoded = None
        values.append(ord(decoded) if decoded is not None and len(decoded) == 1 else INVALID)
    return values


def double_table():
    """先行バイト×後続バイトのマッピング (未定義は INVALID) 。"""
    values = []
    for lead in LEADS:
        for trail in TRAILS:
            try:
                decoded = bytes([lead, trail]).decode('cp932')
            except UnicodeDecodeError:
                decoded = None
            values.append(ord(decoded) if decoded is not None and len(decoded) == 1 else INVALID)
    return values


def format_runes(values, per_line=12):
    lines = []
    for index in range(0, len(values), per_line):
        chunk = values[index:index + per_line]
        parts = []
        for value in chunk:
            parts.append('cp932Invalid,' if value == INVALID else ('0x%04X,' % value))
        lines.append('\t' + ' '.join(parts))
    return '\n'.join(lines)


def format_ints(values, per_line=16):
    lines = []
    for index in range(0, len(values), per_line):
        chunk = values[index:index + per_line]
        lines.append('\t' + ' '.join(('%d,' % value) for value in chunk))
    return '\n'.join(lines)


def main():
    singles = single_table()
    doubles = double_table()
    lead_index = [-1] * 256
    for index, lead in enumerate(LEADS):
        lead_index[lead] = index
    trail_index = [-1] * 256
    for index, trail in enumerate(TRAILS):
        trail_index[trail] = index

    body = []
    body.append('package reservations\n')
    body.append('// このファイルは Python の cp932 codec から機械的に抽出したデコードテーブル。')
    body.append('// 生成スクリプト: internal/reservations/gen_cp932_table.py (手動実行) 。')
    body.append('//')
    body.append('// Go 標準ライブラリには cp932 のデコーダがないため、追加依存を増やさずに')
    body.append('// Python 版 EDCBUtil.convertBytesToString() と同じ結果を得るために埋め込んでいる。\n')
    body.append('// cp932Invalid は cp932 として未定義であることを表す。')
    body.append('const cp932Invalid = 0x%04X\n' % INVALID)
    body.append('// cp932SingleTable は 1 バイトで完結する文字のマッピング。')
    body.append('var cp932SingleTable = [256]rune{')
    body.append(format_runes(singles))
    body.append('}\n')
    body.append('// cp932LeadIndex は先行バイトの cp932DoubleTable 上のインデックス (先行バイトでなければ -1) 。')
    body.append('var cp932LeadIndex = [256]int{')
    body.append(format_ints(lead_index))
    body.append('}\n')
    body.append('// cp932TrailIndex は後続バイトの cp932DoubleTable 上のインデックス (後続バイトでなければ -1) 。')
    body.append('var cp932TrailIndex = [256]int{')
    body.append(format_ints(trail_index))
    body.append('}\n')
    body.append('// cp932DoubleTable は先行バイト×後続バイトのマッピング。')
    body.append('// インデックスは cp932LeadIndex[lead]*%d + cp932TrailIndex[trail] 。' % len(TRAILS))
    body.append('var cp932DoubleTable = [%d]rune{' % len(doubles))
    body.append(format_runes(doubles))
    body.append('}\n')

    with open(OUT, 'w', encoding='utf-8', newline='\n') as handle:
        handle.write('\n'.join(body))
    print('leads=%d trails=%d doubles=%d mapped=%d' % (
        len(LEADS), len(TRAILS), len(doubles), sum(1 for v in doubles if v != INVALID)))
    print('written:', OUT, os.path.getsize(OUT), 'bytes')


if __name__ == '__main__':
    main()
