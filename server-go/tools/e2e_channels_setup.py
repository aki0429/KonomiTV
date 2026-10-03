"""チャンネル一覧 API の E2E 検証用にリアルなチャンネル・番組データを投入する。

使い方: python tools/e2e_channels_setup.py <database.sqlite のパス>
"""

import datetime
import sqlite3
import sys

JST = datetime.timezone(datetime.timedelta(hours=9))


def format_db_time(value: datetime.datetime) -> str:
    return value.isoformat(sep=' ')


def insert_channel(
    con: sqlite3.Connection,
    channel_id: str,
    display_channel_id: str,
    network_id: int,
    service_id: int,
    remocon_id: int,
    channel_number: str,
    channel_type: str,
    name: str,
    is_subchannel: int = 0,
    is_radiochannel: int = 0,
    is_watchable: int = 1,
) -> None:
    con.execute(
        """
        INSERT INTO channels (
            id, display_channel_id, network_id, service_id, transport_stream_id,
            remocon_id, channel_number, type, name, jikkyo_force,
            is_subchannel, is_radiochannel, is_watchable
        ) VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, NULL, ?, ?, ?)
        """,
        (channel_id, display_channel_id, network_id, service_id, remocon_id, channel_number, channel_type, name,
         is_subchannel, is_radiochannel, is_watchable),
    )


def insert_program(
    con: sqlite3.Connection,
    program_id: str,
    channel_id: str,
    title: str,
    start_time: datetime.datetime,
    end_time: datetime.datetime,
) -> None:
    network_id, service_id = con.execute(
        'SELECT network_id, service_id FROM channels WHERE id = ?', (channel_id,)
    ).fetchone()
    duration = (end_time - start_time).total_seconds()
    con.execute(
        """
        INSERT INTO programs (
            id, channel_id, network_id, service_id, event_id, title, description,
            detail, start_time, end_time, duration, is_free, genres,
            video_type, video_codec, video_resolution,
            primary_audio_type, primary_audio_language, primary_audio_sampling_rate,
            secondary_audio_type, secondary_audio_language, secondary_audio_sampling_rate
        ) VALUES (?, ?, ?, ?, 1, ?, '説明', '{"テスト": "値"}', ?, ?, ?, 1,
            '[{"major": "ニュース／報道", "middle": "国内"}]',
            '映像', 'H.264', '1080i', '音声', '日本語', '48kHz', NULL, NULL, NULL
        )
        """,
        (program_id, channel_id, network_id, service_id, title,
         format_db_time(start_time), format_db_time(end_time), duration),
    )


def main() -> None:
    db_path = sys.argv[1]
    con = sqlite3.connect(db_path)
    con.execute('DELETE FROM programs')
    con.execute('DELETE FROM channels')

    # 地デジ (GR): 本放送 + 現在放送中のサブチャンネル + 休止中のサブチャンネル + ラジオ
    insert_channel(con, 'NID32736-SID1024', 'gr011', 32736, 1024, 1, '011', 'GR', 'NHK総合1・東京')
    insert_channel(con, 'NID32736-SID1025', 'gr011-1', 32736, 1025, 1, '011-1', 'GR', 'NHK総合2・東京', is_subchannel=1)
    insert_channel(con, 'NID32736-SID1026', 'gr011-2', 32736, 1026, 1, '011-2', 'GR', 'NHK総合3・東京', is_subchannel=1)
    insert_channel(con, 'NID32736-SID1027', 'gr011-3', 32736, 1027, 1, '011-3', 'GR', 'NHKラジオ第1', is_radiochannel=1)
    insert_channel(con, 'NID32737-SID1028', 'gr021', 32737, 1028, 2, '021', 'GR', 'NHK Eテレ1・東京')
    insert_channel(con, 'NID32738-SID1029', 'gr031', 32738, 1029, 3, '031', 'GR', '視聴不可チャンネル', is_watchable=0)

    # BS / CS / CATV / SKY / BS4K
    insert_channel(con, 'NID4-SID101', 'bs101', 4, 101, 0, 'BS101', 'BS', 'NHK BS')
    insert_channel(con, 'NID6-SID201', 'cs201', 6, 201, 0, 'CS201', 'CS', 'CS放送')
    insert_channel(con, 'NID14-SID301', 'catv301', 14, 301, 0, 'CATV301', 'CATV', 'ケーブルテレビ')
    insert_channel(con, 'NID10-SID401', 'sky401', 10, 401, 0, 'SKY401', 'SKY', 'スカパー!')
    insert_channel(con, 'NID11-SID501', 'bs4k501', 11, 501, 0, 'BS4K501', 'BS4K', 'BS4K放送')

    now = datetime.datetime.now(JST)

    # 地デジ本放送: 現在放送中 + 次の番組
    insert_program(con, 'E1', 'NID32736-SID1024', '放送中の番組', now - datetime.timedelta(minutes=30), now + datetime.timedelta(minutes=30))
    insert_program(con, 'E2', 'NID32736-SID1024', '次の番組', now + datetime.timedelta(minutes=30), now + datetime.timedelta(minutes=90))
    # 地デジ本放送: 24時間以上先の番組 (選択対象外)
    insert_program(con, 'E3', 'NID32736-SID1024', '明日の番組', now + datetime.timedelta(hours=30), now + datetime.timedelta(hours=31))
    # サブチャンネル (放送中)
    insert_program(con, 'E4', 'NID32736-SID1025', 'サブチャンネルの番組', now - datetime.timedelta(minutes=5), now + datetime.timedelta(minutes=55))
    # ラジオ (現在放送中の番組のみ)
    insert_program(con, 'E5', 'NID32736-SID1027', 'ラジオの番組', now - datetime.timedelta(minutes=10), now + datetime.timedelta(minutes=50))
    # Eテレ: 24時間以内に放送開始予定の番組のみ (放送休止中)
    insert_program(con, 'E6', 'NID32737-SID1028', 'Eテレの次の番組', now + datetime.timedelta(hours=2), now + datetime.timedelta(hours=3))
    # BS: 現在放送中 + 次の番組
    insert_program(con, 'E7', 'NID4-SID101', 'BSの放送中の番組', now - datetime.timedelta(minutes=20), now + datetime.timedelta(minutes=40))
    insert_program(con, 'E8', 'NID4-SID101', 'BSの次の番組', now + datetime.timedelta(minutes=40), now + datetime.timedelta(minutes=100))
    # CS: 番組情報なし
    # CATV: 24時間以内に放送開始予定の番組のみ
    insert_program(con, 'E9', 'NID14-SID301', 'CATVの次の番組', now + datetime.timedelta(hours=1), now + datetime.timedelta(hours=2))

    con.commit()
    print('channels:', con.execute('SELECT COUNT(*) FROM channels').fetchone()[0])
    print('programs:', con.execute('SELECT COUNT(*) FROM programs').fetchone()[0])
    con.close()


if __name__ == '__main__':
    main()
