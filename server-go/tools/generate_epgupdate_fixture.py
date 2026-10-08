"""
Channel.updateFromEDCB() / Program.updateFromEDCB() の Go 移植用オラクルを生成する。

KonomiTV の Python 実装をそのまま実行し、EDCB 通信・設定・現在時刻だけを固定値に差し替える。
入力 (初期 DB 行・ChSet5.txt・EnumService・EnumPgInfoEx) と、更新後の
channels / programs テーブルの全行を JSON に書き出す。

使い方 (server/ を作業ディレクトリとし、KonomiTV の Python 環境で実行):
    python ../server-go/tools/generate_epgupdate_fixture.py ../server-go/internal/epgupdate/testdata/edcb_update_fixture.json
"""

import asyncio
import json
import sqlite3
import sys
import tempfile
from datetime import datetime, timedelta
from pathlib import Path

sys.path.insert(0, '.')

from tortoise import Tortoise  # noqa: E402

import app.models.Channel as channel_module  # noqa: E402
import app.models.Program as program_module  # noqa: E402
from app.constants import DATABASE_CONFIG, JST  # noqa: E402
from app.utils.edcb.CtrlCmdUtil import CtrlCmdUtil  # noqa: E402


NOW = datetime(2026, 10, 9, 12, 0, 0, tzinfo=JST)


class FixedDatetime(datetime):
    @classmethod
    def now(cls, tz=None):  # type: ignore[override]
        return NOW if tz is None else NOW.astimezone(tz)


class StubConfig:
    class general:
        backend = 'EDCB'
        debug = False

    class tv:
        preferred_terrestrial_region = None


def at(hours: float) -> str:
    return (NOW + timedelta(hours=hours)).isoformat()


def db_time(hours: float) -> str:
    return (NOW + timedelta(hours=hours)).strftime('%Y-%m-%d %H:%M:%S+09:00')


PROGRAM_COLUMNS = (
    'id, channel_id, network_id, service_id, event_id, title, description, detail, start_time, end_time, '
    'duration, is_free, genres, video_type, video_codec, video_resolution, primary_audio_type, '
    'primary_audio_language, primary_audio_sampling_rate, secondary_audio_type, secondary_audio_language, '
    'secondary_audio_sampling_rate'
)

