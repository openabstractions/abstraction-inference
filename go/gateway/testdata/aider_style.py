"""An aider-style client of the gateway window.

It sends what aider sends through litellm's OpenAI-compatible provider with
OPENAI_API_BASE and OPENAI_API_KEY set: a model listing, then a non-streaming
chat completion with a system prompt, temperature 0 and a function tool, then
the tool's result in a second completion.

    aider_style.py <base URL ending in /v1> <key> <model>

Prints one JSON line: the models listed, the tool call, the final text and its
finish_reason. Exits 3 with the status and body on any answer but 200.
"""
import http.client
import json
import sys
import urllib.parse

if len(sys.argv) != 4:
    print(__doc__)
    raise SystemExit(2)
base, key, model = sys.argv[1:]
url = urllib.parse.urlsplit(base)
prefix = url.path.rstrip("/")
headers = {"Authorization": "Bearer " + key, "Content-Type": "application/json", "User-Agent": "litellm aider-style"}


def call(method, path, body=None):
    conn = http.client.HTTPConnection(url.hostname, url.port, timeout=30)
    conn.request(method, prefix + path, None if body is None else json.dumps(body), headers)
    resp = conn.getresponse()
    data = resp.read().decode("utf-8", "replace")
    if resp.status != 200:
        print(json.dumps({"status": resp.status, "body": data}))
        raise SystemExit(3)
    return json.loads(data)


listed = [m["id"] for m in call("GET", "/models")["data"]]
tools = [{"type": "function", "function": {"name": "replace_lines", "description": "Replace lines in a file",
                                            "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}}]
messages = [{"role": "system", "content": "Act as an expert software developer."},
            {"role": "user", "content": [{"type": "text", "text": "Use the tool on a.py"}]}]
first = call("POST", "/chat/completions", {"model": model, "messages": messages, "temperature": 0, "tools": tools, "stream": False})
choice = first["choices"][0]
calls = choice["message"].get("tool_calls") or []
messages.append({"role": "assistant", "content": None, "tool_calls": calls})
for c in calls:
    messages.append({"role": "tool", "tool_call_id": c["id"], "content": "replaced"})
second = call("POST", "/chat/completions", {"model": model, "messages": messages, "temperature": 0, "tools": tools, "stream": False})
print(json.dumps({"models": listed, "first_finish": choice["finish_reason"],
                  "tool_calls": [{"id": c["id"], "name": c["function"]["name"], "arguments": json.loads(c["function"]["arguments"])} for c in calls],
                  "text": second["choices"][0]["message"]["content"], "finish_reason": second["choices"][0]["finish_reason"],
                  "usage": second["usage"]}))
