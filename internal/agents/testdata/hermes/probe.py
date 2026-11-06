"""Exercise the installed Hermes serializer, consent registry and native runner.

All settings, consent records, tool inputs and model-free human waits are local
test data. No real model or notification service is contacted.
"""

import json
import pathlib
import sys

from ruamel.yaml import YAML
from agent import shell_hooks
from hermes_cli.lifecycle import invoke_hook
from tools import approval_context
from tools.clarify_tool import clarify_tool
from tools.human_input_hooks import human_input_request

settings, capture = map(pathlib.Path, sys.argv[1:3])
cfg = YAML(typ="safe").load(settings.read_text(encoding="utf-8"))
payloads = []
original_serializer = shell_hooks._serialize_payload
request_ids = {}


def record(event, kwargs):
    wire = original_serializer(event, kwargs)
    payload = json.loads(wire)
    payload["cwd"] = "C:/blip-hermes-test/中文 $ & ' 路径"
    request_id = payload.get("extra", {}).get("request_id")
    if request_id:
        request_ids.setdefault(request_id, f"request-{len(request_ids) + 1}")
        payload["extra"]["request_id"] = request_ids[request_id]
    payloads.append(payload)
    return wire


shell_hooks._serialize_payload = record
specs = shell_hooks.register_from_config(cfg, accept_hooks=True, interactive=False)
assert len(specs) == 5, [spec.event for spec in specs]
approval_context._approval_session_id.set("test-session")
approval_context._approval_turn_id.set("turn-1")
approval_context._approval_tool_call_id.set("tool-1")

invoke_hook("pre_tool_call", session_id="test-session", turn_id="turn-1",
            tool_call_id="tool-1", tool_name="clarify",
            args={"questions": [{"question": "选择？", "choices": ["继续", "结束"]}]})
result = clarify_tool(
    [{"question": "选择？", "choices": ["继续", "结束"]}],
    callback=lambda _: {"answers": {"q0": "继续"}, "outcome": "answered"},
)
assert json.loads(result)["outcome"] == "answered", result
invoke_hook("on_session_end", session_id="test-session", turn_id="turn-1",
            completed=True, failed=False, interrupted=False, turn_exit_reason="completed")

invoke_hook("pre_tool_call", session_id="test-session", turn_id="turn-2",
            tool_call_id="tool-2", tool_name="terminal", args={"command": "echo local-test"})
with human_input_request("approval", prompt="echo local-test") as wait:
    wait.outcome = "approved"
invoke_hook("on_session_end", session_id="test-session", turn_id="turn-2",
            completed=True, failed=False, interrupted=False, turn_exit_reason="completed")
invoke_hook("on_session_end", session_id="test-session", turn_id="turn-3",
            completed=False, failed=True, interrupted=False, turn_exit_reason="api_error")
invoke_hook("on_session_end", session_id="test-session", turn_id="turn-4",
            completed=False, failed=False, interrupted=True, turn_exit_reason="interrupted")
invoke_hook("subagent_start", parent_session_id="test-session",
            child_session_id="child-session", child_subagent_id="child-1")
invoke_hook("on_session_end", session_id="child-session", turn_id="child-turn",
            completed=True, failed=False, interrupted=False, turn_exit_reason="completed")
invoke_hook("on_session_end", session_id="test-session", platform="cli")

capture.write_text("".join(json.dumps(p, ensure_ascii=False) + "\n" for p in payloads), encoding="utf-8")
print(json.dumps({"callbacks": len(payloads), "python": sys.version.split()[0]}))
