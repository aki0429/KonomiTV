"""Catalog-only SQLite refresh for a parallel KonomiTV instance (stdlib)."""
import argparse
from contextlib import closing, contextmanager
import hashlib
import json
import os
import shutil
from pathlib import Path
import sqlite3
import struct
import subprocess
import time
import urllib.request

CATALOG_TABLES = (
    "channels", "programs", "series", "series_broadcast_periods",
    "recorded_programs", "recorded_videos",
)


class RefreshError(RuntimeError):
    """A safe, non-row-bearing operational error."""
    def __init__(self, code, *, rollback_ok=False, service_recovered=False, recovery_dir=None):
        super().__init__(code)
        self.code = code
        self.rollback_ok = rollback_ok
        self.service_recovered = service_recovered
        self.recovery_dir = recovery_dir


def require_distinct_databases(source, destination):
    """Resolve filesystem identity, including hardlinks; uncertainty is unsafe."""
    try:
        same = os.path.samefile(source, destination)
    except OSError:
        raise RefreshError("database_identity_unavailable") from None
    if same:
        raise RefreshError("source_destination_alias")


def open_readonly(path):
    connection = sqlite3.connect(Path(path).resolve().as_uri() + "?mode=ro", uri=True)
    connection.execute("PRAGMA query_only = ON")
    return connection


def quote(name):
    return '"' + name.replace('"', '""') + '"'


def fingerprint(connection, table, *, database=None):
    """Type-sensitive, order-independent content digest, including duplicates."""
    row_hashes = []
    qualified = (quote(database) + "." if database is not None else "") + quote(table)
    for row in connection.execute("SELECT * FROM " + qualified):
        digest = hashlib.sha256()
        for value in row:
            if value is None:
                kind, payload = b"n", b""
            elif isinstance(value, int):
                kind, payload = b"i", str(value).encode("ascii")
            elif isinstance(value, float):
                kind, payload = b"f", struct.pack(">d", value)
            elif isinstance(value, str):
                kind, payload = b"s", value.encode("utf-8", "surrogatepass")
            else:
                kind, payload = b"b", bytes(value)
            digest.update(kind + struct.pack(">Q", len(payload)) + payload)
        row_hashes.append(digest.digest())
    digest = hashlib.sha256()
    digest.update(struct.pack(">Q", len(row_hashes)))
    for value in sorted(row_hashes):
        digest.update(value)
    return digest.hexdigest()


def fingerprints(connection, *, database=None):
    return {table: (fingerprint(connection, table) if database is None else
                    fingerprint(connection, table, database=database)) for table in CATALOG_TABLES}


def catalog_schema(connection):
    metadata = {}
    for table in CATALOG_TABLES:
        definition = connection.execute(
            "SELECT sql FROM sqlite_schema WHERE type='table' AND name=?", (table,)
        ).fetchone()
        if definition is None:
            raise RefreshError("catalog_table_missing")
        indexes = connection.execute("PRAGMA index_list(" + quote(table) + ")").fetchall()
        metadata[table] = (
            definition[0],
            connection.execute("PRAGMA table_xinfo(" + quote(table) + ")").fetchall(),
            connection.execute("PRAGMA foreign_key_list(" + quote(table) + ")").fetchall(),
            sorted((row[1:], connection.execute("PRAGMA index_xinfo(" + quote(row[1]) + ")").fetchall(),
                    connection.execute("SELECT sql FROM sqlite_schema WHERE type='index' AND name=?",
                                       (row[1],)).fetchone()) for row in indexes),
            connection.execute("SELECT name, sql FROM sqlite_schema WHERE type='trigger' AND tbl_name=? "
                               "ORDER BY name", (table,)).fetchall(),
        )
    return metadata


def require_compatible(source, destination):
    source_metadata = catalog_schema(source)
    destination_metadata = catalog_schema(destination)
    if source_metadata != destination_metadata:
        raise RefreshError("catalog_schema_mismatch")
    if any(entry[-1] for entry in source_metadata.values()):
        raise RefreshError("catalog_triggers_not_supported")


