"""下载/认证逻辑的单元测试（mock session，无网络）。"""

import hashlib

import pytest
import requests

import docker_image_puller as m


# ---------------------------------------------------------------------------
# fakes
# ---------------------------------------------------------------------------
class FakeResponse:
    """模拟 requests.Response：支持 with 语法、iter_content、json、raise_for_status。"""

    def __init__(self, status_code=200, content=b"", headers=None, json_data=None):
        self.status_code = status_code
        self._content = content
        self.headers = headers or {}
        self._json_data = json_data

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def raise_for_status(self):
        if self.status_code >= 400:
            raise requests.exceptions.HTTPError(response=self)

    def iter_content(self, chunk_size=1):
        for i in range(0, len(self._content), chunk_size):
            yield self._content[i:i + chunk_size]

    def json(self):
        if self._json_data is not None:
            return self._json_data
        import json
        return json.loads(self._content)


class FakeSession:
    """模拟 requests.Session。get 可传响应列表/单个响应/callable；head 同理。"""

    def __init__(self, get_responses=None, head_response=None):
        self._get = get_responses
        self._head = head_response

    def get(self, url, headers=None, verify=False, timeout=None, stream=False, **kw):
        if isinstance(self._get, list):
            return self._get.pop(0)
        if callable(self._get):
            return self._get(url, headers)
        return self._get

    def head(self, url, headers=None, verify=False, timeout=None, **kw):
        if callable(self._head):
            return self._head(url, headers)
        return self._head


def _digest(data: bytes) -> str:
    return "sha256:" + hashlib.sha256(data).hexdigest()


def _range_getter(content: bytes):
    """返回一个 callable(url, headers)，按 Range 头返回对应分片（206）。"""

    def _get(url, headers):
        rng = headers.get("Range", "")
        s, e = rng.replace("bytes=", "").split("-")
        s, e = int(s), int(e)
        chunk = content[s:e + 1]
        return FakeResponse(
            206, chunk,
            {"content-length": str(len(chunk)),
             "content-range": f"bytes {s}-{e}/{len(content)}"},
        )

    return _get


# ---------------------------------------------------------------------------
# download_file_with_progress
# ---------------------------------------------------------------------------
def test_download_fresh_success(tmp_path):
    content = b"hello world data" * 10
    save = tmp_path / "layer.tar"
    session = FakeSession(FakeResponse(200, content, {"content-length": str(len(content))}))

    ok = m.download_file_with_progress(
        session, "http://x/blob", {}, str(save), "desc",
        expected_digest=_digest(content), max_retries=1,
    )

    assert ok
    assert save.read_bytes() == content


def test_download_resume_with_206(tmp_path):
    full = b"0123456789" * 100  # 1000
    save = tmp_path / "layer.tar"
    save.write_bytes(full[:400])  # 已存在半截

    rest = full[400:]
    resp = FakeResponse(206, rest, {
        "content-length": str(len(rest)),
        "content-range": f"bytes 400-{len(full) - 1}/{len(full)}",
    })
    session = FakeSession(resp)

    ok = m.download_file_with_progress(
        session, "http://x/blob", {}, str(save), "desc",
        expected_digest=_digest(full), max_retries=1,
    )

    assert ok
    assert save.read_bytes() == full


def test_download_resume_ignored_range(tmp_path):
    """服务器忽略 Range 返回 200 全文，应从头下载而非追加损坏。"""
    full = b"abcdefghij" * 100  # 1000
    save = tmp_path / "layer.tar"
    save.write_bytes(full[:400])  # 半截

    # 200 + 无 content-range = 服务器未支持续传
    resp = FakeResponse(200, full, {"content-length": str(len(full))})
    session = FakeSession(resp)

    ok = m.download_file_with_progress(
        session, "http://x/blob", {}, str(save), "desc",
        expected_digest=_digest(full), max_retries=1,
    )

    assert ok
    assert save.read_bytes() == full  # 不是 partial + full


def test_download_416_marks_complete(tmp_path):
    full = b"xyz" * 10
    save = tmp_path / "layer.tar"
    save.write_bytes(full)  # 已完整

    session = FakeSession(FakeResponse(416, b"", {}))
    ok = m.download_file_with_progress(
        session, "http://x/blob", {}, str(save), "desc",
        expected_digest=_digest(full), max_retries=1,
    )

    assert ok
    assert save.read_bytes() == full


