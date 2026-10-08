"""docker_image_puller 核心纯函数的单元测试（不涉及网络）。"""

import base64
import json

import docker_image_puller as m


# ---------------------------------------------------------------------------
# parse_image_input
# ---------------------------------------------------------------------------
def test_parse_dockerhub_official():
    ii = m.parse_image_input("nginx:latest")
    assert ii.registry == "registry-1.docker.io"
    assert ii.repository == "library/nginx"
    assert ii.image_name == "nginx"
    assert ii.tag == "latest"


def test_parse_dockerhub_namespaced():
    ii = m.parse_image_input("user/repo:1.0")
    assert ii.registry == "registry-1.docker.io"
    assert ii.repository == "user/repo"
    assert ii.image_name == "repo"
    assert ii.tag == "1.0"


def test_parse_default_tag():
    ii = m.parse_image_input("nginx")
    assert ii.tag == "latest"
    assert ii.repository == "library/nginx"


def test_parse_localhost_is_registry():
    # 回归：localhost 不应被当作 Docker Hub 命名空间
    ii = m.parse_image_input("localhost/nginx:latest")
    assert ii.registry == "localhost"
    assert ii.repository == "nginx"


def test_parse_localhost_with_port():
    ii = m.parse_image_input("localhost:5000/repo:1.0")
    assert ii.registry == "localhost:5000"
    assert ii.repository == "repo"


def test_parse_host_with_port():
    ii = m.parse_image_input("harbor.abc.com:5000/lib/nginx:1.26")
    assert ii.registry == "harbor.abc.com:5000"
    assert ii.repository == "lib/nginx"
    assert ii.tag == "1.26"


def test_parse_ghcr():
    ii = m.parse_image_input("ghcr.io/owner/repo:1.0")
    assert ii.registry == "ghcr.io"
    assert ii.repository == "owner/repo"


def test_parse_custom_registry_override():
    ii = m.parse_image_input("nginx:latest", custom_registry="myreg.com")
    assert ii.registry == "myreg.com"
    assert ii.repository == "library/nginx"


def test_parse_mcr_with_registry_flag():
    # 对应 issue #18 的命令：dotnet/aspnet:9.0 -r mcr.microsoft.com
    ii = m.parse_image_input("dotnet/aspnet:9.0", custom_registry="mcr.microsoft.com")
    assert ii.registry == "mcr.microsoft.com"
    assert ii.repository == "dotnet/aspnet"
    assert ii.tag == "9.0"


# ---------------------------------------------------------------------------
# resolve_arch_aliases
# ---------------------------------------------------------------------------
def test_resolve_arch_exact():
    assert m.resolve_arch_aliases("amd64", ["amd64", "arm64"]) == "amd64"


def test_resolve_arch_arm64v8():
    assert m.resolve_arch_aliases("arm64v8", ["amd64", "arm64"]) == "arm64"


def test_resolve_arch_x86_64():
    assert m.resolve_arch_aliases("x86_64", ["amd64", "arm64"]) == "amd64"


def test_resolve_arch_linux_slash():
    # issue #18: linux/amd64 应回退到 amd64
    assert m.resolve_arch_aliases("linux/amd64", ["amd64", "arm64"]) == "amd64"


def test_resolve_arch_no_match():
    assert m.resolve_arch_aliases("mips", ["amd64", "arm64"]) is None


# ---------------------------------------------------------------------------
# parse_www_authenticate / basic_auth_header
# ---------------------------------------------------------------------------
def test_parse_www_authenticate_bearer():
    h = 'Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"'
    r = m.parse_www_authenticate(h)
    assert r["scheme"] == "bearer"
    assert r["realm"] == "https://auth.docker.io/token"
    assert r["service"] == "registry.docker.io"
    assert r["scope"] == "repository:library/nginx:pull"


def test_parse_www_authenticate_basic():
    # 回归：原 split('"')[3] 对 Basic 头会 IndexError
    r = m.parse_www_authenticate('Basic realm="Registry"')
    assert r["scheme"] == "basic"
    assert r["realm"] == "Registry"


def test_parse_www_authenticate_empty():
    assert m.parse_www_authenticate("")["scheme"] == "none"


def test_parse_www_authenticate_unknown():
    assert m.parse_www_authenticate("Negotiate")["scheme"] == "unknown"


def test_basic_auth_header():
    h = m.basic_auth_header("user", "pass")
    assert h["Authorization"] == "Basic " + base64.b64encode(b"user:pass").decode()
    assert "manifest.v2" in h["Accept"]


