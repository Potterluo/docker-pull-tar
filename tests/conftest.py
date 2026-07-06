"""pytest 共享夹具：本地 mock Docker registry v2 server 与合成镜像。"""

import base64
import gzip
import hashlib
import io
import json
import socket
import tarfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest


class _RegistryHandler(BaseHTTPRequestHandler):
    """极简 Docker registry v2 实现：服务 /v2/ ping、manifests、blobs。"""

    def log_message(self, *args):  # 静默
        pass

    def do_GET(self):  # noqa: N802
        self._handle(head=False)

    def do_HEAD(self):  # noqa: N802
        self._handle(head=True)

    # ---- helpers ----
    def _check_basic(self):
        srv = self.server
        header = self.headers.get("Authorization", "")
        if not header.startswith("Basic "):
            return False
        try:
            decoded = base64.b64decode(header[6:]).decode()
            user, _, pw = decoded.partition(":")
        except Exception:
            return False
        return user == srv.basic_user and pw == srv.basic_pass

    def _send(self, code, body=b"", extra=None):
        self.send_response(code)
        if extra:
            for k, v in extra.items():
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body and self.command != "HEAD":
            self.wfile.write(body)

    # ---- main dispatch ----
    def _handle(self, head):
        srv = self.server
        path = self.path

        # /v2/ ping
        if path.rstrip("/") == "/v2":
            if srv.auth == "basic" and not self._check_basic():
                self._send(401, b"", {"WWW-Authenticate": 'Basic realm="registry"'})
                return
            self._send(200)
            return

        if not path.startswith("/v2/"):
            self._send(404)
            return

        rest = path[len("/v2/"):]
        if "/manifests/" in rest:
            repo, ref = rest.split("/manifests/", 1)
            body = srv.manifests.get((repo, ref))
            content_type = "application/vnd.docker.distribution.manifest.v2+json"
        elif "/blobs/" in rest:
            repo, digest = rest.split("/blobs/", 1)
            body = srv.blobs.get(digest)
            content_type = "application/octet-stream"
        else:
            self._send(404)
            return

        if srv.auth == "basic" and not self._check_basic():
            self._send(401, b"", {"WWW-Authenticate": 'Basic realm="registry"'})
            return
        if body is None:
            self._send(404)
            return
        self._send(200, body, {"Content-Type": content_type})


class MockRegistry:
    """对 running 中的 mock registry 的访问句柄。"""

    def __init__(self, server: ThreadingHTTPServer):
        self._server = server
        port = server.server_address[1]
        self.host = f"127.0.0.1:{port}"
        self.base = f"http://{self.host}"

    def add_blob(self, digest: str, body: bytes):
        self._server.blobs[digest] = body

    def add_manifest(self, repo: str, ref: str, body: bytes):
        self._server.manifests[(repo, ref)] = body

    def set_basic_auth(self, user: str, password: str):
        self._server.auth = "basic"
        self._server.basic_user = user
        self._server.basic_pass = password

    def stop(self):
        self._server.shutdown()
        self._server.server_close()


@pytest.fixture
def mock_registry():
    """启动一个线程化的本地 registry，返回 MockRegistry 句柄。"""
    server = ThreadingHTTPServer(("127.0.0.1", 0), _RegistryHandler)
    server.blobs = {}
    server.manifests = {}
    server.auth = None
    server.basic_user = "user"
    server.basic_pass = "pass"
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    reg = MockRegistry(server)
    yield reg
    reg.stop()