def run_command(argv, *, timeout):
    return subprocess.run(argv, timeout=timeout, capture_output=True, text=True, check=False)


def command(runner, action):
    try:
        result = runner(["pm2", action, "konomitv-go"] if action != "jlist" else ["pm2", "jlist"],
                        timeout=30)
        if result.returncode:
            raise RefreshError("service_" + action + "_failed")
        return result
    except RefreshError:
        raise
    except Exception:
        raise RefreshError("service_" + action + "_failed") from None


def service_status(runner):
    result = command(runner, "jlist")
    try:
        matches = [item for item in json.loads(result.stdout) if item.get("name") == "konomitv-go"]
        if len(matches) != 1:
            raise ValueError
        item = matches[0]
        return item["pm2_env"]["status"], int(item.get("pid", 0))
    except (ValueError, KeyError, TypeError):
        raise RefreshError("service_status_invalid") from None


def stop_service(runner):
    command(runner, "stop")
    status, pid = service_status(runner)
    if status != "stopped" or pid != 0:
        raise RefreshError("service_not_stopped")


def http_health():
    try:
        with urllib.request.urlopen("http://127.0.0.1:7002/api/videos?page=1", timeout=10) as response:
            return response.status == 200
    except Exception:
        return False


def verify_service(runner, health_check, attempts, interval):
    for attempt in range(attempts):
        try:
            status, pid = service_status(runner)
            if status == "online" and pid > 0 and health_check():
                return True
        except Exception:
            pass
        if attempt + 1 < attempts:
            time.sleep(interval)
    return False


def open_writable(path):
    return sqlite3.connect(Path(path).resolve().as_uri() + "?mode=rw", uri=True, isolation_level=None)


def backup(source, destination):
    with closing(open_readonly(source)) as original, closing(sqlite3.connect(destination)) as copy:
        original.backup(copy)
        if copy.execute("PRAGMA integrity_check").fetchall() != [("ok",)]:
            raise RefreshError("snapshot_integrity_failed")


def sync_catalog(connection, staged, expected=None, *, commit_state=None, rollback_snapshot=None,
                 staged_expected=None):
    """Change six tables only; do not fire cascades into local-owned tables."""
    connection.execute("PRAGMA foreign_keys = OFF")
    connection.execute("ATTACH DATABASE ? AS incoming", (Path(staged).as_uri() + "?mode=ro",))
    def only_catalog_writes(action, table, column, database, origin):
        if action in (sqlite3.SQLITE_INSERT, sqlite3.SQLITE_UPDATE, sqlite3.SQLITE_DELETE):
            if database != "main" or table not in CATALOG_TABLES:
                return sqlite3.SQLITE_DENY
        return sqlite3.SQLITE_OK

    connection.set_authorizer(only_catalog_writes)
    try:
        connection.execute("BEGIN IMMEDIATE")
        with closing(open_readonly(staged)) as snapshot:
            require_compatible(snapshot, connection)
        if expected is not None and fingerprints(connection) != expected:
            raise RefreshError("destination_catalog_changed")
        if staged_expected is not None and fingerprints(connection, database="incoming") != staged_expected:
            raise RefreshError("rollback_baseline_changed")
        if rollback_snapshot is not None:
            with closing(open_readonly(rollback_snapshot)) as previous:
                previous.execute("BEGIN")
                require_compatible(previous, connection)
                if fingerprints(previous) != expected:
                    raise RefreshError("rollback_baseline_changed")
        for table in reversed(CATALOG_TABLES):
            connection.execute("DELETE FROM main." + quote(table))
        for table in CATALOG_TABLES:
            connection.execute("INSERT INTO main." + quote(table) +
                               " SELECT * FROM incoming." + quote(table))
        if connection.execute("PRAGMA main.foreign_key_check").fetchone() is not None:
            raise RefreshError("foreign_key_violation")
        if connection.execute("PRAGMA main.integrity_check").fetchall() != [("ok",)]:
            raise RefreshError("merged_integrity_failed")
        # Record uncertainty BEFORE COMMIT. Only its normal return proves ownership;
        # an interrupt in the return/assignment gap leaves recovery fail-closed.
        if commit_state is not None:
            commit_state["state"] = "unknown"
        connection.commit()
        if commit_state is not None:
            commit_state["state"] = "committed"
    except BaseException:
        connection.rollback()
        raise
    finally:
        connection.set_authorizer(None)
        connection.execute("DETACH DATABASE incoming")


