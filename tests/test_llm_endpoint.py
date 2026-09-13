"""LLM 端点测试（pytest）。

覆盖 CodeForge 实际依赖的 OpenAI 兼容协议面：
  1. GET  /models            模型列表可用
  2. POST /chat/completions  非流式对话（含 usage）
  3. POST /chat/completions  流式对话（SSE 增量拼装 + [DONE]）
  4. POST /chat/completions  Tool Call（函数调用）
  5. POST /chat/completions  CodeForge 真实载荷形态（system + tools + stream + 工具结果回填）
  6. 错误密钥被正确拒绝

运行方式：
    cp .env.example .env      # 填写 LLM_API_KEY
    pytest tests/ -v -s

也可直接用环境变量覆盖：
    LLM_BASE_URL=... LLM_MODEL=... LLM_API_KEY=... pytest tests/ -v -s
"""

from __future__ import annotations

import json
from typing import Any

import pytest
import requests

from llm_client import chat_payload, collect_stream, post_json

WEATHER_TOOL: dict[str, Any] = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "查询指定城市的当前天气",
        "parameters": {
            "type": "object",
            "properties": {
                "city": {"type": "string", "description": "城市名称，例如 北京"},
            },
            "required": ["city"],
        },
    },
}


# --------------------------------------------------------------------------- 用例


def test_models_endpoint(session: requests.Session, base_url: str, timeout: int) -> None:
    """GET /models 应返回可用的模型列表。"""
    resp = session.get(f"{base_url}/models", timeout=timeout)
    assert resp.status_code == 200, f"模型列表请求失败: {resp.status_code} {resp.text[:300]}"

    body = resp.json()
    assert isinstance(body, dict), f"响应不是 JSON 对象: {body!r}"
    models = body.get("data")
    assert isinstance(models, list) and models, f"模型列表为空: {body!r}"

    ids = [m.get("id") for m in models if isinstance(m, dict)]
    print(f"\n可用模型 {len(ids)} 个，示例: {ids[:8]}")


def test_chat_completion_non_stream(
    session: requests.Session, base_url: str, model: str, timeout: int
) -> None:
    """非流式对话应返回非空文本与 usage。"""
    resp = post_json(
        session,
        f"{base_url}/chat/completions",
        chat_payload(model, [{"role": "user", "content": "用一句话介绍你自己。"}]),
        timeout,
    )
    assert resp.status_code == 200, f"对话请求失败: {resp.status_code} {resp.text[:300]}"

    body = resp.json()
    assert body.get("choices"), f"缺少 choices: {body!r}"

    message = body["choices"][0].get("message", {})
    content = (message.get("content") or "").strip()
    assert content, f"回复内容为空: {body!r}"

    usage = body.get("usage") or {}
    print(f"\n模型: {body.get('model')}")
    print(f"回复: {content[:120]}")
    print(f"用量: {usage}")
    assert usage.get("total_tokens", 0) > 0, f"usage 缺失或为 0: {usage!r}"


def test_chat_completion_stream(
    session: requests.Session, base_url: str, model: str, timeout: int
) -> None:
    """流式对话应逐块返回增量，并以 [DONE] 结束。"""
    payload = chat_payload(
        model,
        [{"role": "user", "content": "从 1 数到 5，只输出数字，用逗号分隔。"}],
        stream=True,
    )
    status, err, events = collect_stream(session, f"{base_url}/chat/completions", payload, timeout)
    assert status == 200, f"流式请求失败: {status} {err}"

    chunks: list[str] = []
    saw_done = False
    for event in events:
        if event.get("__done__"):
            saw_done = True
            break
        for choice in event.get("choices") or []:
            piece = (choice.get("delta") or {}).get("content")
            if piece:
                chunks.append(piece)

    text = "".join(chunks).strip()
    print(f"\n流式分片数: {len(chunks)}")
    print(f"拼接结果: {text[:120]}")
    assert chunks, "流式响应未产生任何增量分片"
    assert text, "流式拼接结果为空"
    assert saw_done, "流式响应未以 [DONE] 正常结束"