def build_image(repo="repo", tag="tag", arch="amd64",
                file_name="hello.txt", file_content="hello world"):
    """构造一个最小可用的单架构镜像（config + 单层 gzip tar）。

    返回 dict，包含各 blob 字节、digest、manifest 字节等，供 mock registry 注册。
    """
    # 1) 层：未压缩 tar -> gzip
    tbuf = io.BytesIO()
    with tarfile.open(fileobj=tbuf, mode="w") as t:
        data = file_content.encode()
        info = tarfile.TarInfo(file_name)
        info.size = len(data)
        t.addfile(info, io.BytesIO(data))
    raw_tar = tbuf.getvalue()
    layer_gz = gzip.compress(raw_tar)
    layer_digest = "sha256:" + hashlib.sha256(layer_gz).hexdigest()
    diff_id = "sha256:" + hashlib.sha256(raw_tar).hexdigest()

    # 2) config blob
    config = {
        "architecture": arch,
        "os": "linux",
        "config": {},
        "rootfs": {"type": "layers", "diff_ids": [diff_id]},
    }
    config_bytes = json.dumps(config).encode()
    config_digest = "sha256:" + hashlib.sha256(config_bytes).hexdigest()

    # 3) 单架构 manifest (v2)
    manifest = {
        "schemaVersion": 2,
        "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
        "config": {
            "mediaType": "application/vnd.docker.container.image.v1+json",
            "digest": config_digest,
            "size": len(config_bytes),
        },
        "layers": [
            {
                "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
                "digest": layer_digest,
                "size": len(layer_gz),
            }
        ],
    }
    manifest_bytes = json.dumps(manifest).encode()

    return {
        "repo": repo,
        "tag": tag,
        "arch": arch,
        "file_name": file_name,
        "file_content": file_content,
        "layer_raw_tar": raw_tar,
        "layer_gz": layer_gz,
        "layer_digest": layer_digest,
        "config_bytes": config_bytes,
        "config_digest": config_digest,
        "manifest_bytes": manifest_bytes,
        "manifest_digest": "sha256:" + hashlib.sha256(manifest_bytes).hexdigest(),
    }


def register_single_arch(reg: MockRegistry, img: dict):
    """把合成单架构镜像注册到 mock registry。"""
    reg.add_blob(img["config_digest"], img["config_bytes"])
    reg.add_blob(img["layer_digest"], img["layer_gz"])
    reg.add_manifest(img["repo"], img["tag"], img["manifest_bytes"])


def register_manifest_list(reg: MockRegistry, img: dict):
    """把同一镜像以 manifest list（多架构）形式注册，含一个 linux/{arch} 入口。"""
    sub_digest = "sha256:" + hashlib.sha256(img["manifest_bytes"]).hexdigest()
    reg.add_manifest(img["repo"], sub_digest, img["manifest_bytes"])
    index = {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.index.v1+json",
        "manifests": [
            {
                "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
                "digest": sub_digest,
                "size": len(img["manifest_bytes"]),
                "platform": {"architecture": img["arch"], "os": "linux"},
            }
        ],
    }
    reg.add_blob(img["config_digest"], img["config_bytes"])
    reg.add_blob(img["layer_digest"], img["layer_gz"])
    reg.add_manifest(img["repo"], img["tag"], json.dumps(index).encode())
    return sub_digest


def register_multi_arch(reg: MockRegistry, images):
    """注册一个含多个架构的 manifest list。images: [(arch, img_dict), ...]。"""
    entries = []
    for arch, img in images:
        sub_digest = "sha256:" + hashlib.sha256(img["manifest_bytes"]).hexdigest()
        reg.add_manifest(img["repo"], sub_digest, img["manifest_bytes"])
        reg.add_blob(img["config_digest"], img["config_bytes"])
        reg.add_blob(img["layer_digest"], img["layer_gz"])
        entries.append({
            "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
            "digest": sub_digest,
            "size": len(img["manifest_bytes"]),
            "platform": {"architecture": arch, "os": "linux"},
        })
    repo = images[0][1]["repo"]
    tag = images[0][1]["tag"]
    index = {
        "schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.index.v1+json",
        "manifests": entries,
    }
    reg.add_manifest(repo, tag, json.dumps(index).encode())


@pytest.fixture(autouse=True)
def _reset_globals():
    """每个测试前后重置模块级全局状态，避免相互污染。"""
    import docker_image_puller as m
    m.stop_event.clear()
    m._use_http_registry = False
    m.progress_display = m.ProgressDisplay(ci_mode=True)  # 静默动画
    # SessionManager 是单例，重置以隔离代理等配置
    m.SessionManager._instance = None
    yield
    m.stop_event.clear()