def test_download_digest_mismatch_returns_false(tmp_path):
    content = b"wrong content"
    save = tmp_path / "layer.tar"
    session = FakeSession(FakeResponse(200, content, {"content-length": str(len(content))}))

    ok = m.download_file_with_progress(
        session, "http://x/blob", {}, str(save), "desc",
        expected_digest=_digest(b"right content"), max_retries=1,
    )

    assert not ok
    assert not save.exists()  # 校验失败应删除


def test_download_connection_error_exhausted(tmp_path):
    save = tmp_path / "layer.tar"

    def raising(url, headers):
        raise requests.exceptions.ConnectionError("boom")

    session = FakeSession(raising)
    ok = m.download_file_with_progress(
        session, "http://x/blob", {}, str(save), "desc", max_retries=1,
    )
    assert not ok


def test_download_stop_event_aborts(tmp_path):
    save = tmp_path / "layer.tar"
    m.stop_event.set()
    session = FakeSession(FakeResponse(200, b"data", {"content-length": "4"}))
    ok = m.download_file_with_progress(
        session, "http://x/blob", {}, str(save), "desc", max_retries=1,
    )
    assert not ok


# ---------------------------------------------------------------------------
# download_file_in_chunks
# ---------------------------------------------------------------------------
def test_download_in_chunks_success(tmp_path):
    content = b"0123456789" * 100  # 1000, chunk_size=400 -> 3 chunks
    save = tmp_path / "blob"
    session = FakeSession(_range_getter(content))

    ok = m.download_file_in_chunks(
        session, "http://x/blob", {}, str(save), "desc",
        total_size=len(content), expected_digest=_digest(content),
        max_retries=1, chunk_size=400, workers=2,
    )

    assert ok
    assert save.read_bytes() == content


def test_download_in_chunks_digest_mismatch(tmp_path):
    content = b"0123456789" * 100
    save = tmp_path / "blob"
    session = FakeSession(_range_getter(content))

    ok = m.download_file_in_chunks(
        session, "http://x/blob", {}, str(save), "desc",
        total_size=len(content), expected_digest=_digest(b"different"),
        max_retries=1, chunk_size=400, workers=2,
    )

    assert not ok
    assert not save.exists()


# ---------------------------------------------------------------------------
# get_auth_head
# ---------------------------------------------------------------------------
def test_get_auth_head_token():
    session = FakeSession(FakeResponse(200, b"", json_data={"token": "abc"}))
    head = m.get_auth_head(session, "http://auth", "svc", "repo")
    assert head["Authorization"] == "Bearer abc"
    assert "manifest.v2" in head["Accept"]


def test_get_auth_head_access_token_fallback():
    session = FakeSession(FakeResponse(200, b"", json_data={"access_token": "xyz"}))
    head = m.get_auth_head(session, "http://auth", "svc", "repo")
    assert head["Authorization"] == "Bearer xyz"


def test_get_auth_head_missing_token_raises():
    session = FakeSession(FakeResponse(200, b"", json_data={}))
    with pytest.raises(requests.exceptions.RequestException):
        m.get_auth_head(session, "http://auth", "svc", "repo")


# ---------------------------------------------------------------------------
# get_file_size
# ---------------------------------------------------------------------------
def test_get_file_size_ok():
    session = FakeSession(head_response=FakeResponse(200, b"", {"content-length": "12345"}))
    assert m.get_file_size(session, "http://x/blob", {}) == 12345


def test_get_file_size_non_200():
    session = FakeSession(head_response=FakeResponse(404, b"", {}))
    assert m.get_file_size(session, "http://x/blob", {}) == 0


def test_get_file_size_failure():
    def raising(url, headers):
        raise requests.exceptions.ConnectionError("x")

    session = FakeSession(head_response=raising)
    assert m.get_file_size(session, "http://x/blob", {}) == 0


# ---------------------------------------------------------------------------
# registry_api / ImageInfo
# ---------------------------------------------------------------------------
def test_registry_api_https_by_default():
    m._use_http_registry = False
    assert m.registry_api("reg.io", "/v2/") == "https://reg.io/v2/"


def test_registry_api_http_when_insecure():
    m._use_http_registry = True
    assert m.registry_api("reg.io", "/v2/") == "http://reg.io/v2/"


def test_image_info_registry_url():
    ii = m.ImageInfo("reg.io", "repo", "img", "tag")
    assert ii.registry_url("/v2/") == "https://reg.io/v2/"
    ii.use_http = True
    assert ii.registry_url("/v2/") == "http://reg.io/v2/"
