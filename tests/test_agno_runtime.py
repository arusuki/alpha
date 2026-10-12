"""Exercise the real pinned Agno/OpenAI libraries using an in-process HTTP stub.
Run with: <agno-venv>/bin/python -m unittest discover -s tests -p test_agno_runtime.py
"""
import importlib.util
import json
from pathlib import Path
import unittest
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest.mock import patch

import httpx

spec = importlib.util.spec_from_file_location("alpha_agno", Path(__file__).resolve().parents[1] / "runtime/agno/runtime.py")
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class RuntimeTests(unittest.TestCase):
    def data(self, protocol="completions"):
        suffix = "/chat/completions" if protocol == "completions" else "/responses"
        return {"settings": {"instructions": "按证据巡检，优先检查 GPU", "builtin_tools": ["list_nodes"], "mcp_servers": []}, "mcp_tokens": {}, "config": {"protocol": protocol, "endpoint": "https://model.example/v1" + suffix,
                "model": "test", "timeout_seconds": 30, "reasoning_summary": False},
                "api_key": "private-test-token", "user_id": "user-a", "session_id": "session-a",
                "message": "记住我的巡检偏好：先检查 GPU，并列出节点。", "session": None, "memories": []}

    def execute(self, data, respond):
        actual_client = httpx.Client
        class Client(actual_client):
            def __init__(self, **kwargs):
                super().__init__(transport=httpx.MockTransport(respond), **kwargs)
        with patch.object(runtime, "query", return_value='{"value":{"nodes":[]}}') as tool:
            with patch.object(httpx, "Client", Client):
                result = runtime.execute(data)
        return result, tool

    def completion(self, content=None, name=None, args=None):
        message = {"role": "assistant", "content": content}
        if name:
            message["tool_calls"] = [{"id": "call_" + name, "type": "function", "function": {
                "name": name, "arguments": json.dumps(args or {})}}]
        return httpx.Response(200, json={"id": "chat-1", "object": "chat.completion", "created": 1,
            "model": "test", "choices": [{"index": 0, "message": message,
                "finish_reason": "tool_calls" if name else "stop"}]})

    def test_tools_memory_and_restore(self):
        def respond(req):
            body = json.loads(req.content)
            self.assertEqual(req.url.path, "/v1/chat/completions")
            self.assertEqual(req.headers["authorization"], "Bearer private-test-token")
            names = {t["function"]["name"] for t in body.get("tools", [])}
            used = {m.get("tool_call_id", "").removeprefix("call_") for m in body["messages"] if m["role"] == "tool"}
            if "add_memory" in names:
                if "add_memory" not in used:
                    return self.completion(name="add_memory", args={"memory": "巡检先检查 GPU", "topics": ["operations"]})
                return self.completion("记忆已保存")
            if "list_nodes" not in used:
                return self.completion(name="list_nodes")
            if "update_user_memory" not in used:
                return self.completion(name="update_user_memory", args={"task": "记住巡检先检查 GPU"})
            return self.completion("当前未注册节点；已记住巡检先检查 GPU。")
        first, tool = self.execute(self.data(), respond)
        tool.assert_called_with("list_nodes")
        self.assertEqual(first["memories"][0]["memory"], "巡检先检查 GPU")
        self.assertEqual(first["memories"][0]["user_id"], "user-a")
        self.assertNotIn("private-test-token", json.dumps(first))
        second_data = self.data()
        second_data.update(session=first["session"], memories=first["memories"], message="我的偏好是什么？")
        requests = []
        def recall(req):
            requests.append(json.loads(req.content))
            return self.completion("你偏好先检查 GPU。")
        second, _ = self.execute(second_data, recall)
        self.assertIn("巡检先检查 GPU", json.dumps(requests, ensure_ascii=False))
        self.assertIn("当前未注册节点", json.dumps(requests, ensure_ascii=False))
        self.assertEqual(len(second["session"]["state"]["runs"]), 2)
        second_data.update(session=None, session_id="session-b")
        requests.clear()
        self.execute(second_data, recall)
        self.assertIn("巡检先检查 GPU", json.dumps(requests, ensure_ascii=False))
        self.assertNotIn("当前未注册节点", json.dumps(requests, ensure_ascii=False))

    def test_responses_configuration_and_no_implicit_key(self):
        data = self.data("responses")
        data["api_key"] = ""
        data["config"]["reasoning_summary"] = True
        def respond(req):
            self.assertNotIn("authorization", req.headers)
            self.assertEqual(req.url.path, "/v1/responses")
            body = json.loads(req.content)
            self.assertFalse(body["store"])
            self.assertNotIn("previous_response_id", body)
            self.assertEqual(body["reasoning"]["summary"], "auto")
            return httpx.Response(200, json={"id": "resp_1", "object": "response", "created_at": 1,
                "model": "test", "status": "completed", "output": [{"id": "msg_1", "type": "message",
                "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "正常回复", "annotations": []}]}]})
        result, _ = self.execute(data, respond)
        self.assertEqual(result["content"], "正常回复")

    def test_invalid_state_and_owner_rejected(self):
        data = self.data()
        for state in [{"format": "old"}, {"format": runtime.FORMAT, "agno_version": runtime.AGNO_VERSION,
                      "user_id": "other", "session_id": "session-a", "state": {}}]:
            data["session"] = state
            with self.assertRaises(runtime.StateError):
                runtime.restore(data)
        data["session"] = None
        data["memories"] = [{"user_id": "other", "memory": "private"}]
        with self.assertRaises(runtime.StateError):
            runtime.restore(data)

    def test_incomplete_provider_output_rejected(self):
        def respond(req):
            response = self.completion("truncated")
            body = json.loads(response.content)
            body["choices"][0]["finish_reason"] = "length"
            return httpx.Response(200, json=body)
        with self.assertRaises(Exception):
            self.execute(self.data(), respond)

    def test_memory_model_failure_aborts_turn_and_keeps_transport(self):
        data = self.data()
        data["api_key"] = ""
        memory_requests = []
        def respond(req):
            self.assertNotIn("authorization", req.headers)
            body = json.loads(req.content)
            names = {t["function"]["name"] for t in body.get("tools", [])}
            if "add_memory" in names:
                memory_requests.append(req)
                return httpx.Response(503, json={"error": {"message": "memory unavailable", "type": "server_error"}})
            if not any(m["role"] == "tool" for m in body["messages"]):
                return self.completion(name="update_user_memory", args={"task": "记住先看 GPU"})
            return self.completion("partial answer after memory failure")
        with self.assertRaises(ValueError):
            self.execute(data, respond)
        self.assertEqual(len(memory_requests), 1)

    def test_provider_failure_is_not_retried(self):
        requests = []
        def respond(req):
            requests.append(req)
            return httpx.Response(429, json={"error": {"message": "rate limit", "type": "rate_limit_error"}})
        with self.assertRaises(Exception):
            self.execute(self.data(), respond)
        self.assertEqual(len(requests), 1)

    def test_remote_mcp_allowlist_prompt_auth_and_restore(self):
        calls = []
        class MCP(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def do_GET(self):
                self.send_error(405)
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
                if self.headers.get('Authorization') != 'Bearer mcp-private-token':
                    self.send_error(401)
                    return
                method = body['method']
                if method == 'notifications/initialized':
                    self.send_response(202); self.end_headers(); return
                if method == 'initialize':
                    value = dict(protocolVersion=body['params']['protocolVersion'], capabilities=dict(tools={}), serverInfo=dict(name='fixture',version='1'))
                elif method == 'tools/list':
                    value = dict(tools=[dict(name=name,description=name,inputSchema=dict(type='object',properties=dict(node=dict(type='string')),required=['node'])) for name in ['status','delete']])
                elif method == 'tools/call':
                    calls.append(body['params'])
                    value = dict(content=[dict(type='text',text='GPU healthy')],isError=False)
                else:
                    self.send_error(400); return
                raw = json.dumps(dict(jsonrpc='2.0',id=body['id'],result=value)).encode()
                self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
        server = ThreadingHTTPServer(('127.0.0.1',0),MCP)
        threading.Thread(target=server.serve_forever,daemon=True).start()
        data = self.data()
        data['settings'] = dict(instructions='只汇报 GPU 指标',builtin_tools=[],mcp_servers=[dict(id='metrics',url=f'http://127.0.0.1:{server.server_port}/mcp',enabled=True,tools=['status'])])
        data['mcp_tokens'] = dict(metrics='mcp-private-token')
        def respond(req):
            body=json.loads(req.content)
            names={t['function']['name'] for t in body.get('tools',[])}
            self.assertEqual(names,{'mcp_metrics_status','update_user_memory'})
            self.assertIn('只汇报 GPU 指标',json.dumps(body,ensure_ascii=False))
            if not any(m['role']=='tool' for m in body['messages']):
                return self.completion(name='mcp_metrics_status',args=dict(node='worker-a'))
            self.assertIn('GPU healthy',json.dumps(body))
            return self.completion('GPU 正常')
        try:
            actual=httpx.Client
            class Client(actual):
                def __init__(self,**kwargs):
                    super().__init__(transport=httpx.MockTransport(respond),**kwargs)
            with patch.object(runtime,'query',return_value='{"value":{"allowed":true}}') as auth, patch.object(httpx,'Client',Client):
                result=runtime.execute(data)
                auth.assert_called_once_with('mcp:metrics:status')
            self.assertEqual([(c['name'],c['arguments']) for c in calls],[('status',dict(node='worker-a'))])
            self.assertNotIn('mcp-private-token',json.dumps(result))
            data.update(session=result['session'],memories=result['memories'],message='下一轮')
            data['settings']['instructions']='只汇报 GPU 指标，简洁回答'
            with patch.object(runtime,'query',return_value='{"value":{"allowed":true}}'),patch.object(httpx,'Client',Client):
                result=runtime.execute(data)
            self.assertEqual(len(result['session']['state']['runs']),2)
            data['settings']['mcp_servers'][0]['tools']=['missing']
            with self.assertRaises(Exception):
                runtime.execute(data)
            data['settings']['mcp_servers'][0]['enabled']=False
            data['settings']['mcp_servers'][0]['url']='http://127.0.0.1:1/mcp'
            result,_=self.execute(data,lambda req:self.completion('MCP 已禁用'))
            self.assertEqual(result['content'],'MCP 已禁用')
        finally:
            server.shutdown();server.server_close()


if __name__ == "__main__":
    unittest.main()