# ---------------------------------------------------------------------------
# get_output_dir / create_image_tar 的架构转义
# ---------------------------------------------------------------------------
def test_get_output_dir_sanitizes_arch(tmp_path):
    d = m.get_output_dir("dotnet/aspnet", "9.0", "linux/amd64", output_path=str(tmp_path))
    assert "linux_amd64" in d.name
    assert "/" not in d.name
    assert d.exists()


def test_get_output_dir_custom_path(tmp_path):
    d = m.get_output_dir("lib/nginx", "latest", "amd64", output_path=str(tmp_path))
    assert d.parent == tmp_path
    assert d.exists()


def test_create_image_tar_sanitizes_arch(tmp_path):
    imgdir = tmp_path / "layers"
    imgdir.mkdir()
    tar = m.create_image_tar(str(imgdir), "dotnet/aspnet", "9.0", "linux/amd64", tmp_path)
    assert tar.endswith("dotnet_aspnet_9.0_linux_amd64.tar")
    assert "/" not in tar.split(tmp_path.name)[-1]


# ---------------------------------------------------------------------------
# DownloadProgressManager
# ---------------------------------------------------------------------------
def test_progress_new(tmp_path):
    pm = m.DownloadProgressManager(tmp_path, "lib/nginx", "latest", "amd64")
    assert not pm.is_layer_completed("sha256:abc")
    assert not pm.is_config_completed()
    assert pm.progress_data["metadata"]["repository"] == "lib/nginx"
    assert pm.progress_data["metadata"]["arch"] == "amd64"


def test_progress_save_load_roundtrip(tmp_path):
    pm = m.DownloadProgressManager(tmp_path, "lib/nginx", "latest", "amd64")
    pm.update_layer_status("sha256:abc", "completed", size=100)
    pm.update_config_status("completed", digest="sha256:cfg")

    pm2 = m.DownloadProgressManager(tmp_path, "lib/nginx", "latest", "amd64")
    assert pm2.is_layer_completed("sha256:abc")
    assert pm2.is_config_completed()


def test_progress_metadata_mismatch_creates_new(tmp_path):
    pm = m.DownloadProgressManager(tmp_path, "lib/nginx", "latest", "amd64")
    pm.update_layer_status("sha256:abc", "completed")

    # 架构不同 -> 元数据不匹配 -> 视为新进度，旧层状态不可用
    pm2 = m.DownloadProgressManager(tmp_path, "lib/nginx", "latest", "arm64")
    assert not pm2.is_layer_completed("sha256:abc")


def test_progress_corrupt_file_falls_back(tmp_path):
    # 损坏的 progress.json 不应导致崩溃
    (tmp_path / "progress.json").write_text("not json", encoding="utf-8")
    pm = m.DownloadProgressManager(tmp_path, "lib/nginx", "latest", "amd64")
    assert not pm.is_layer_completed("sha256:abc")


# ---------------------------------------------------------------------------
# DownloadStats / LayerProgress
# ---------------------------------------------------------------------------
def test_format_size():
    stats = m.DownloadStats()
    assert stats.format_size(0) == "0.0B"
    assert stats.format_size(1024) == "1.0KB"
    assert stats.format_size(1048576) == "1.0MB"
    assert m.LayerProgress.format_size(2048) == "2.0KB"


def test_format_time():
    stats = m.DownloadStats()
    assert stats.format_time(30) == "30秒"
    assert stats.format_time(90) == "1分30秒"
    assert stats.format_time(3700) == "1小时1分"


def test_avg_speed_empty():
    assert m.DownloadStats().get_avg_speed() == 0.0


# ---------------------------------------------------------------------------
# select_manifest
# ---------------------------------------------------------------------------
def test_select_manifest_by_arch():
    manifests = [
        {"platform": {"architecture": "amd64", "os": "linux"}, "digest": "sha256:aaa"},
        {"platform": {"architecture": "arm64", "os": "linux"}, "digest": "sha256:bbb"},
        {"platform": {"architecture": "amd64", "os": "windows"}, "digest": "sha256:win"},
    ]
    assert m.select_manifest(manifests, "amd64") == "sha256:aaa"
    assert m.select_manifest(manifests, "arm64") == "sha256:bbb"
    # windows 镜像被 os 过滤
    assert m.select_manifest(manifests, "windows") is None
    assert m.select_manifest(manifests, "s390x") is None


def test_select_manifest_by_annotation():
    manifests = [
        {
            "annotations": {"com.docker.official-images.bashbrew.arch": "ppc64le"},
            "platform": {"architecture": "ppc64le", "os": "linux"},
            "digest": "sha256:ppc",
        }
    ]
    assert m.select_manifest(manifests, "ppc64le") == "sha256:ppc"