def test_tool_calling(
    session: requests.Session, base_url: str, model: str, timeout: int
) -> None:
    """带 tools 的请求应触发 Tool Call，且 arguments 为合法 JSON。"""
    payload = chat_payload(
        model,
        [{"role": "user", "content": "北京现在天气怎么样？请调用工具查询。"}],
        tools=[WEATHER_TOOL],
        tool_choice="auto",
    )
    resp = post_json(session, f"{base_url}/chat/completions", payload, timeout)
    assert resp.status_code == 200, f"工具调用请求失败: {resp.status_code} {resp.text[:300]}"

    body = resp.json()
    choice = body["choices"][0]
    message = choice.get("message", {})
    tool_calls = message.get("tool_calls") or []

    if not tool_calls:
        pytest.fail(
            "模型未返回 tool_calls（该模型可能不支持函数调用）。"
            f"finish_reason={choice.get('finish_reason')!r}, "
            f"content={(message.get('content') or '')[:120]!r}"
        )

    call = tool_calls[0]
    fn = call.get("function") or {}
    print(f"\n工具名: {fn.get('name')}")
    print(f"参数: {fn.get('arguments')}")

    assert call.get("id"), "tool_call 缺少 id"
    assert fn.get("name") == "get_weather", f"工具名不符: {fn.get('name')!r}"
    args = json.loads(fn.get("arguments") or "{}")
    assert "city" in args, f"参数缺少 city: {args!r}"


def test_codeforge_payload_shape(
    session: requests.Session, base_url: str, model: str, timeout: int
) -> None:
    """复现 CodeForge 适配层载荷（tools + stream），并校验多轮工具结果回填。"""
    payload = chat_payload(
        model,
        [{"role": "user", "content": "北京现在天气怎么样？请调用工具查询。"}],
        tools=[WEATHER_TOOL],
        tool_choice="auto",
        stream=True,
    )
    status, err, events = collect_stream(session, f"{base_url}/chat/completions", payload, timeout)
    assert status == 200, f"流式工具请求失败: {status} {err}"

    tool_id, tool_name, args_buf = "", "", []
    for event in events:
        if event.get("__done__"):
            break
        for choice in event.get("choices") or []:
            for tc in (choice.get("delta") or {}).get("tool_calls") or []:
                if tc.get("id"):
                    tool_id = tc["id"]
                fn = tc.get("function") or {}
                if fn.get("name"):
                    tool_name = fn["name"]
                if fn.get("arguments"):
                    args_buf.append(fn["arguments"])

    assert tool_id and tool_name, (
        f"流式分片未拼装出完整 tool_call: id={tool_id!r} name={tool_name!r}"
    )
    arguments = "".join(args_buf) or "{}"
    print(f"\n拼装结果: id={tool_id} name={tool_name} args={arguments}")

    # 第二轮：回填工具结果，模型应给出自然语言总结
    followup = chat_payload(
        model,
        [
            {"role": "user", "content": "北京现在天气怎么样？请调用工具查询。"},
            {
                "role": "assistant",
                "content": "",
                "tool_calls": [
                    {
                        "id": tool_id,
                        "type": "function",
                        "function": {"name": tool_name, "arguments": arguments},
                    }
                ],
            },
            {"role": "tool", "tool_call_id": tool_id, "content": "晴，26 摄氏度"},
        ],
    )
    resp2 = post_json(session, f"{base_url}/chat/completions", followup, timeout)
    assert resp2.status_code == 200, f"工具结果回填失败: {resp2.status_code} {resp2.text[:300]}"

    final = (resp2.json()["choices"][0].get("message", {}).get("content") or "").strip()
    print(f"最终回复: {final[:150]}")
    assert final, "回填工具结果后模型未给出文本回复"


def test_invalid_api_key_rejected(base_url: str, model: str, timeout: int) -> None:
    """错误密钥必须被拒绝（401/403）。"""
    resp = requests.post(
        f"{base_url}/chat/completions",
        headers={
            "Authorization": "Bearer sk-invalid-key-for-testing",
            "Content-Type": "application/json",
        },
        json=chat_payload(model, [{"role": "user", "content": "hi"}]),
        timeout=timeout,
    )
    print(f"\n错误密钥返回: {resp.status_code} {resp.text[:200]}")
    assert resp.status_code in (401, 403), f"错误密钥未被拒绝: {resp.status_code}"