# 初期 DB の行 (Go 側も同じ SQL で投入する)
SEED_SQL = [
    # 既存の視聴可能チャンネル (ChSet5 に存在 → 更新、jikkyo_force は None に戻る)
    "INSERT INTO channels VALUES ('NID32736-SID1024','gr011',32736,1024,32736,1,'011','GR','ＮＨＫ総合１・東京',5,0,0,1)",
    # 既存の視聴可能チャンネル (ChSet5 に無く録画から参照 → 視聴不可へ)
    "INSERT INTO channels VALUES ('NID32391-SID23608','gr071',32391,23608,32391,7,'071','GR','テレビ東京',NULL,0,0,1)",
    # 既存の視聴可能チャンネル (ChSet5 に無く参照なし → 削除)
    "INSERT INTO channels VALUES ('NID4-SID211','bs211',4,211,16624,11,'211','BS','ＢＳ１１イレブン',NULL,0,0,1)",
    # 録画専用 (視聴不可) で ChSet5 に存在 → 視聴可能へ復帰
    "INSERT INTO channels VALUES ('NID4-SID141','bs141',4,141,16529,4,'141','BS','ＢＳ日テレ',NULL,0,0,0)",
    # 録画専用 (視聴不可) の閉局 BS → スキップされ視聴不可のまま
    "INSERT INTO channels VALUES ('NID4-SID103','bs103',4,103,16626,3,'103','BS','ＮＨＫ　ＢＳプレミアム',NULL,0,0,0)",
    # 録画専用 (視聴不可) の地デジ → 枝番の再計算対象
    "INSERT INTO channels VALUES ('NID32738-SID1040','gr041',32738,1040,32738,4,'041','GR','日テレ',NULL,0,0,0)",
    "INSERT INTO channels VALUES ('NID32745-SID1072','gr081',32745,1072,32745,8,'081','GR','フジテレビ',NULL,0,0,0)",
    # 録画番組 (チャンネル参照の確認用)
    "INSERT INTO recorded_programs (id, recording_start_margin, recording_end_margin, is_partially_recorded, channel_id, network_id, service_id, event_id, title, description, detail, start_time, end_time, duration, is_free, genres, primary_audio_type, primary_audio_language, created_at, updated_at) VALUES (1,0,0,0,'NID32391-SID23608',32391,23608,1,'録画','','{}','2026-10-01 00:00:00+09:00','2026-10-01 01:00:00+09:00',3600,1,'[]','','','2026-10-01 00:00:00+09:00','2026-10-01 00:00:00+09:00')",
    # 変更なしの番組 (タイトル/概要/詳細件数/開始/終了が一致)
    f"INSERT INTO programs ({PROGRAM_COLUMNS}) VALUES ('NID32736-SID1024-EID100','NID32736-SID1024',32736,1024,100,'変更なし','概要','{{}}','{db_time(1)}','{db_time(2)}',3600.0,1,'[]',NULL,NULL,NULL,'古い音声','日本語','48kHz',NULL,NULL,NULL)",
    # タイトルが変わる番組
    f"INSERT INTO programs ({PROGRAM_COLUMNS}) VALUES ('NID32736-SID1024-EID101','NID32736-SID1024',32736,1024,101,'旧タイトル','概要２','{{}}','{db_time(2)}','{db_time(2.5)}',1800.0,1,'[]',NULL,NULL,NULL,'','','',NULL,NULL,NULL)",
    # 詳細件数だけ同じ (Python は件数しか比較しないので更新されない)
    f"INSERT INTO programs ({PROGRAM_COLUMNS}) VALUES ('NID32736-SID1024-EID111','NID32736-SID1024',32736,1024,111,'詳細件数同じ','概要','{{\"見出し\":\"古い本文\"}}','{db_time(9)}','{db_time(9 + 1/6)}',600.0,1,'[]',NULL,NULL,NULL,'','','',NULL,NULL,NULL)",
    # EPG から消えた番組 → 削除
    f"INSERT INTO programs ({PROGRAM_COLUMNS}) VALUES ('NID32736-SID1024-EID999','NID32736-SID1024',32736,1024,999,'消えた','','{{}}','2026-10-01 00:00:00+09:00','2026-10-01 01:00:00+09:00',3600.0,1,'[]',NULL,NULL,NULL,'','','',NULL,NULL,NULL)",
]

CHSET5 = '\r\n'.join([
    # service_name, network_name, onid, tsid, sid, service_type, partial, epgcap, search, remocon
    'ＮＨＫ総合１・東京\t東京\t32736\t32736\t1024\t1\t0\t1\t1\t1',
    'ＮＨＫ総合２・東京\t東京\t32736\t32736\t1025\t1\t0\t1\t1\t1',
    'ＮＨＫワンセグ\t東京\t32736\t32736\t1408\t192\t1\t1\t1\t1',
    'ＮＨＫＥテレ１東京\t東京\t32737\t32737\t1032\t1\t0\t1\t1\t0',
    'ＮＨＫ総合・千葉\t千葉\t32391\t32391\t23656\t1\t0\t1\t1\t1',
    '臨時サービス\t東京\t32736\t32736\t1026\t161\t0\t1\t1\t0',
    'ＥＰＧ無し地デジ\t東京\t32740\t32740\t1048\t1\t0\t1\t1\t0',
    'ＮＨＫ　ＢＳ\tBS\t4\t16625\t101\t1\t0\t1\t1\t0',
    'ＮＨＫ　ＢＳプレミアム\tBS\t4\t16626\t103\t1\t0\t1\t1\t0',
    'ＢＳ日テレ\tBS\t4\t16529\t141\t1\t0\t1\t1\t0',
    '放送大学ラジオ\tBS\t4\t16545\t531\t2\t0\t1\t1\t0',
    'ＱＶＣ\tCS\t6\t24608\t161\t1\t0\t1\t1\t0',
    '試験チャンネル１\tCATV\t65535\t1\t100\t1\t0\t1\t1\t0',
    '不明\tX\t12345\t1\t1\t1\t0\t1\t1\t0',
    'short line',
]) + '\r\n'

ENUM_SERVICE = [
    {'onid': 32736, 'tsid': 32736, 'sid': 1024, 'service_type': 1, 'partial_reception_flag': 0, 'service_provider_name': '', 'service_name': 'ＮＨＫ総合１・東京', 'network_name': '', 'ts_name': '', 'remote_control_key_id': 1},
    {'onid': 32737, 'tsid': 32737, 'sid': 1032, 'service_type': 1, 'partial_reception_flag': 0, 'service_provider_name': '', 'service_name': 'ＮＨＫＥテレ１東京', 'network_name': '', 'ts_name': '', 'remote_control_key_id': 2},
]


