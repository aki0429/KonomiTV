"""Synthetic SQLite fixtures only; never reads deployed databases or PM2."""
from contextlib import contextmanager
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import unittest
from unittest import mock

try:
    import refresh_catalog as rc
except ModuleNotFoundError:
    rc = None

HERE = Path(__file__).resolve().parent
SCHEMA = """
CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT NOT NULL, number INTEGER);
CREATE TABLE series (id INTEGER PRIMARY KEY, title TEXT NOT NULL);
CREATE TABLE series_broadcast_periods (
    id INTEGER PRIMARY KEY, series_id INTEGER REFERENCES series(id), label TEXT);
CREATE TABLE programs (
    id TEXT PRIMARY KEY, channel_id TEXT REFERENCES channels(id), title TEXT, starts_at TEXT);
CREATE TABLE recorded_programs (
    id INTEGER PRIMARY KEY, channel_id TEXT REFERENCES channels(id),
    series_id INTEGER REFERENCES series(id), title TEXT NOT NULL);
CREATE TABLE recorded_videos (
    id INTEGER PRIMARY KEY, recorded_program_id INTEGER NOT NULL REFERENCES recorded_programs(id),
    status TEXT NOT NULL, duration REAL, file_path TEXT);
CREATE INDEX recorded_videos_status ON recorded_videos(status);
CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT, settings TEXT);
CREATE TABLE user_account_links (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id), label TEXT);
CREATE TABLE twitter_accounts (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id), label TEXT);
CREATE TABLE bluesky_accounts (id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id), label TEXT);
CREATE TABLE captures (
    id INTEGER PRIMARY KEY, user_id INTEGER REFERENCES users(id),
    recorded_program_id INTEGER REFERENCES recorded_programs(id) ON DELETE CASCADE, metadata BLOB);
"""
LOCAL_TABLES = ("users", "user_account_links", "twitter_accounts", "bluesky_accounts", "captures")


@contextmanager
def connect(*args, **kwargs):
    connection = sqlite3.connect(*args, **kwargs)
    try:
        with connection:
            yield connection
    finally:
        connection.close()


def database(path):
    with connect(path) as connection:
        connection.executescript(SCHEMA)
        connection.execute("INSERT INTO channels VALUES ('ch1', 'Test channel', 1)")
        connection.execute("INSERT INTO series VALUES (1, 'Test series')")
        connection.execute("INSERT INTO series_broadcast_periods VALUES (1, 1, '2026')")
        connection.execute("INSERT INTO programs VALUES ('p1', 'ch1', 'Test show', '2026-01-01')")
        connection.execute("INSERT INTO recorded_programs VALUES (1, 'ch1', 1, 'Test recording')")
        connection.execute("INSERT INTO recorded_videos VALUES (1, 1, 'Recording', 1.25, '/synthetic.ts')")
        connection.execute("INSERT INTO users VALUES (1, 'synthetic-user', '{}')")
        for table in LOCAL_TABLES[1:-1]:
            connection.execute(f"INSERT INTO {table} VALUES (1, 1, 'synthetic-local')")
        connection.execute("INSERT INTO captures VALUES (1, 1, 1, ?)", (b"synthetic-capture",))


def rows(path, table):
    with connect(f"{Path(path).as_uri()}?mode=ro", uri=True) as connection:
        return connection.execute(f'SELECT * FROM "{table}" ORDER BY 1').fetchall()


class FakeRunner:
    def __init__(self):
        self.calls = []
        self.status = "online"
        self.on_stop = None
        self.on_start = None
        self.start_count = 0
        self.fail_start_count = 0
        self.fail_stop = False

    def __call__(self, argv, *, timeout):
        self.calls.append(tuple(argv))
        action = argv[1]
        if action == "stop":
            if self.on_stop:
                self.on_stop()
            if self.fail_stop:
                return subprocess.CompletedProcess(argv, 1, "", "")
            self.status = "stopped"
        elif action == "start":
            self.start_count += 1
            if self.on_start:
                self.on_start(self.start_count)
            if self.start_count <= self.fail_start_count:
                return subprocess.CompletedProcess(argv, 1, "", "")
            self.status = "online"
        if action == "jlist":
            stdout = json.dumps([{"name": "konomitv-go", "pid": 123 if self.status == "online" else 0,
                                  "pm2_env": {"status": self.status}}])
        else:
            stdout = ""
        return subprocess.CompletedProcess(argv, 0, stdout, "")

    def count(self, action):
        return sum(call[1] == action for call in self.calls)


class RefreshCatalogTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="synthetic-test-", dir=HERE)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / "source.sqlite"
        self.destination = self.root / "destination.sqlite"
        database(self.source)
        database(self.destination)
        self.runner = FakeRunner()

    def refresh(self, **kwargs):
        self.assertIsNotNone(rc, "refresh_catalog.py is not implemented")
        options = dict(runner=self.runner, health_check=lambda: True,
                       verify_attempts=1, verify_interval=0)
        options.update(kwargs)
        return rc.refresh_catalog(self.source, self.destination, **options)

    def change_status(self, status="Recorded"):
        with connect(self.source) as connection:
            connection.execute("UPDATE recorded_videos SET status = ? WHERE id = 1", (status,))

    def assert_identity_rejected(self, source, code):
        before = {path: path.read_bytes() for path in (self.source, self.destination, source)}
        for operation in (rc.refresh_catalog, rc.check_catalog):
            with self.subTest(operation=operation.__name__):
                with self.assertRaises(rc.RefreshError) as error:
                    if operation is rc.refresh_catalog:
                        operation(source, self.destination, runner=self.runner, health_check=lambda: True,
                                  verify_attempts=1, verify_interval=0)
                    else:
                        operation(source, self.destination)
                self.assertEqual(error.exception.code, code)
                self.assertEqual({path: path.read_bytes() for path in before}, before)
                self.assertEqual(self.runner.calls, [])
                self.assertFalse(rc.recovery_path(self.destination).exists())
                self.assertFalse(self.destination.with_name(self.destination.name + ".catalog-refresh.lock").exists())

    def test_same_source_destination_rejected_before_any_write(self):
        self.assert_identity_rejected(self.destination, "source_destination_alias")

    def test_hardlinked_source_destination_rejected_before_any_write(self):
        alias = self.root / "hardlink.sqlite"
        os.link(self.destination, alias)
        self.assert_identity_rejected(alias, "source_destination_alias")

    def test_symlinked_source_destination_rejected_before_any_write(self):
        alias = self.root / "symlink.sqlite"
        try:
            os.symlink(self.destination, alias)
        except OSError as error:
            self.skipTest("OS does not permit synthetic symlink creation: " + str(error.winerror
                          if hasattr(error, "winerror") else error.errno))
        self.assert_identity_rejected(alias, "source_destination_alias")

    def test_unknown_source_destination_identity_fails_closed(self):
        with mock.patch.object(rc.os.path, "samefile", side_effect=OSError("synthetic stat failure")):
            self.assert_identity_rejected(self.source, "database_identity_unavailable")

    def test_alias_created_before_stop_aborts_before_service_stop(self):
        self.change_status()
        real_backup = rc.backup

        def alias_after_candidate(source, target):
            real_backup(source, target)
            if Path(target).name == "candidate.sqlite":
                self.source.unlink()
                os.link(self.destination, self.source)
        with mock.patch.object(rc, "backup", side_effect=alias_after_candidate):
            with self.assertRaises(rc.RefreshError) as error:
                self.refresh()
        self.assertEqual(error.exception.code, "source_destination_alias")
        self.assertEqual(self.runner.count("stop"), 0)
        self.assertEqual(rows(self.destination, "recorded_videos")[0][2], "Recording")

    def test_alias_created_during_stop_aborts_before_catalog_write(self):
        self.change_status()

        def alias_after_stop():
            self.source.unlink()
            os.link(self.destination, self.source)
        self.runner.on_stop = alias_after_stop
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "source_destination_alias")
        self.assertEqual(rows(self.destination, "recorded_videos")[0][2], "Recording")
        self.assertEqual(rows(self.source, "recorded_videos")[0][2], "Recording")
        self.assertEqual(self.runner.status, "online")

    def test_failed_merged_integrity_check_aborts_before_stop(self):
        self.change_status()
        before = self.destination.read_bytes()
        original_open = rc.open_writable

        class BadIntegrity:
            def __init__(self, connection):
                self.connection = connection
            def __getattr__(self, name):
                return getattr(self.connection, name)
            def execute(self, sql, parameters=()):
                if sql == "PRAGMA main.integrity_check":
                    class Result:
                        def fetchall(self):
                            return [("synthetic integrity failure",)]
                    return Result()
                return self.connection.execute(sql, parameters)
        with mock.patch.object(rc, "open_writable", side_effect=lambda path: BadIntegrity(original_open(path))):
            with self.assertRaises(rc.RefreshError) as error:
                self.refresh()
        self.assertEqual(error.exception.code, "merged_integrity_failed")
        self.assertEqual(self.runner.count("stop"), 0)
        self.assertEqual(self.destination.read_bytes(), before)

    def test_transaction_failure_mid_insert_rolls_back_every_catalog_table(self):
        self.change_status()
        before = {table: rows(self.destination, table) for table in rc.CATALOG_TABLES + LOCAL_TABLES}
        original_open = rc.open_writable

        class FailingConnection:
            def __init__(self, connection):
                self.connection = connection
            def __getattr__(self, name):
                return getattr(self.connection, name)
            def execute(self, sql, parameters=()):
                if sql.startswith('INSERT INTO main."series"'):
                    raise sqlite3.OperationalError("synthetic write failure")
                return self.connection.execute(sql, parameters)

        def fail_only_actual_write(path):
            connection = original_open(path)
            return FailingConnection(connection) if Path(path) == self.destination else connection
        with mock.patch.object(rc, "open_writable", side_effect=fail_only_actual_write):
            with self.assertRaises(rc.RefreshError) as error:
                self.refresh()
        self.assertTrue(error.exception.rollback_ok)
        self.assertEqual({table: rows(self.destination, table) for table in before}, before)
        self.assertEqual(self.runner.status, "online")

    def test_wal_source_commits_are_visible_in_consistent_backup(self):
        with connect(self.source) as writer:
            writer.execute("PRAGMA journal_mode=WAL")
            writer.execute("PRAGMA wal_autocheckpoint=0")
            writer.execute("UPDATE recorded_videos SET status='Recorded'")
            writer.commit()
            self.assertTrue(Path(str(self.source) + "-wal").exists())
            writer.execute("BEGIN IMMEDIATE")
            writer.execute("UPDATE recorded_videos SET duration=999")
            self.assertEqual(self.refresh()["status"], "updated")
            writer.rollback()
        self.assertEqual(rows(self.destination, "recorded_videos")[0][2:4], ("Recorded", 1.25))

    def test_catalog_autoincrement_keeps_local_user_sequence(self):
        # Rebuild only synthetic fixtures, with an autoincrement catalog PK.
        for path in (self.source, self.destination):
            with connect(path) as connection:
                connection.execute("DROP TABLE recorded_videos")
                connection.execute("CREATE TABLE recorded_videos (id INTEGER PRIMARY KEY AUTOINCREMENT, "
                                   "recorded_program_id INTEGER, status TEXT)")
                connection.execute("INSERT INTO recorded_videos VALUES (4, 1, 'Recording')")
        self.change_status()
        with connect(self.source) as connection:
            connection.execute("UPDATE recorded_videos SET status='Recorded'")
        self.refresh()
        with connect(self.destination) as connection:
            self.assertEqual(connection.execute("SELECT seq FROM sqlite_sequence WHERE name='users'").fetchone(), (1,))
            self.assertEqual(connection.execute("SELECT seq FROM sqlite_sequence WHERE name='recorded_videos'").fetchone(), (4,))

    def test_readonly_check_detects_change_without_writes_or_service_calls(self):
        self.change_status()
        before = self.destination.read_bytes()
        self.assertTrue(hasattr(rc, "check_catalog"), "readonly verification handle is missing")
        result = rc.check_catalog(self.source, self.destination)
        self.assertEqual(result["status"], "change_available")
        self.assertEqual(result["changed_tables"], ["recorded_videos"])
        self.assertEqual(self.destination.read_bytes(), before)
        self.assertFalse(rc.recovery_path(self.destination).exists())
        self.assertEqual(self.runner.calls, [])

    def test_check_command_outputs_summary_only(self):
        result = subprocess.run([os.sys.executable, str(HERE / "refresh_catalog.py"),
                                 "--source", str(self.source), "--destination", str(self.destination), "--check"],
                                capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, 0)
        summary = json.loads(result.stdout)
        self.assertEqual(summary["status"], "unchanged")
        self.assertNotIn("synthetic-user", result.stdout + result.stderr)
        self.assertEqual(result.stderr, "")

    def test_catalog_update_denies_writes_to_local_owned_tables(self):
        self.change_status()
        with connect(self.destination) as connection:
            connection.execute("CREATE TABLE generated_audit (id INTEGER PRIMARY KEY, payload TEXT)")
            connection.execute("INSERT INTO generated_audit VALUES (1, 'keep-local')")
        real_sync = rc.sync_catalog
        denied = []

        def exercise_guard(connection, staged, expected=None, *, commit_state=None, rollback_snapshot=None,
                           staged_expected=None):
            def attack(value):
                try:
                    connection.execute("DELETE FROM generated_audit")
                except sqlite3.DatabaseError:
                    denied.append(True)
                return value.lower()
            connection.create_function("lower", 1, attack, deterministic=True)
            return real_sync(connection, staged, expected, commit_state=commit_state,
                             rollback_snapshot=rollback_snapshot, staged_expected=staged_expected)
        # A stored generated column causes lower() to run during catalog insertion.
        for path in (self.source, self.destination):
            with connect(path) as connection:
                connection.execute("ALTER TABLE recorded_videos ADD COLUMN harmless TEXT "
                                   "GENERATED ALWAYS AS (lower(status)) VIRTUAL")
        with mock.patch.object(rc, "sync_catalog", side_effect=exercise_guard):
            self.refresh()
        self.assertTrue(denied)
        self.assertEqual(rows(self.destination, "generated_audit"), [(1, 'keep-local')])

    def test_generated_catalog_columns_are_compatible_and_synced(self):
        for path in (self.source, self.destination):
            with connect(path) as connection:
                connection.execute("ALTER TABLE recorded_videos ADD COLUMN status_lower TEXT "
                                   "GENERATED ALWAYS AS (lower(status)) VIRTUAL")
        self.change_status()
        self.assertEqual(self.refresh()["status"], "updated")
        self.assertEqual(rows(self.destination, "recorded_videos")[0][-1], "recorded")

    def test_failed_stop_after_stopping_service_recovers_original_online_state(self):
        self.change_status()
        before = self.destination.read_bytes()
        real_runner = self.runner

        def stopped_but_failed(argv, *, timeout):
            result = real_runner(argv, timeout=timeout)
            if argv[1] == "stop":
                return subprocess.CompletedProcess(argv, 1, "", "")
            return result
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh(runner=stopped_but_failed)
        self.assertEqual(error.exception.code, "service_stop_failed")
        self.assertEqual(self.runner.status, "online")
        self.assertEqual(self.destination.read_bytes(), before)

    def test_intentionally_stopped_service_is_never_started(self):
        self.change_status()
        before = self.destination.read_bytes()
        self.runner.status = "stopped"
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "service_not_online")
        self.assertEqual(self.runner.count("stop"), 0)
        self.assertEqual(self.runner.count("start"), 0)
        self.assertEqual(self.destination.read_bytes(), before)

    def test_interruption_after_commit_rolls_back_or_retains_recovery(self):
        self.change_status()
        before = rows(self.destination, "recorded_videos")

        def interrupt_first_start(number):
            if number == 1:
                raise KeyboardInterrupt
        self.runner.on_start = interrupt_first_start
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "interrupted")
        self.assertTrue(error.exception.rollback_ok)
        self.assertTrue(error.exception.service_recovered)
        self.assertEqual(rows(self.destination, "recorded_videos"), before)

    def test_unrecoverable_restart_retains_previous_catalog_for_manual_recovery(self):
        self.change_status()
        self.runner.fail_start_count = 20
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertTrue(error.exception.rollback_ok)
        self.assertFalse(error.exception.service_recovered)
        self.assertIsNotNone(error.exception.recovery_dir)
        recovery = Path(error.exception.recovery_dir)
        self.assertTrue((recovery / "previous.sqlite").is_file())
        self.assertTrue((recovery / "pending.json").is_file())
        self.assertEqual(rows(recovery / "previous.sqlite", "recorded_videos")[0][2], "Recording")
        stops = self.runner.count("stop")
        with self.assertRaises(rc.RefreshError) as blocked:
            self.refresh()
        self.assertEqual(blocked.exception.code, "pending_recovery")
        self.assertEqual(self.runner.count("stop"), stops)

    def test_all_snapshots_complete_before_service_stop(self):
        self.change_status()
        real_backup = rc.backup

        def pre_stop_only(source, target):
            if self.runner.status == "stopped":
                raise OSError("snapshot attempted after service stop")
            return real_backup(source, target)
        with mock.patch.object(rc, "backup", side_effect=pre_stop_only):
            self.assertEqual(self.refresh()["status"], "updated")

    def test_pre_stop_backup_failure_leaves_unchanged_database(self):
        self.change_status()
        before = self.destination.read_bytes()
        real_backup = rc.backup

        def failing_backup(source, target):
            if Path(source) == self.destination:
                raise OSError("synthetic disk failure")
            return real_backup(source, target)
        with mock.patch.object(rc, "backup", side_effect=failing_backup):
            with self.assertRaises(rc.RefreshError):
                self.refresh()
        self.assertEqual(self.runner.count("stop"), 0)
        self.assertEqual(self.runner.status, "online")
        self.assertEqual(self.destination.read_bytes(), before)

    def test_rollback_preserves_local_write_during_failed_health_check(self):
        self.change_status()
        before = rows(self.destination, "recorded_videos")
        calls = []

        def health():
            calls.append(1)
            if len(calls) == 1:
                with connect(self.destination) as connection:
                    connection.execute("INSERT INTO users VALUES (3, 'post-restart-user', '{}')")
                    connection.execute("INSERT INTO captures VALUES (3, 3, 1, ?)", (b"post-restart-capture",))
                return False
            return True
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh(health_check=health)
        self.assertTrue(error.exception.rollback_ok)
        self.assertEqual(rows(self.destination, "recorded_videos"), before)
        self.assertEqual(rows(self.destination, "users")[-1][0], 3)
        self.assertEqual(rows(self.destination, "captures")[-1][3], b"post-restart-capture")

    def test_stop_failure_leaves_database_usable(self):
        self.change_status()
        self.runner.fail_stop = True
        before = self.destination.read_bytes()
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "service_stop_failed")
        self.assertEqual(self.destination.read_bytes(), before)
        self.assertEqual(self.runner.status, "online")

    def test_failed_start_restores_catalog_without_replacing_local_tables(self):
        self.change_status()
        before = {table: rows(self.destination, table) for table in rc.CATALOG_TABLES + LOCAL_TABLES}
        self.runner.fail_start_count = 1
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "service_start_failed")
        self.assertTrue(error.exception.rollback_ok)
        self.assertTrue(error.exception.service_recovered)
        self.assertEqual({table: rows(self.destination, table) for table in before}, before)

    def test_false_successful_stop_never_updates_a_running_database(self):
        self.change_status()
        before = self.destination.read_bytes()
        real_runner = self.runner

        def still_running(argv, *, timeout):
            if argv[1] == "stop":
                real_runner.calls.append(tuple(argv))
                return subprocess.CompletedProcess(argv, 0, "", "")
            return real_runner(argv, timeout=timeout)
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh(runner=still_running)
        self.assertEqual(error.exception.code, "service_not_stopped")
        self.assertEqual(self.destination.read_bytes(), before)

    def test_overlapping_refresh_is_locked_out_before_staging(self):
        self.change_status()
        before = self.destination.read_bytes()
        nested_errors = []

        def second_refresh():
            try:
                self.refresh()
            except rc.RefreshError as error:
                nested_errors.append(error.code)
        self.runner.on_stop = second_refresh
        self.refresh()
        self.assertEqual(nested_errors, ["lock_busy"])
        self.assertEqual(self.runner.count("stop"), 1)
        self.assertNotEqual(self.destination.read_bytes(), before)
        self.assertEqual(self.refresh()["status"], "unchanged")

    def test_schema_change_during_stop_aborts_without_catalog_write(self):
        self.change_status()
        before = rows(self.destination, "recorded_videos")

        def change_schema():
            with connect(self.destination) as connection:
                connection.execute("ALTER TABLE recorded_videos ADD COLUMN unexpected TEXT")
        self.runner.on_stop = change_schema
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "catalog_schema_mismatch")
        self.assertEqual(rows(self.destination, "recorded_videos")[0][:-1], before[0])
        self.assertEqual(self.runner.status, "online")

    def test_aba_previous_snapshot_mismatch_aborts_without_rollback(self):
        self.change_status()
        before = {table: rows(self.destination, table) for table in rc.CATALOG_TABLES + LOCAL_TABLES}
        source_before = self.source.read_bytes()
        real_backup = rc.backup

        def backup_transient_catalog(source, target):
            previous = Path(source) == self.destination and Path(target).name == "previous.sqlite"
            if previous:
                with connect(self.destination) as connection:
                    connection.execute("UPDATE recorded_videos SET status='Transient-local'")
            real_backup(source, target)
            if previous:
                with connect(self.destination) as connection:
                    connection.execute("UPDATE recorded_videos SET status='Recording'")
        answers = iter((False, True))
        with mock.patch.object(rc, "backup", side_effect=backup_transient_catalog):
            with self.assertRaises(rc.RefreshError) as error:
                self.refresh(health_check=lambda: next(answers))
        self.assertEqual({table: rows(self.destination, table) for table in before}, before)
        self.assertEqual(error.exception.code, "rollback_baseline_changed")
        self.assertEqual(self.source.read_bytes(), source_before)
        self.assertEqual(self.runner.count("stop"), 1)
        self.assertEqual(self.runner.status, "online")

    def test_modified_previous_snapshot_aborts_before_catalog_write(self):
        self.change_status()
        before = {table: rows(self.destination, table) for table in rc.CATALOG_TABLES + LOCAL_TABLES}

        def modify_previous():
            previous = rc.recovery_path(self.destination) / "previous.sqlite"
            with connect(previous) as connection:
                connection.execute("UPDATE recorded_videos SET status='Transient-local'")
        self.runner.on_stop = modify_previous
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "rollback_baseline_changed")
        self.assertEqual({table: rows(self.destination, table) for table in before}, before)
        self.assertEqual(self.runner.count("stop"), 1)
        self.assertEqual(self.runner.status, "online")

    def test_alias_created_after_commit_blocks_catalog_rollback(self):
        self.change_status()
        incoming = rows(self.source, "recorded_videos")

        def alias_after_start(number):
            if number == 1:
                self.source.unlink()
                os.link(self.destination, self.source)
        self.runner.on_start = alias_after_start
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh(health_check=lambda: False)
        self.assertEqual(rows(self.source, "recorded_videos"), incoming)
        self.assertEqual(rows(self.destination, "recorded_videos"), incoming)
        self.assertFalse(error.exception.rollback_ok)
        self.assertIsNotNone(error.exception.recovery_dir)
        self.assertTrue((Path(error.exception.recovery_dir) / "pending.json").is_file())

    def test_changed_previous_after_commit_is_not_used_for_rollback(self):
        self.change_status()
        incoming = rows(self.source, "recorded_videos")

        def modify_previous_after_start(number):
            if number == 1:
                previous = rc.recovery_path(self.destination) / "previous.sqlite"
                with connect(previous) as connection:
                    connection.execute("UPDATE recorded_videos SET status='Transient-local'")
        self.runner.on_start = modify_previous_after_start
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh(health_check=lambda: False)
        self.assertEqual(rows(self.destination, "recorded_videos"), incoming)
        self.assertFalse(error.exception.rollback_ok)
        self.assertIsNotNone(error.exception.recovery_dir)
        self.assertTrue((Path(error.exception.recovery_dir) / "pending.json").exists())

    def test_external_commit_matching_incoming_is_not_rolled_back(self):
        self.change_status()
        source_before = {table: rows(self.source, table) for table in rc.CATALOG_TABLES + LOCAL_TABLES}
        local_before = {table: rows(self.destination, table) for table in LOCAL_TABLES}

        def external_commit():
            if self.runner.count("stop") != 1:
                return
            with connect(self.destination) as connection:
                connection.execute("UPDATE recorded_videos SET status='Recorded'")
                connection.execute("INSERT INTO users VALUES (2, 'external-local-user', '{}')")
        self.runner.on_stop = external_commit
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "destination_catalog_changed")
        self.assertEqual(rows(self.destination, "recorded_videos"), source_before["recorded_videos"])
        for table in LOCAL_TABLES:
            self.assertEqual(rows(self.destination, table)[:len(local_before[table])], local_before[table])
        self.assertEqual(rows(self.destination, "users")[-1][0], 2)
        self.assertEqual({table: rows(self.source, table) for table in source_before}, source_before)
        self.assertEqual(self.runner.count("stop"), 1)
        self.assertEqual(self.runner.status, "online")
        self.assertTrue(error.exception.rollback_ok)
        self.assertIsNone(error.exception.recovery_dir)

    def assert_ambiguous_commit_preserved(self, commit_first):
        self.change_status()
        original_open = rc.open_writable
        expected = rows(self.source if commit_first else self.destination, "recorded_videos")
        local_before = {table: rows(self.destination, table) for table in LOCAL_TABLES}
        attempted = []

        class AmbiguousCommit:
            def __init__(self, connection):
                self.connection = connection
            def __getattr__(self, name):
                return getattr(self.connection, name)
            def commit(self):
                if attempted:
                    return self.connection.commit()
                attempted.append(True)
                if commit_first:
                    self.connection.commit()
                raise KeyboardInterrupt

        def ambiguous_actual_commit(path):
            connection = original_open(path)
            return AmbiguousCommit(connection) if Path(path) == self.destination else connection
        with mock.patch.object(rc, "open_writable", side_effect=ambiguous_actual_commit):
            with self.assertRaises(rc.RefreshError) as error:
                self.refresh()
        self.assertEqual(error.exception.code, "interrupted")
        self.assertEqual(rows(self.destination, "recorded_videos"), expected)
        self.assertFalse(error.exception.rollback_ok)
        self.assertTrue(error.exception.service_recovered)
        self.assertIsNotNone(error.exception.recovery_dir)
        self.assertTrue((Path(error.exception.recovery_dir) / "pending.json").is_file())
        self.assertEqual({table: rows(self.destination, table) for table in LOCAL_TABLES}, local_before)
        self.assertEqual(self.runner.count("stop"), 1)
        self.assertEqual(self.runner.status, "online")
        with self.assertRaises(rc.RefreshError) as blocked:
            self.refresh()
        self.assertEqual(blocked.exception.code, "pending_recovery")

    def test_ambiguous_commit_after_sqlite_commit_retains_incoming_and_pending(self):
        self.assert_ambiguous_commit_preserved(commit_first=True)

    def test_ambiguous_commit_before_sqlite_commit_retains_original_and_pending(self):
        self.assert_ambiguous_commit_preserved(commit_first=False)

    def test_catalog_change_during_stop_aborts_without_overwriting_it(self):
        self.change_status()

        def change_local_catalog():
            with connect(self.destination) as connection:
                connection.execute("UPDATE recorded_videos SET status='Local-change'")
        self.runner.on_stop = change_local_catalog
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "destination_catalog_changed")
        self.assertEqual(rows(self.destination, "recorded_videos")[0][2], "Local-change")
        self.assertEqual(self.runner.status, "online")

    def test_catalog_trigger_is_rejected_before_it_can_mutate_local_data(self):
        trigger = "CREATE TRIGGER unsafe_catalog AFTER DELETE ON recorded_videos BEGIN DELETE FROM captures; END"
        for path in (self.source, self.destination):
            with connect(path) as connection:
                connection.execute(trigger)
        before = self.destination.read_bytes()
        self.change_status()
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "catalog_triggers_not_supported")
        self.assertEqual(self.runner.count("stop"), 0)
        self.assertEqual(self.destination.read_bytes(), before)

    def test_capture_bookmark_reference_blocks_removal_before_stop(self):
        with connect(self.destination) as connection:
            connection.execute("CREATE TABLE capture_bookmarks (id INTEGER PRIMARY KEY, "
                               "recorded_video_id INTEGER REFERENCES recorded_videos(id) ON DELETE CASCADE)")
            connection.execute("INSERT INTO capture_bookmarks VALUES (1, 1)")
        with connect(self.source) as connection:
            connection.execute("DELETE FROM recorded_videos")
        before = self.destination.read_bytes()
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "foreign_key_violation")
        self.assertEqual(self.runner.count("stop"), 0)
        self.assertEqual(self.destination.read_bytes(), before)
        self.assertEqual(rows(self.destination, "capture_bookmarks"), [(1, 1)])

    def test_local_rows_written_before_stop_are_preserved(self):
        self.change_status()
        with connect(self.source) as connection:
            connection.execute("UPDATE users SET username = 'different-source-user'")
            connection.execute("UPDATE captures SET metadata = ?", (b"different-source",))
        before = {table: rows(self.destination, table) for table in LOCAL_TABLES}

        def write_local():
            with connect(self.destination) as connection:
                connection.execute("INSERT INTO users VALUES (2, 'new-local-user', '{\"local\":true}')")
                connection.execute("INSERT INTO captures VALUES (2, 2, 1, ?)", (b"new-local-capture",))
        self.runner.on_stop = write_local
        self.refresh()
        for table in LOCAL_TABLES:
            self.assertEqual(rows(self.destination, table)[:len(before[table])], before[table])
        self.assertEqual(rows(self.destination, "users")[-1][1], "new-local-user")
        self.assertEqual(rows(self.destination, "captures")[-1][3], b"new-local-capture")
        with connect(self.destination) as connection:
            self.assertEqual(connection.execute("SELECT seq FROM sqlite_sequence WHERE name='users'").fetchone(), (2,))

    def test_source_read_failure_leaves_destination_unchanged(self):
        self.source.write_bytes(b"not-a-sqlite-database")
        before = self.destination.read_bytes()
        with self.assertRaises(rc.RefreshError):
            self.refresh()
        self.assertEqual(self.runner.calls, [])
        self.assertEqual(self.destination.read_bytes(), before)

    def test_restart_verification_failure_rolls_back_previous_catalog(self):
        before = {table: rows(self.destination, table) for table in rc.CATALOG_TABLES + LOCAL_TABLES}
        self.change_status()
        answers = iter((False, True))
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh(health_check=lambda: next(answers))
        self.assertEqual(error.exception.code, "restart_verification_failed")
        self.assertTrue(error.exception.rollback_ok)
        self.assertEqual({table: rows(self.destination, table) for table in before}, before)
        self.assertEqual(self.runner.count("stop"), 2)
        self.assertEqual(self.runner.count("start"), 2)

    def test_schema_difference_fails_closed_even_when_rows_match(self):
        with connect(self.destination) as connection:
            connection.execute("CREATE UNIQUE INDEX local_status ON recorded_videos(status)")
        before = self.destination.read_bytes()
        with self.assertRaises(rc.RefreshError):
            self.refresh()
        self.assertEqual(self.runner.count("stop"), 0)
        self.assertEqual(self.destination.read_bytes(), before)

    def test_status_only_change_is_detected(self):
        before = rows(self.destination, "recorded_programs")
        self.change_status()
        result = self.refresh()
        self.assertEqual(result["status"], "updated")
        self.assertEqual(rows(self.destination, "recorded_programs"), before)
        self.assertEqual(rows(self.destination, "recorded_videos")[0][2], "Recorded")
        self.assertEqual(self.runner.count("stop"), 1)
        self.assertEqual(self.runner.count("start"), 1)

    def test_torn_destination_read_cannot_report_false_unchanged(self):
        source_before = self.source.read_bytes()
        self.runner.status = "stopped"
        real_fingerprint = rc.fingerprint
        observed = {}
        with connect(self.destination) as writer:
            writer.execute("PRAGMA journal_mode=WAL")
            writer.execute("UPDATE recorded_videos SET status='Transient-local'")
            writer.commit()
            expected_view = rc.fingerprints(writer)

            def change_between_tables(connection, table):
                result = real_fingerprint(connection, table)
                if Path(connection.execute("PRAGMA database_list").fetchone()[2]) == self.destination:
                    observed[table] = result
                    if table == "channels" and len(observed) == 1:
                        writer.execute("UPDATE channels SET name='Transient-channel'")
                        writer.execute("UPDATE recorded_videos SET status='Recording'")
                        writer.commit()
                return result
            with mock.patch.object(rc, "fingerprint", side_effect=change_between_tables):
                with self.assertRaises(rc.RefreshError) as error:
                    self.refresh()
            self.assertEqual(error.exception.code, "service_not_online")
            self.assertEqual(observed, expected_view)
        self.assertEqual(self.runner.count("stop"), 0)
        self.assertEqual(self.source.read_bytes(), source_before)
        self.assertEqual(rows(self.destination, "channels")[0][1], "Transient-channel")
        self.assertEqual(rows(self.destination, "recorded_videos")[0][2], "Recording")

    def test_unchanged_schema_and_fingerprints_share_one_destination_view(self):
        real_schema = rc.catalog_schema
        real_fingerprint = rc.fingerprint
        observed = {}
        with connect(self.destination) as writer:
            writer.execute("PRAGMA journal_mode=WAL")
            expected_view = rc.fingerprints(writer)

            def change_after_schema(connection):
                result = real_schema(connection)
                if Path(connection.execute("PRAGMA database_list").fetchone()[2]) == self.destination:
                    writer.execute("CREATE INDEX interleaved_status ON recorded_videos(status)")
                    writer.execute("UPDATE recorded_videos SET status='Transient-local'")
                    writer.commit()
                return result

            def record_view(connection, table):
                result = real_fingerprint(connection, table)
                if Path(connection.execute("PRAGMA database_list").fetchone()[2]) == self.destination:
                    observed[table] = result
                return result
            with mock.patch.object(rc, "catalog_schema", side_effect=change_after_schema), \
                    mock.patch.object(rc, "fingerprint", side_effect=record_view):
                try:
                    result = self.refresh()
                except rc.RefreshError as error:
                    result = {"error": error.code}
            self.assertEqual(result, {"status": "unchanged"})
            self.assertEqual(observed, expected_view)
            self.assertIsNotNone(writer.execute("SELECT name FROM sqlite_schema WHERE name='interleaved_status'").fetchone())
        self.assertEqual(self.runner.calls, [])
        self.assertEqual(rows(self.destination, "recorded_videos")[0][2], "Transient-local")

    def test_readonly_check_rejects_foreign_key_violations(self):
        for path in (self.source, self.destination):
            with connect(path) as connection:
                connection.execute("UPDATE recorded_videos SET recorded_program_id=999")
        before = {path: path.read_bytes() for path in (self.source, self.destination)}
        with self.assertRaises(rc.RefreshError) as error:
            rc.check_catalog(self.source, self.destination)
        self.assertEqual(error.exception.code, "foreign_key_violation")
        self.assertEqual({path: path.read_bytes() for path in before}, before)
        self.assertEqual(self.runner.calls, [])
        self.assertFalse(rc.recovery_path(self.destination).exists())

    def test_unchanged_refresh_rejects_foreign_key_violations(self):
        for path in (self.source, self.destination):
            with connect(path) as connection:
                connection.execute("UPDATE recorded_videos SET recorded_program_id=999")
        before = {path: path.read_bytes() for path in (self.source, self.destination)}
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "foreign_key_violation")
        self.assertEqual({path: path.read_bytes() for path in before}, before)
        self.assertEqual(self.runner.calls, [])
        self.assertFalse(rc.recovery_path(self.destination).exists())

    def test_source_only_foreign_key_violation_is_rejected_by_check(self):
        with connect(self.source) as connection:
            connection.execute("UPDATE recorded_videos SET recorded_program_id=999")
        with self.assertRaises(rc.RefreshError) as error:
            rc.check_catalog(self.source, self.destination)
        self.assertEqual(error.exception.code, "foreign_key_violation")
        self.assertEqual(self.runner.calls, [])

    def test_destination_only_foreign_key_violation_is_rejected_by_check(self):
        with connect(self.destination) as connection:
            connection.execute("UPDATE recorded_videos SET recorded_program_id=999")
        with self.assertRaises(rc.RefreshError) as error:
            rc.check_catalog(self.source, self.destination)
        self.assertEqual(error.exception.code, "foreign_key_violation")
        self.assertEqual(self.runner.calls, [])

    def test_unchanged_refresh_rejects_local_foreign_key_violation(self):
        with connect(self.destination) as connection:
            connection.execute("UPDATE captures SET recorded_program_id=999")
        before = self.destination.read_bytes()
        with self.assertRaises(rc.RefreshError) as error:
            self.refresh()
        self.assertEqual(error.exception.code, "foreign_key_violation")
        self.assertEqual(self.runner.calls, [])
        self.assertEqual(self.destination.read_bytes(), before)

    def test_no_change_never_stops_service(self):
        before = self.destination.read_bytes()
        result = self.refresh()
        self.assertEqual(result["status"], "unchanged")
        self.assertEqual(self.runner.calls, [])
        self.assertEqual(self.destination.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()