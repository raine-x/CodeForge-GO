"""pytest 公共 fixture：LLM 端点配置与 HTTP 会话。

配置来源（优先级从高到低）：
  1. 进程环境变量
  2. 项目根目录 / tests 目录下的 .env 文件（已被 .gitignore 忽略）

支持的变量：
    LLM_BASE_URL  默认 https://api.tokenrouter.com/v1
    LLM_MODEL     默认 z-ai/glm-5.3-free
    LLM_API_KEY   必填；未设置时跳过全部在线用例
    LLM_TIMEOUT   单请求超时秒数，默认 120

生成 .env：
    cp .env.example .env   # 然后填写 LLM_API_KEY
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest
import requests

DEFAULT_BASE_URL = "https://api.tokenrouter.com/v1"
DEFAULT_MODEL = "z-ai/glm-5.3-free"


def _load_dotenv() -> None:
    """极简 .env 加载器（零依赖）：不覆盖已存在的环境变量。"""
    here = Path(__file__).resolve()
    for candidate in (here.parent.parent / ".env", here.parent / ".env"):
        if not candidate.is_file():
            continue
        for raw in candidate.read_text(encoding="utf-8").splitlines():
            line = raw.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            key = key.strip()
            value = value.strip().strip('"').strip("'")
            if key and key not in os.environ:
                os.environ[key] = value
        break


_load_dotenv()

# 免费模型首包可能较慢，统一给足超时。
REQUEST_TIMEOUT = int(os.getenv("LLM_TIMEOUT", "120"))


@pytest.fixture(scope="session")
def base_url() -> str:
    return os.getenv("LLM_BASE_URL", DEFAULT_BASE_URL).rstrip("/")


@pytest.fixture(scope="session")
def model() -> str:
    return os.getenv("LLM_MODEL", DEFAULT_MODEL)


@pytest.fixture(scope="session")
def api_key() -> str:
    key = os.getenv("LLM_API_KEY", "").strip()
    if not key:
        pytest.skip("未设置 LLM_API_KEY（环境变量或 .env），跳过在线 LLM 测试")
    return key


@pytest.fixture(scope="session")
def session(api_key: str, base_url: str) -> requests.Session:
    s = requests.Session()
    s.headers.update(
        {
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            "Accept": "application/json",
        }
    )
    return s


@pytest.fixture(scope="session")
def timeout() -> int:
    return REQUEST_TIMEOUT