def event(eid: int, start_h: float | None, duration: int | None, **extra) -> dict:
    data: dict = {'onid': 32736, 'tsid': 32736, 'sid': 1024, 'eid': eid, 'free_ca_flag': 0}
    if start_h is not None:
        data['start_time'] = at(start_h)
    if duration is not None:
        data['duration_sec'] = duration
    data.update(extra)
    return data


def service_info(onid: int, tsid: int, sid: int) -> dict:
    return {'onid': onid, 'tsid': tsid, 'sid': sid, 'service_type': 1, 'partial_reception_flag': 0, 'service_provider_name': '', 'service_name': '', 'network_name': '', 'ts_name': '', 'remote_control_key_id': 0}


def audio(component_type: int, multi_lingual: int, sampling_rate: int) -> dict:
    return {'stream_content': 2, 'component_type': component_type, 'component_tag': 0x10, 'stream_type': 15, 'simulcast_group_tag': 255, 'es_multi_lingual_flag': multi_lingual, 'main_component_flag': 1, 'quality_indicator': 1, 'sampling_rate': sampling_rate, 'text_char': ''}


SERVICE_EVENTS = [
    {
        'service_info': service_info(32736, 32736, 1024),
        'event_list': [
            event(100, 1, 3600, short_info={'event_name': '変更なし', 'text_char': '概要'}),
            event(101, 2, 1800, short_info={'event_name': 'ニュース　新タイトル', 'text_char': '概要２'}, free_ca_flag=1),
            event(102, 3, 5400,
                short_info={'event_name': '【字】ドラマ', 'text_char': ''},
                ext_info={'text_char': '◇出演者\r\n山田太郎\r\n◇内容\r\n本文です\r\n- 項目'},
                content_info={'nibble_list': [
                    {'content_nibble': 0x0300, 'user_nibble': 0},
                    {'content_nibble': 0x0E00, 'user_nibble': 0x0104},
                    {'content_nibble': 0x0E01, 'user_nibble': 0},
                    {'content_nibble': 0xF000, 'user_nibble': 0},
                    {'content_nibble': 0x070F, 'user_nibble': 0},
                ]},
                component_info={'stream_content': 1, 'component_type': 0xb3, 'component_tag': 0, 'text_char': ''},
                audio_info={'component_list': [audio(2, 1, 7), audio(3, 0, 99)]}),
            event(103, None, None, short_info={'event_name': '未定', 'text_char': ''}),
            event(104, 4, None, short_info={'event_name': '長さ未定', 'text_char': ''}),
            event(105, -14, 3600, short_info={'event_name': '古い', 'text_char': ''}),
            event(106, -12, 3600, short_info={'event_name': '少し前', 'text_char': ''}),
            event(107, 5, 600, short_info={'event_name': '副側', 'text_char': ''}, event_group_info={'group_type': 1, 'event_data_list': [{'onid': 32736, 'tsid': 32736, 'sid': 1025, 'eid': 900}]}),
            event(108, 6, 600, short_info={'event_name': '主側', 'text_char': ''}, event_group_info={'group_type': 1, 'event_data_list': [{'onid': 32736, 'tsid': 32736, 'sid': 1024, 'eid': 108}]}),
            event(109, 7, 600, short_info={'event_name': '詳細だけ', 'text_char': ' '}, ext_info={'text_char': '\r\n最初の本文\r\n◇出演者\r\nA\r\n出演者\r\nB'}),
            event(110, 8, 600, short_info={'event_name': '音声不明', 'text_char': ''}, audio_info={'component_list': [audio(0x7f, 0, 7)]}),
            event(111, 9, 600, short_info={'event_name': '詳細件数同じ', 'text_char': '概要'}, ext_info={'text_char': '◇見出し\r\n新しい本文'}),
            event(112, 10, 600, short_info={'event_name': '音声空', 'text_char': ''}, audio_info={'component_list': []}, component_info={'stream_content': 0x0e, 'component_type': 0x01, 'component_tag': 0, 'text_char': ''}),
        ],
    },
    {
        # TSID 違いの同一 NID-SID → チャンネルが一致しないので除外
        'service_info': service_info(4, 9999, 101),
        'event_list': [dict(event(200, 1, 600, short_info={'event_name': 'TSID違い', 'text_char': ''}), onid=4, tsid=9999, sid=101)],
    },
    {
        'service_info': service_info(4, 16625, 101),
        'event_list': [dict(event(201, 1, 600, short_info={'event_name': 'ＢＳの番組', 'text_char': ''}), onid=4, tsid=16625, sid=101)],
    },
    {
        # 登録されないサービス (ワンセグ) → 除外
        'service_info': service_info(32736, 32736, 1408),
        'event_list': [dict(event(300, 1, 600, short_info={'event_name': 'ワンセグ', 'text_char': ''}), sid=1408)],
    },
]


