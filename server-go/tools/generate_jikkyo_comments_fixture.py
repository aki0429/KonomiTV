"""ニコニコ実況のコメント整形処理のパリティ検証用フィクスチャを生成する。

server/ で `uv run python ../server-go/tools/generate_jikkyo_comments_fixture.py` として実行する。
生成先: server-go/internal/jikkyo/testdata/jikkyo_comments.json

- colors: 色指定 (ニコニコの色名・16 進数カラーコード・未定義の色名) の変換結果
- commands: コメントコマンド (mail) の解析結果 (色・位置・サイズ)
- special_commands: 運営コマンド付きコメントの判定結果
- comments: 過去ログ API のレスポンス (packet) を整形した結果
"""

import json
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(REPO_ROOT / 'server'))

from app.utils.JikkyoClient import JikkyoClient  # noqa: E402


def main() -> None:
    # 色指定の変換
    colors = {}
    for color in [
        'white', 'red', 'pink', 'orange', 'yellow', 'green', 'cyan', 'blue', 'purple', 'black',
        'white2', 'niconicowhite', 'red2', 'truered', 'pink2', 'orange2', 'passionorange',
        'yellow2', 'madyellow', 'green2', 'elementalgreen', 'cyan2', 'blue2', 'marineblue',
        'purple2', 'nobleviolet', 'black2', '#FFEAEA', '#123abc', '#12345', 'unknown', '',
    ]:
        colors[color] = JikkyoClient.getCommentColor(color)

    # コメントコマンドの解析
    commands = {}
    for command in [
        None, '', '184', '184 ue', '184 shita big', 'red ue big', 'naka', 'shita small',
        'blue2 right', '#FF0000 ue small', '184 unknown_command', ' 184  ',
    ]:
        commands[command if command is not None else '<null>'] = list(JikkyoClient.parseCommentCommand(command))

    # 運営コマンド付きコメントの判定
    special_commands = {}
    for comment, premium in [
        ('/vote', None), ('/vote', '3'), ('/vote', '1'), ('/vote now', '3'),
        ('/vote_now', '3'), ('/VOTE', '3'), ('/', '3'), ('/1vote', '3'),
        ('普通のコメント', '3'), ('/vote', ''), ('/nico', '3'),
    ]:
        key = f'{comment}|{premium if premium is not None else "<null>"}'
        special_commands[key] = JikkyoClient.isSpecialCommandComment(comment, premium)

    # 過去ログ API のレスポンスを整形した結果
    start_time = datetime(2026, 1, 1, 12, 0, 0, tzinfo=timezone(timedelta(hours=9)))
    packets = [
        {'chat': {'thread': '1', 'no': '1', 'vpos': '0', 'date': f'{int(start_time.timestamp())}',
                  'date_usec': '0', 'user_id': 'user1', 'mail': '184 ue big', 'premium': '1',
                  'anonymity': '1', 'content': 'コメント1'}},
        {'chat': {'thread': '1', 'no': '2', 'vpos': '0', 'date': f'{int(start_time.timestamp()) + 10}',
                  'date_usec': '500000', 'user_id': 'user2', 'mail': 'red shita small', 'premium': '1',
                  'anonymity': '1', 'content': 'コメント2'}},
        # 削除済みのコメント (除外される)
        {'chat': {'thread': '1', 'no': '3', 'vpos': '0', 'date': f'{int(start_time.timestamp()) + 20}',
                  'date_usec': '0', 'user_id': 'user3', 'mail': '184', 'premium': '1',
                  'anonymity': '1', 'deleted': '1', 'content': '削除済み'}},
        # 運営コメント (除外される)
        {'chat': {'thread': '1', 'no': '4', 'vpos': '0', 'date': f'{int(start_time.timestamp()) + 30}',
                  'date_usec': '0', 'user_id': 'user4', 'mail': '184', 'premium': '3',
                  'anonymity': '1', 'content': '/vote'}},
        # コメント本文がない (除外される)
        {'chat': {'thread': '1', 'no': '5', 'vpos': '0', 'date': f'{int(start_time.timestamp()) + 40}',
                  'date_usec': '0', 'user_id': 'user5', 'mail': '184', 'premium': '1',
                  'anonymity': '1'}},
        # mail がないコメント (デフォルトの色・位置・サイズになる)
        {'chat': {'thread': '1', 'no': '6', 'vpos': '0', 'date': f'{int(start_time.timestamp()) + 50}',
                  'date_usec': '0', 'user_id': 'user6', 'premium': '1',
                  'anonymity': '1', 'content': 'コメント3'}},
    ]

    # fetchJikkyoComments() と同じ整形処理を行う
    comments = []
    for raw_comment in packets:
        comment = raw_comment['chat'].get('content')
        if type(comment) is not str or comment == '':
            continue
        if raw_comment['chat'].get('deleted') == '1':
            continue
        if JikkyoClient.isSpecialCommandComment(comment, raw_comment['chat'].get('premium')):
            continue
        color, position, size = JikkyoClient.parseCommentCommand(raw_comment['chat'].get('mail'))
        chat_date = float(raw_comment['chat']['date'])
        chat_date_usec = int(raw_comment['chat'].get('date_usec', 0))
        comment_time = int(chat_date - int(start_time.timestamp())) + chat_date_usec / 1000000
        comments.append({
            'time': comment_time,
            'type': position,
            'size': size,
            'color': color,
            'author': raw_comment['chat'].get('user_id', ''),
            'text': comment,
        })

    output_path = REPO_ROOT / 'server-go' / 'internal' / 'jikkyo' / 'testdata' / 'jikkyo_comments.json'
    with open(output_path, mode='w', encoding='utf-8', newline='\n') as file:
        json.dump(
            {
                'start_time': int(start_time.timestamp()),
                'colors': colors,
                'commands': commands,
                'special_commands': special_commands,
                'packets': packets,
                'comments': comments,
            },
            file, ensure_ascii=False, indent=2,
        )
        file.write('\n')
    print(f'Wrote {output_path} ({len(comments)} comments)')


if __name__ == '__main__':
    main()