@contextmanager
def exclusive_lock(path):
    descriptor = os.open(path, os.O_RDWR | os.O_CREAT, 0o600)
    handle = os.fdopen(descriptor, "r+b", buffering=0)
    locked = False
    try:
        if os.fstat(descriptor).st_size == 0:
            handle.write(b"0")
        handle.seek(0)
        try:
            if os.name == "nt":
                import msvcrt
                msvcrt.locking(descriptor, msvcrt.LK_NBLCK, 1)
            else:
                import fcntl
                fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            locked = True
        except OSError:
            raise RefreshError("lock_busy") from None
        yield
    finally:
        if locked:
            handle.seek(0)
            if os.name == "nt":
                import msvcrt
                msvcrt.locking(descriptor, msvcrt.LK_UNLCK, 1)
            else:
                import fcntl
                fcntl.flock(descriptor, fcntl.LOCK_UN)
        handle.close()
        # Never unlink the inode: unlinking a flock file permits two owners.


def refresh_catalog(source, destination, *, runner=None, health_check=None,
                    verify_attempts=5, verify_interval=1):
    require_distinct_databases(source, destination)
    destination = Path(destination).resolve()
    with exclusive_lock(destination.with_name(destination.name + ".catalog-refresh.lock")):
        return _refresh_catalog(source, destination, runner=runner, health_check=health_check,
                                verify_attempts=verify_attempts, verify_interval=verify_interval)


def recovery_path(destination):
    return destination.with_name(destination.name + ".catalog-recovery")


def durable_json(path, data):
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("w", encoding="utf-8") as handle:
        json.dump(data, handle, sort_keys=True)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)


