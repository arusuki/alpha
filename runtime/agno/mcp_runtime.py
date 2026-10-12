"""Remote MCP tools for one isolated run; only explicit names are exposed."""
import asyncio
from contextlib import AsyncExitStack
import json

MCP_VERSION = "2.1.0"


class MCPError(Exception):
    pass


async def run(data, execute, authorize):
    import httpx2
    from agno.tools.function import Function
    from mcp import ClientSession
    from mcp.client.streamable_http import streamable_http_client
    from mcp.types import PaginatedRequestParams

    loop = asyncio.get_running_loop()
    functions = []
    failed = []
    async with AsyncExitStack() as stack:
        for server in data["settings"]["mcp_servers"]:
            if not server["enabled"]:
                continue
            try:
                token = data["mcp_tokens"].get(server["id"], "")
                client = await stack.enter_async_context(httpx2.AsyncClient(
                    headers={"Authorization": "Bearer " + token} if token else {},
                    timeout=httpx2.Timeout(15, read=60), follow_redirects=False, trust_env=False))
                streams = await stack.enter_async_context(streamable_http_client(server["url"], http_client=client))
                session = await stack.enter_async_context(ClientSession(streams[0], streams[1], read_timeout_seconds=60))
                await session.initialize()
                discovered = {}
                cursor = None
                for _ in range(20):
                    page = await session.list_tools(params=PaginatedRequestParams(cursor=cursor) if cursor else None)
                    for tool in page.tools:
                        if tool.name in discovered:
                            raise MCPError()
                        discovered[tool.name] = tool
                    cursor = page.next_cursor
                    if not cursor:
                        break
                else:
                    raise MCPError()
                for name in server["tools"]:
                    tool = discovered[name]  # Missing tools fail closed, never enable all.
                    def bind(session, server_id, name):
                        def invoke(**kwargs):
                            try:
                                approval = json.loads(authorize("mcp:" + server_id + ":" + name))
                                if not approval.get("value", {}).get("allowed"):
                                    raise MCPError()
                                future = asyncio.run_coroutine_threadsafe(session.call_tool(name, kwargs), loop)
                                try:
                                    result = future.result(timeout=65)
                                except Exception:
                                    future.cancel()
                                    raise
                                if result.is_error:
                                    raise MCPError()
                                encoded = result.model_dump_json(by_alias=True, exclude_none=True)
                                if len(encoded.encode()) > 2 << 20:
                                    raise MCPError()
                                return encoded
                            except Exception:
                                failed.append(True)
                                raise MCPError("MCP 工具调用失败，远端操作可能已执行，请核实") from None
                        return invoke
                    functions.append(Function(name="mcp_" + server["id"] + "_" + name,
                        description=tool.description or name, parameters=tool.input_schema,
                        entrypoint=bind(session, server["id"], name), skip_entrypoint_processing=True))
            except Exception:
                raise MCPError("MCP 连接或工具发现失败，请检查服务地址、认证和工具名称") from None
        result = await asyncio.to_thread(execute, data, functions)
        if failed:
            raise MCPError()
        return result
