"""Python サーバー・既存 DB なしで Go の新規起動とアカウントを検証する開発用ハーネス。"""

import argparse
import json
from pathlib import Path
import socket
import subprocess
import time
from urllib.error import URLError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("binary", type=Path)
parser.add_argument("work_dir", type=Path)
args = parser.parse_args()
root = args.work_dir.resolve()
root.mkdir(parents=True, exist_ok=False)
server_dir = root / "server"
server_dir.mkdir()
(root / "config.yaml").write_text("""general:
  backend: IPTV
  encoder: FFmpeg
  program_update_interval: 15
server:
  port: 17018
video:
  recorded_folders: []
capture:
  upload_folders: []
iptv:
  enabled: false
  sources: []
""", encoding="utf-8")
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
base = f"http://127.0.0.1:{port}"


def request(method, path, data=None, token=None, form=False):
    headers = {}
    if data is not None:
        headers["Content-Type"] = "application/x-www-form-urlencoded" if form else "application/json"
        data = (urlencode(data) if form else json.dumps(data)).encode()
    if token:
        headers["Authorization"] = "Bearer " + token
    with urlopen(Request(base + path, data=data, headers=headers, method=method), timeout=10) as response:
        body = response.read()
        return response.status, json.loads(body) if body else None


with (root / "server.log").open("wb") as log:
    child = subprocess.Popen(
        [str(args.binary.resolve()), "-no-proxy", "-listen", f"127.0.0.1:{port}"],
        cwd=root, stdout=log, stderr=subprocess.STDOUT,
        creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
    )
    try:
        for _ in range(100):
            if child.poll() is not None:
                raise RuntimeError("Go exited before serving requests; inspect server.log")
            try:
                status, _ = request("GET", "/api/channels")
                if status == 200:
                    break
            except URLError:
                time.sleep(0.1)
        else:
            raise RuntimeError("Go did not become ready")
        status, user = request("POST", "/api/users", {"username": "fresh-user", "password": "synthetic-test-password"})
        assert status == 201 and user["is_admin"] is True, (status, user)
        status, auth = request("POST", "/api/users/token", {"username": "fresh-user", "password": "synthetic-test-password"}, form=True)
        assert status == 200
        status, me = request("GET", "/api/users/me", token=auth["access_token"])
        assert status == 200 and me["name"] == "fresh-user"
        status, users = request("GET", "/api/users", token=auth["access_token"])
        assert status == 200 and len(users) == 1
        status, _ = request("POST", "/api/maintenance/shutdown", token=auth["access_token"])
        assert status == 204
        child.wait(timeout=10)
        assert child.returncode == 0, child.returncode
        assert (server_dir / "data/database.sqlite").is_file()
        assert not (server_dir / "app").exists()
        result = {"fresh_database": True, "registration": True, "first_user_admin": True, "login": True, "authenticated_read": True, "graceful_shutdown": True, "python_server_source_present": False}
        (root / "result.json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(result))
    finally:
        if child.poll() is None:
            child.kill()
            child.wait(timeout=5)