def _refresh_catalog(source, destination, *, runner=None, health_check=None,
                     verify_attempts=5, verify_interval=1):
    require_distinct_databases(source, destination)
    source, destination = Path(source).resolve(), Path(destination).resolve()
    work = recovery_path(destination)
    if work.exists():
        if (work / "pending.json").exists():
            raise RefreshError("pending_recovery", recovery_dir=str(work))
        raise RefreshError("stale_staging_requires_review", recovery_dir=str(work))
    work.mkdir(mode=0o700)
    keep = False
    try:
        staged = Path(work) / "source.sqlite"
        try:
            backup(source, staged)
            with closing(open_readonly(staged)) as snapshot:
                incoming = fingerprints(snapshot)
        except (sqlite3.Error, OSError):
            raise RefreshError("source_read_failed") from None
        with closing(open_readonly(destination)) as current, closing(open_readonly(staged)) as snapshot:
            current.execute("BEGIN")
            snapshot.execute("BEGIN")
            require_compatible(snapshot, current)
            for connection in (snapshot, current):
                if connection.execute("PRAGMA foreign_key_check").fetchone() is not None:
                    raise RefreshError("foreign_key_violation")
            existing = fingerprints(current)
        if incoming == existing:
            return {"status": "unchanged"}
        previous = Path(work) / "previous.sqlite"
        candidate = Path(work) / "candidate.sqlite"
        try:
            backup(destination, previous)
            backup(previous, candidate)
            with closing(open_writable(candidate)) as trial:
                sync_catalog(trial, staged)
        except (sqlite3.Error, OSError):
            raise RefreshError("destination_staging_failed") from None
        runner = runner or run_command
        health_check = health_check or http_health
        status, pid = service_status(runner)
        if status != "online" or pid <= 0:
            raise RefreshError("service_not_online")
        require_distinct_databases(source, destination)
        durable_json(work / "pending.json", {"destination": str(destination), "phase": "prepared"})
        # Keep the durable recovery set on interrupts/crashes after this point.
        keep = True
        commit_state = {"state": "before_commit"}
        try:
            stop_service(runner)
            require_distinct_databases(source, destination)
            with closing(open_writable(destination)) as target:
                sync_catalog(target, staged, existing, commit_state=commit_state,
                             rollback_snapshot=previous)
            command(runner, "start")
            if not verify_service(runner, health_check, verify_attempts, verify_interval):
                raise RefreshError("restart_verification_failed")
            keep = False
            return {"status": "updated"}
        except BaseException as failure:
            if isinstance(failure, (KeyboardInterrupt, SystemExit)):
                code = "interrupted"
            else:
                code = failure.code if isinstance(failure, RefreshError) else "catalog_update_failed"
            # Content equality is not evidence of our COMMIT: another writer can
            # independently produce incoming. Unknown outcomes retain recovery.
            rollback_ok = commit_state["state"] == "before_commit"
            recovered = False
            try:
                if commit_state["state"] == "committed":
                    require_distinct_databases(source, destination)
                    stop_service(runner)
                    require_distinct_databases(source, destination)
                    with closing(open_writable(destination)) as target:
                        sync_catalog(target, previous, incoming, staged_expected=existing)
                    rollback_ok = True
                status, pid = service_status(runner)
                if status != "online" or pid <= 0:
                    command(runner, "start")
                recovered = verify_service(runner, health_check, verify_attempts, verify_interval)
            except BaseException:
                pass
            keep = not (rollback_ok and recovered)
            raise RefreshError(code, rollback_ok=rollback_ok, service_recovered=recovered,
                               recovery_dir=str(work) if keep else None) from None
    finally:
        if not keep:
            shutil.rmtree(work)


def check_catalog(source, destination):
    """Read-only, per-database consistent check. Never invokes PM2 or creates DBs."""
    require_distinct_databases(source, destination)
    with closing(open_readonly(source)) as original, closing(open_readonly(destination)) as current:
        original.execute("BEGIN")
        current.execute("BEGIN")
        require_compatible(original, current)
        for connection in (original, current):
            if connection.execute("PRAGMA integrity_check").fetchall() != [("ok",)]:
                raise RefreshError("integrity_failed")
            if connection.execute("PRAGMA foreign_key_check").fetchone() is not None:
                raise RefreshError("foreign_key_violation")
        incoming, existing = fingerprints(original), fingerprints(current)
        changed = [table for table in CATALOG_TABLES if incoming[table] != existing[table]]
        return {"status": "change_available" if changed else "unchanged", "changed_tables": changed}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, default=Path("/opt/KonomiTV/server/data/database.sqlite"))
    parser.add_argument("--destination", type=Path,
                        default=Path.home() / "konomitv-go/server/data/database.sqlite")
    parser.add_argument("--check", action="store_true", help="read-only schema/integrity/content check; no PM2")
    parser.add_argument("--verify-attempts", type=int, default=5)
    parser.add_argument("--verify-interval", type=float, default=1)
    arguments = parser.parse_args(argv)
    try:
        result = (check_catalog(arguments.source, arguments.destination) if arguments.check else
                  refresh_catalog(arguments.source, arguments.destination,
                                  verify_attempts=arguments.verify_attempts,
                                  verify_interval=arguments.verify_interval))
        print(json.dumps(result, sort_keys=True))
        return 0
    except RefreshError as error:
        print(json.dumps({"status": "failed", "code": error.code, "rollback_ok": error.rollback_ok,
                          "service_recovered": error.service_recovered,
                          "recovery_dir": error.recovery_dir}, sort_keys=True))
        return 75 if error.code == "lock_busy" else 1
    except Exception:
        # DB rows, PM2 environment, exception text and HTTP bodies are never logged.
        print(json.dumps({"status": "failed", "code": "operation_failed"}, sort_keys=True))
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