def to_python_event(data: dict) -> dict:
    result = dict(data)
    if 'start_time' in result:
        result['start_time'] = datetime.fromisoformat(result['start_time'])
    return result


async def run_case(name: str, preferred_region: str | None, out_cases: list) -> None:
    with tempfile.TemporaryDirectory() as directory:
        db_path = Path(directory) / 'database.sqlite'
        config = json.loads(json.dumps(DATABASE_CONFIG))
        config['connections']['default'] = f'sqlite://{db_path}'
        await Tortoise.init(config=config)
        await Tortoise.generate_schemas()
        connection = sqlite3.connect(db_path)
        schema = [row[0] for row in connection.execute(
            "SELECT sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY rowid")]
        for statement in SEED_SQL:
            connection.execute(statement)
        connection.commit()
        connection.close()

        StubConfig.tv.preferred_terrestrial_region = preferred_region
        channel_module.Config = lambda: StubConfig  # type: ignore
        program_module.Config = lambda: StubConfig  # type: ignore
        program_module.datetime = FixedDatetime  # type: ignore
        import app.logging as logging_module
        logging_module.Config = lambda: StubConfig  # type: ignore

        async def file_copy(self, name):
            assert name == 'ChSet5.txt'
            return CHSET5.encode('utf-8')

        async def enum_service(self):
            return [dict(service) for service in ENUM_SERVICE]

        async def enum_pg_info_ex(self, service_time_list):
            assert service_time_list == [0xffffffffffff, 0xffffffffffff, 1, 0x7fffffffffffffff]
            return [{'service_info': dict(s['service_info']), 'event_list': [to_python_event(e) for e in s['event_list']]} for s in SERVICE_EVENTS]

        from app.utils.edcb.EDCBUtil import EDCBUtil
        EDCBUtil.getEDCBHost = staticmethod(lambda edcb_url=None: '127.0.0.1')  # type: ignore
        EDCBUtil.getEDCBPort = staticmethod(lambda edcb_url=None: 4510)  # type: ignore
        CtrlCmdUtil.sendFileCopy = file_copy  # type: ignore
        CtrlCmdUtil.sendEnumService = enum_service  # type: ignore
        CtrlCmdUtil.sendEnumPgInfoEx = enum_pg_info_ex  # type: ignore

        await channel_module.Channel.updateFromEDCB()
        await program_module.Program.updateFromEDCB()
        await Tortoise.close_connections()

        connection = sqlite3.connect(db_path)
        tables = {}
        for table in ['channels', 'programs']:
            cursor = connection.execute(f'SELECT * FROM {table} ORDER BY id')
            columns = [description[0] for description in cursor.description]
            tables[table] = {'columns': columns, 'rows': [list(row) for row in cursor.fetchall()]}
        connection.close()
        out_cases.append({'name': name, 'preferred_terrestrial_region': preferred_region, 'schema': schema, 'expected': tables})


async def main() -> None:
    cases: list = []
    await run_case('default', None, cases)
    await run_case('preferred-kanagawa', '神奈川県', cases)
    fixture = {
        'now': NOW.isoformat(),
        'seed_sql': SEED_SQL,
        'chset5': CHSET5,
        'enum_service': ENUM_SERVICE,
        'service_events': SERVICE_EVENTS,
        'cases': cases,
    }
    Path(sys.argv[1]).write_text(json.dumps(fixture, ensure_ascii=False, indent=1) + '\n', encoding='utf-8')


exit_code = 0
try:
    asyncio.run(main())
except BaseException:
    import traceback
    traceback.print_exc()
    exit_code = 1
# aiosqlite のスレッドが残ってもプロセスを確実に終了する
sys.stdout.flush()
sys.stderr.flush()
import os  # noqa: E402
os._exit(exit_code)
