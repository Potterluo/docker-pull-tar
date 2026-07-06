"""端到端测试：以子进程运行真实 CLI，对本地 mock registry 完成拉取全流程。"""

import io
import subprocess
import sys
import tarfile
from pathlib import Path

import pytest

from conftest import (
    build_image,
    register_manifest_list,
    register_multi_arch,
    register_single_arch,
)

PROJECT_ROOT = Path(__file__).resolve().parents[1]
CLI = "docker_image_puller.py"


def _run_cli(args, tmp_path):
    """以子进程运行 CLI，返回 CompletedProcess。"""
    cmd = [sys.executable, CLI] + args
    return subprocess.run(
        cmd,
        cwd=str(PROJECT_ROOT),
        capture_output=True,
        text=True,
        timeout=60,
    )


def _find_output_tar(tmp_path):
    """在输出目录下找到生成的 docker tar。"""
    tars = list(Path(tmp_path).rglob("*.tar"))
    assert tars, f"未找到输出 tar，stderr:\n{tmp_path}"
    return tars[0]


def _assert_tar_valid(tar_path, img):
    """校验 tar 是合法的 docker image tar，且层内含预期文件。"""
    with tarfile.open(tar_path, "r") as tar:
        names = tar.getnames()
        assert "manifest.json" in names
        assert "repositories" in names

        # 找到层 layer.tar 并校验内容
        layer_members = [n for n in names if n.endswith("/layer.tar")]
        assert layer_members, f"未找到 layer.tar，成员: {names}"

        layer_member = tar.extractfile(layer_members[0])
        assert layer_member is not None
        inner = tarfile.open(fileobj=io.BytesIO(layer_member.read()), mode="r")
        try:
            member = inner.getmember(img["file_name"])
            content = inner.extractfile(member).read().decode()
            assert content == img["file_content"]
        finally:
            inner.close()


def test_e2e_single_arch(mock_registry, tmp_path):
    img = build_image()
    register_single_arch(mock_registry, img)

    result = _run_cli(
        ["-i", f"{mock_registry.host}/{img['repo']}:{img['tag']}",
         "--ci", "--insecure", "-o", str(tmp_path)],
        tmp_path,
    )
    assert result.returncode == 0, f"stderr:\n{result.stderr}"
    tar_path = _find_output_tar(tmp_path)
    _assert_tar_valid(tar_path, img)


def test_e2e_manifest_list(mock_registry, tmp_path):
    img = build_image()
    register_manifest_list(mock_registry, img)

    result = _run_cli(
        ["-i", f"{mock_registry.host}/{img['repo']}:{img['tag']}",
         "--ci", "--insecure", "-o", str(tmp_path)],
        tmp_path,
    )
    assert result.returncode == 0, f"stderr:\n{result.stderr}"
    tar_path = _find_output_tar(tmp_path)
    _assert_tar_valid(tar_path, img)


def test_e2e_list_arch(mock_registry, tmp_path):
    img = build_image(arch="arm64")
    register_manifest_list(mock_registry, img)

    result = _run_cli(
        ["-i", f"{mock_registry.host}/{img['repo']}:{img['tag']}",
         "--ci", "--insecure", "--list-arch"],
        tmp_path,
    )
    assert result.returncode == 0, f"stderr:\n{result.stderr}"
    assert "arm64" in result.stderr


def test_e2e_basic_auth_success(mock_registry, tmp_path):
    img = build_image()
    register_single_arch(mock_registry, img)
    mock_registry.set_basic_auth("user", "secret")

    result = _run_cli(
        ["-i", f"{mock_registry.host}/{img['repo']}:{img['tag']}",
         "--ci", "--insecure", "-o", str(tmp_path),
         "-u", "user", "-p", "secret"],
        tmp_path,
    )
    assert result.returncode == 0, f"stderr:\n{result.stderr}"
    assert "Basic 认证" in result.stderr
    tar_path = _find_output_tar(tmp_path)
    _assert_tar_valid(tar_path, img)


def test_e2e_basic_auth_missing_credentials(mock_registry, tmp_path):
    """CI 模式下 Basic 仓库未提供凭据应失败退出（退出码 1）。"""
    img = build_image()
    register_single_arch(mock_registry, img)
    mock_registry.set_basic_auth("user", "secret")

    result = _run_cli(
        ["-i", f"{mock_registry.host}/{img['repo']}:{img['tag']}",
         "--ci", "--insecure", "-o", str(tmp_path)],
        tmp_path,
    )
    assert result.returncode == 1, f"stderr:\n{result.stderr}"
    assert "Basic 认证" in result.stderr


def test_e2e_ci_missing_image_fails():
    """CI 模式未指定 -i 应以退出码 1 失败。"""
    result = _run_cli(["--ci"], None)
    assert result.returncode == 1
    assert "必须通过 -i" in result.stderr


def test_e2e_interactive_arch_selection(mock_registry, tmp_path):
    """交互模式下多架构镜像应提示选择架构，而非静默下载默认 amd64。"""
    amd64_img = build_image(repo="myrepo/img", tag="tag", arch="amd64",
                           file_content="amd64-content")
    arm64_img = build_image(repo="myrepo/img", tag="tag", arch="arm64",
                           file_content="arm64-content")
    register_multi_arch(mock_registry, [("amd64", amd64_img), ("arm64", arm64_img)])

    # 不传 -i / --ci：镜像名与架构均通过 stdin 交互输入
    result = subprocess.run(
        [sys.executable, CLI,
         "-r", mock_registry.host, "-u", "x", "-p", "y",
         "--insecure", "-o", str(tmp_path)],
        cwd=str(PROJECT_ROOT),
        input="myrepo/img:tag\narm64\n",
        capture_output=True, text=True, timeout=60,
    )
    assert result.returncode == 0, f"stderr:\n{result.stderr}"
    assert "请输入架构" in result.stdout

    tar_path = _find_output_tar(tmp_path)
    # 应下载 arm64（层内容为 arm64-content），而非默认 amd64
    _assert_tar_valid(tar_path, arm64_img)
