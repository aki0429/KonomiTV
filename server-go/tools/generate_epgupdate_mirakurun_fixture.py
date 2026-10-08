"""
Channel.updateFromMirakurun() / Program.updateFromMirakurun() の Go 移植用オラクルを生成する。

KonomiTV の Python 実装をそのまま実行し、Mirakurun API (HTTPX_CLIENT)・設定・現在時刻だけを固定値に差し替える。
入力 (初期 DB 行・/api/services・/api/programs) と、更新後の channels / programs テーブルの全行を JSON に書き出す。

使い方 (server/ を作業ディレクトリとし、KonomiTV の Python 環境で実行):
    python ../server-go/tools/generate_epgupdate_mirakurun_fixture.py ../server-go/internal/epgupdate/testdata/mirakurun_update_fixture.json
"""

import asyncio
import json
import os
import sqlite3
import sys
import tempfile
import traceback
from datetime import datetime, timedelta
from pathlib import Path

sys.path.insert(0, '.')

from tortoise import Tortoise  # noqa: E402

import app.logging as logging_module  # noqa: E402
import app.models.Channel as channel_module  # noqa: E402
import app.models.Program as program_module  # noqa: E402
import app.utils as utils_module  # noqa: E402
from app.constants import DATABASE_CONFIG, JST  # noqa: E402


NOW = datetime(2026, 10, 9, 12, 0, 0, tzinfo=JST)


class FixedDatetime(datetime):
    @classmethod
    def now(cls, tz=None):  # type: ignore[override]
        return NOW if tz is None else NOW.astimezone(tz)

    @classmethod
    def fromtimestamp(cls, timestamp, tz=None):  # type: ignore[override]
        # SQLite へ渡す値はサブクラスではなく通常の datetime にする
        return datetime.fromtimestamp(timestamp, tz)


class StubConfig:
    class general:
        backend = 'Mirakurun'
        debug = False
        mirakurun_url = 'http://mirakurun.invalid:40772/'

    class tv:
        preferred_terrestrial_region = None


def ms(hours: float) -> int:
    return int((NOW + timedelta(hours=hours)).timestamp() * 1000)


def db_time(hours: float) -> str:
    return (NOW + timedelta(hours=hours)).strftime('%Y-%m-%d %H:%M:%S+09:00')


PROGRAM_COLUMNS = (
    'id, channel_id, network_id, service_id, event_id, title, description, detail, start_time, end_time, '
    'duration, is_free, genres, video_type, video_codec, video_resolution, primary_audio_type, '
    'primary_audio_language, primary_audio_sampling_rate, secondary_audio_type, secondary_audio_language, '
    'secondary_audio_sampling_rate'
)

SEED_SQL = [
    # 既存の視聴可能チャンネル (更新される、BS の TSID は保持される)
    "INSERT INTO channels VALUES ('NID32736-SID1024','gr011',32736,1024,NULL,1,'011','GR','ＮＨＫ総合１・東京',5,0,0,1)",
    "INSERT INTO channels VALUES ('NID4-SID101','bs101',4,101,16625,1,'101','BS','ＮＨＫ　ＢＳ',NULL,0,0,1)",
    # ChSet に無く録画から参照 → 視聴不可へ
    "INSERT INTO channels VALUES ('NID32391-SID23608','gr071',32391,23608,32391,7,'071','GR','テレビ東京',NULL,0,0,1)",
    # ChSet に無く参照なし → 削除
    "INSERT INTO channels VALUES ('NID4-SID211','bs211',4,211,16624,11,'211','BS','ＢＳ１１イレブン',NULL,0,0,1)",
    # 録画専用で Mirakurun に存在 → 復帰
    "INSERT INTO channels VALUES ('NID4-SID141','bs141',4,141,16529,4,'141','BS','ＢＳ日テレ',NULL,0,0,0)",
    # 録画専用の閉局 BS → スキップ
    "INSERT INTO channels VALUES ('NID4-SID103','bs103',4,103,16626,3,'103','BS','ＮＨＫ　ＢＳプレミアム',NULL,0,0,0)",
    # 録画専用の地デジ → 枝番再計算
    "INSERT INTO channels VALUES ('NID32738-SID1040','gr041',32738,1040,32738,4,'041','GR','日テレ',NULL,0,0,0)",
    "INSERT INTO recorded_programs (id, recording_start_margin, recording_end_margin, is_partially_recorded, channel_id, network_id, service_id, event_id, title, description, detail, start_time, end_time, duration, is_free, genres, primary_audio_type, primary_audio_language, created_at, updated_at) VALUES (1,0,0,0,'NID32391-SID23608',32391,23608,1,'録画','','{}','2026-10-01 00:00:00+09:00','2026-10-01 01:00:00+09:00',3600,1,'[]','','','2026-10-01 00:00:00+09:00','2026-10-01 00:00:00+09:00')",
    # 変更なし
    f"INSERT INTO programs ({PROGRAM_COLUMNS}) VALUES ('NID32736-SID1024-EID100','NID32736-SID1024',32736,1024,100,'変更なし','概要','{{}}','{db_time(1)}','{db_time(2)}',3600.0,1,'[]',NULL,NULL,NULL,'古い音声','日本語','48kHz',NULL,NULL,NULL)",
    # 終了時間未定が降ってくる既存番組 (以前の終了時刻を保持)
    f"INSERT INTO programs ({PROGRAM_COLUMNS}) VALUES ('NID32736-SID1024-EID101','NID32736-SID1024',32736,1024,101,'延長','','{{}}','{db_time(2)}','{db_time(3)}',3600.0,1,'[]',NULL,NULL,NULL,'','','',NULL,NULL,NULL)",
    # 消えた番組
    f"INSERT INTO programs ({PROGRAM_COLUMNS}) VALUES ('NID32736-SID1024-EID999','NID32736-SID1024',32736,1024,999,'消えた','','{{}}','2026-10-01 00:00:00+09:00','2026-10-01 01:00:00+09:00',3600.0,1,'[]',NULL,NULL,NULL,'','','',NULL,NULL,NULL)",
]

