"""ariblib.constants の CONTENT_TYPE / USER_TYPE を Go ソース (arib_genre_table.go) として生成する。

番組検索条件のジャンル範囲の変換 (ReservationConditionsRouter.py) で使う。
辞書の挿入順が変換結果に影響するため、Go 側は map ではなく順序付きのスライスで保持する。

使い方 (テーブルを作り直すときだけ手動で実行する) :
    python internal/reservations/gen_arib_genre_table.py <ariblib/constants.py のパス>
"""

import json
import os
import sys

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), 'arib_genre_table.go')


def load_constants(path):
    namespace = {}
    with open(path, encoding='utf-8') as handle:
        source = handle.read()
    # 必要な 2 つの辞書だけを安全に取り出す (定数ファイル全体を import すると依存が重いため)
    import ast
    tree = ast.parse(source)
    wanted = {'CONTENT_TYPE', 'USER_TYPE'}
    for node in tree.body:
        if isinstance(node, ast.Assign) and len(node.targets) == 1:
            target = node.targets[0]
            if isinstance(target, ast.Name) and target.id in wanted:
                namespace[target.id] = ast.literal_eval(node.value)
    assert wanted <= set(namespace), namespace.keys()
    return namespace


def main():
    path = sys.argv[1] if len(sys.argv) > 1 else None
    assert path, 'usage: gen_arib_genre_table.py <ariblib/constants.py>'
    constants = load_constants(path)
    content_type = constants['CONTENT_TYPE']
    user_type = constants['USER_TYPE']

    lines = []
    lines.append('package reservations\n')
    lines.append('// このファイルは ariblib.constants の CONTENT_TYPE / USER_TYPE から機械的に抽出したテーブル。')
    lines.append('// 生成スクリプト: internal/reservations/gen_arib_genre_table.py (手動実行) 。')
    lines.append('//')
    lines.append('// ReservationConditionsRouter.py のジャンル変換で使う。変換結果は辞書の挿入順に依存するため、')
    lines.append('// Go 側は map ではなく順序付きのスライスで保持する。\n')
    lines.append('// aribGenreMiddleEntry は中分類 (content_nibble_level2) の 1 エントリ。')
    lines.append('type aribGenreMiddleEntry struct {')
    lines.append('\tKey  int')
    lines.append('\tName string')
    lines.append('}\n')
    lines.append('// aribGenreEntry は大分類 (content_nibble_level1) の 1 エントリ。')
    lines.append('type aribGenreEntry struct {')
    lines.append('\tKey    int')
    lines.append('\tMajor  string')
    lines.append('\tMiddle []aribGenreMiddleEntry')
    lines.append('}\n')
    lines.append('// aribContentType は ariblib.constants.CONTENT_TYPE 相当 (挿入順を保持) 。')
    lines.append('var aribContentType = []aribGenreEntry{')
    for major_key, (major_name, middle_map) in content_type.items():
        lines.append('\t{Key: %d, Major: %s, Middle: []aribGenreMiddleEntry{' % (major_key, json.dumps(major_name, ensure_ascii=False)))
        for middle_key, middle_name in middle_map.items():
            lines.append('\t\t{Key: %d, Name: %s},' % (middle_key, json.dumps(middle_name, ensure_ascii=False)))
        lines.append('\t}},')
    lines.append('}\n')
    lines.append('// aribUserTypeEntry は中分類ではなく user_nibble で表現されるジャンルの 1 エントリ。')
    lines.append('type aribUserTypeEntry struct {')
    lines.append('\tKey  int')
    lines.append('\tName string')
    lines.append('}\n')
    lines.append('// aribUserType は ariblib.constants.USER_TYPE 相当 (挿入順を保持) 。')
    lines.append('var aribUserType = []aribUserTypeEntry{')
    for key, name in user_type.items():
        lines.append('\t{Key: %d, Name: %s},' % (key, json.dumps(name, ensure_ascii=False)))
    lines.append('}\n')

    with open(OUT, 'w', encoding='utf-8', newline='\n') as handle:
        handle.write('\n'.join(lines))
    print('content_type majors=%d user_type=%d' % (len(content_type), len(user_type)))
    print('written:', OUT, os.path.getsize(OUT), 'bytes')


if __name__ == '__main__':
    main()
