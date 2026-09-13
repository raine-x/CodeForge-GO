"""OpenAI 兼容协议的测试辅助：SSE 解析、载荷构造、带重试的请求。

共享/免费网关会偶发 408 / 429 / 5xx（例如 "gateway overloaded"），
这类上游瞬时故障不应判定为被测客户端的问题，因此统一做指数退避重试，
重试耗尽后才把结果交回用例断言。
"""

from __future__ import annotations

import json
import time
from typing import Any, Iterator

import requests

TRANSIENT_STATUS = {408, 425, 429, 500, 502, 503, 504}
DEFAULT_RETRIES = 4
DEFAULT_BACKOFF = 1.5


def _sleep(attempt: int, backoff: float) -> None:
    time.sleep(backoff * (2**attempt))


def chat_payload(model: str, messages: list[dict], **extra: Any) -> dict[str, Any]:
    """构造 /chat/completions 请求体。"""
    payload: dict[str, Any] = {
        "model": model,
        "messages": messages,
        "max_tokens": 256,
        "temperature": 0.2,
    }
    payload.update(extra)
    return payload


def sse_events(resp: requests.Response) -> Iterator[dict[str, Any]]:
    """解析 SSE 流，逐条 yield 解析后的 JSON（跳过注释行与空行）。"""
    for raw in resp.iter_lines(decode_unicode=True):
        if not raw:
            continue
        line = raw.strip()
        if line.startswith(":"):
            continue
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if payload == "[DONE]":
            yield {"__done__": True}
            continue
        try:
            yield json.loads(payload)
        except json.JSONDecodeError:
            continue


def post_json(
    session: requests.Session,
    url: str,
    payload: dict[str, Any],
    timeout: int,
    retries: int = DEFAULT_RETRIES,
    backoff: float = DEFAULT_BACKOFF,
) -> requests.Response:
    """非流式 POST，遇瞬时状态码自动重试。"""
    resp: requests.Response | None = None
    for attempt in range(retries + 1):
        resp = session.post(url, json=payload, timeout=timeout)
        if resp.status_code not in TRANSIENT_STATUS:
            return resp
        if attempt < retries:
            _sleep(attempt, backoff)
    assert resp is not None
    return resp


def collect_stream(
    session: requests.Session,
    url: str,
    payload: dict[str, Any],
    timeout: int,
    retries: int = DEFAULT_RETRIES,
    backoff: float = DEFAULT_BACKOFF,
) -> tuple[int, str, list[dict[str, Any]]]:
    """流式 POST，返回 (status_code, error_text, events)。瞬时故障自动重试。"""
    last_status, last_text = 0, ""
    for attempt in range(retries + 1):
        resp = session.post(url, json=payload, stream=True, timeout=timeout)
        if resp.status_code in TRANSIENT_STATUS:
            last_status = resp.status_code
            last_text = resp.text[:300]
            resp.close()
            if attempt < retries:
                _sleep(attempt, backoff)
                continue
            return last_status, last_text, []

        events = list(sse_events(resp))
        status = resp.status_code
        resp.close()
        return status, "", events
    return last_status, last_text, []