SERVICES = [
    {'id': 3273601024, 'serviceId': 1024, 'networkId': 32736, 'name': 'ＮＨＫ総合１・東京', 'type': 1, 'remoteControlKeyId': 1},
    {'id': 3273601025, 'serviceId': 1025, 'networkId': 32736, 'name': 'ＮＨＫ総合２・東京', 'type': 1, 'remoteControlKeyId': 1},
    {'id': 3273601408, 'serviceId': 1408, 'networkId': 32736, 'name': 'ＮＨＫワンセグ', 'type': 192, 'remoteControlKeyId': 1},
    {'id': 3273701032, 'serviceId': 1032, 'networkId': 32737, 'name': 'ＮＨＫＥテレ１東京', 'type': 1, 'remoteControlKeyId': 2},
    {'id': 3239123656, 'serviceId': 23656, 'networkId': 32391, 'name': 'ＮＨＫ総合・千葉', 'type': 1, 'remoteControlKeyId': 1},
    {'id': 3274001048, 'serviceId': 1048, 'networkId': 32740, 'name': 'リモコン無し', 'type': 161},
    {'id': 400101, 'serviceId': 101, 'networkId': 4, 'name': 'ＮＨＫ　ＢＳ', 'type': 1, 'remoteControlKeyId': 1},
    {'id': 400103, 'serviceId': 103, 'networkId': 4, 'name': 'ＮＨＫ　ＢＳプレミアム', 'type': 1},
    {'id': 400141, 'serviceId': 141, 'networkId': 4, 'name': 'ＢＳ日テレ', 'type': 1},
    {'id': 400531, 'serviceId': 531, 'networkId': 4, 'name': '放送大学ラジオ', 'type': 2},
    {'id': 600161, 'serviceId': 161, 'networkId': 6, 'name': 'ＱＶＣ', 'type': 1},
    {'id': 1033024, 'serviceId': 33024, 'networkId': 10, 'name': 'スカパー', 'type': 1},
    {'id': 6553500100, 'serviceId': 100, 'networkId': 65535, 'name': '試験チャンネル１', 'type': 1},
    {'id': 1234500001, 'serviceId': 1, 'networkId': 12345, 'name': '不明', 'type': 1},
]


def program(eid: int, start_h: float, duration_ms: int, sid: int = 1024, nid: int = 32736, **extra) -> dict:
    data = {'id': nid * 100000 * 100000 + sid * 100000 + eid, 'eventId': eid, 'serviceId': sid, 'networkId': nid,
            'startAt': ms(start_h), 'duration': duration_ms, 'isFree': True}
    data.update(extra)
    return data


def audio(component_type: int, rate: int, langs: list[str], is_main: bool = True) -> dict:
    return {'componentType': component_type, 'componentTag': 16, 'isMain': is_main, 'samplingRate': rate, 'langs': langs}


