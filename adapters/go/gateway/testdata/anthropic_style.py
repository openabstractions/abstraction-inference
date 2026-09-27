"""An Anthropic-messages client of the gateway window.

It sends what a program on the Anthropic wire sends with ANTHROPIC_BASE_URL
set: POST <base>/v1/messages with x-api-key and anthropic-version, max_tokens, a
system prompt and stream true, and reads the named events to message_stop.

    anthropic_style.py <base URL> <key> <model>

Prints one JSON line: text, stop_reason, output tokens and the event names in
order. Exits 3 with the status and body on any answer but 200.
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
conn = http.client.HTTPConnection(url.hostname, url.port, timeout=30)
body = {"model": model, "max_tokens": 256, "system": "Be brief.", "stream": True,
        "messages": [{"role": "user", "content": [{"type": "text", "text": "Say hello"}]}]}
conn.request("POST", url.path.rstrip("/") + "/v1/messages", json.dumps(body),
             {"x-api-key": key, "anthropic-version": "2023-06-01", "content-type": "application/json"})
resp = conn.getresponse()
if resp.status != 200:
    print(json.dumps({"status": resp.status, "body": resp.read().decode("utf-8", "replace")}))
    raise SystemExit(3)
events, text, stop, output = [], "", None, None
event = None
for raw in resp:
    line = raw.decode("utf-8").strip()
    if line.startswith("event: "):
        event = line[len("event: "):]
        continue
    if not line.startswith("data: "):
        continue
    data = json.loads(line[len("data: "):])
    events.append(event)
    if event == "error":
        print(json.dumps({"error": data["error"]}))
        raise SystemExit(4)
    if event == "content_block_delta" and data["delta"]["type"] == "text_delta":
        text += data["delta"]["text"]
    if event == "message_delta":
        stop, output = data["delta"]["stop_reason"], data["usage"]["output_tokens"]
    if event == "message_stop":
        break
print(json.dumps({"text": text, "stop_reason": stop, "output_tokens": output, "events": events}))