PROGRAMS = [
    program(100, 1, 3600000, name='変更なし', description='概要', audios=[audio(3, 48000, ['jpn'])]),
    # 終了時間未定 (既存あり → 以前の終了時刻を使う)
    program(101, 2.5, 1, name='延長', audios=[audio(3, 48000, ['jpn'])]),
    # 終了時間未定 (新規 → 5 分)
    program(102, 4, 1, name='未定新規', audios=[audio(3, 48000, ['jpn'])]),
    # 詳細 (順序・空見出し・同名上書き)・ジャンル・映像・デュアルモノ
    program(103, 5, 5400000, name='【字】ドラマ', description='',
            extended={'◇出演者': '山田太郎', '': '本文', '内容': 'A', '◇内容': 'B'},
            genres=[{'lv1': 3, 'lv2': 0, 'un1': 15, 'un2': 15}, {'lv1': 14, 'lv2': 0, 'un1': 1, 'un2': 4},
                    {'lv1': 14, 'lv2': 1, 'un1': 0, 'un2': 0}, {'lv1': 15, 'lv2': 0, 'un1': 0, 'un2': 0}, {'lv1': 7, 'lv2': 15, 'un1': 0, 'un2': 0}],
            video={'type': 'mpeg2', 'resolution': '1080i', 'streamContent': 1, 'componentType': 0xb3},
            audios=[audio(2, 48000, ['jpn', 'eng']), audio(2, 44100, ['eng'], False)]),
    # 映像 streamContent が null / componentType 不明、音声 3.8 以下形式
    program(104, 6, 600000, name='旧形式', video={'type': None, 'resolution': None, 'streamContent': None, 'componentType': None},
            audio={'componentType': 2, 'samplingRate': 32000}, isFree=False),
    program(105, 7, 600000, name='映像不明', video={'type': 'h264', 'resolution': '720p', 'streamContent': 5, 'componentType': 0x7f},
            audios=[audio(0x7f, 24000, ['xyz']), audio(2, 48000, ['fra'], False)]),
    # 名前が無い (サブチャンネルで本線と同内容) → 除外
    program(106, 8, 600000, sid=1025, audios=[audio(3, 48000, ['jpn'])]),
    # relatedItems: shared の副側 → 除外 / 主側・relay・movement・type 無し → 残す
    program(107, 9, 600000, name='共有副側', relatedItems=[{'type': 'shared', 'networkId': 32736, 'serviceId': 1025, 'eventId': 900}], audios=[audio(3, 48000, ['jpn'])]),
    program(108, 10, 600000, name='共有主側', relatedItems=[{'type': 'shared', 'networkId': 32736, 'serviceId': 1024, 'eventId': 108}], audios=[audio(3, 48000, ['jpn'])]),
    program(109, 11, 600000, name='リレー', relatedItems=[{'type': 'relay', 'networkId': 32736, 'serviceId': 1032, 'eventId': 1}], audios=[audio(3, 48000, ['jpn'])]),
    program(110, 12, 600000, name='旧Mirakurun', relatedItems=[{'networkId': 32736, 'serviceId': 1032, 'eventId': 1}], audios=[audio(3, 48000, ['jpn'])]),
    program(111, 13, 600000, name='不明type', relatedItems=[{'type': 'unknown', 'networkId': 32736, 'serviceId': 1032, 'eventId': 1}], audios=[audio(3, 48000, ['jpn'])]),
    program(112, 14, 600000, name='空関連', relatedItems=[], audios=[audio(3, 48000, ['jpn'])]),
    # 12 時間以上前に終了 → 除外 / 11 時間前 → 残す
    program(113, -14, 3600000, name='古い', audios=[audio(3, 48000, ['jpn'])]),
    program(114, -12, 3600000, name='少し前', audios=[audio(3, 48000, ['jpn'])]),
    # 視聴可能でないチャンネル / 未登録チャンネル → 除外
    program(115, 1, 600000, sid=103, nid=4, name='閉局BS', audios=[audio(3, 48000, ['jpn'])]),
    program(116, 1, 600000, sid=1408, name='ワンセグ', audios=[audio(3, 48000, ['jpn'])]),
    # BS の番組 (ミリ秒あり) と説明の空白整形
    program(117, 1.0001, 123456, sid=101, nid=4, name='　ＢＳの番組　', description='  説明  ', audios=[audio(3, 48000, ['jpn'])]),
]


class FakeResponse:
    def __init__(self, data):
        self.status_code = 200
        self._data = data

    def json(self):
        return json.loads(json.dumps(self._data))


class FakeClient:
    async def __aenter__(self):
        return self

    async def __aexit__(self, *args):
        return False

    async def get(self, url, timeout=None):
        if url == 'http://mirakurun.invalid:40772/api/services':
            return FakeResponse(SERVICES)
        if url == 'http://mirakurun.invalid:40772/api/programs':
            return FakeResponse(PROGRAMS)
        raise AssertionError(url)


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
        import app.config as config_module
        config_module._CONFIG = StubConfig  # type: ignore
        channel_module.Config = lambda: StubConfig  # type: ignore
        program_module.Config = lambda: StubConfig  # type: ignore
        logging_module.Config = lambda: StubConfig  # type: ignore
        program_module.datetime = FixedDatetime  # type: ignore
        channel_module.HTTPX_CLIENT = FakeClient  # type: ignore
        program_module.HTTPX_CLIENT = FakeClient  # type: ignore

        await channel_module.Channel.updateFromMirakurun()
        await program_module.Program.updateFromMirakurun()
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
    fixture = {'now': NOW.isoformat(), 'seed_sql': SEED_SQL, 'services': SERVICES, 'programs': PROGRAMS, 'cases': cases}
    Path(sys.argv[1]).write_text(json.dumps(fixture, ensure_ascii=False, indent=1) + '\n', encoding='utf-8')


exit_code = 0
try:
    asyncio.run(main())
except BaseException:
    traceback.print_exc()
    exit_code = 1
# aiosqlite のスレッドが残ってもプロセスを確実に終了する
sys.stdout.flush()
sys.stderr.flush()
os._exit(exit_code)
