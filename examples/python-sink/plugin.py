#!/usr/bin/env python3
"""acplug/1 example: an event sink served over the HTTP/JSON mapping, standard library only.

Run: python3 plugin.py [--port 8099] [--token-env SINK_TOKEN] [--out events.jsonl]
Prints "LISTENING <port>" once it accepts connections. Events are kept once per id
(at-least-once delivery is deduplicated) and, with --out, appended to a JSON Lines file.
"""
import argparse
import hmac
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PROTOCOL = "acplug/1"


class Rpc(Exception):
    def __init__(self, http, code, message):
        super().__init__(message)
        self.http, self.code, self.message = http, code, message


class Sink:
    def __init__(self, out):
        self.lock = threading.Lock()
        self.seen = set()
        self.out = out

    def manifest(self, req):
        if PROTOCOL not in req.get("host_protocols", []):
            raise Rpc(400, "FAILED_PRECONDITION", "host does not speak " + PROTOCOL)
        return {
            "protocol": PROTOCOL,
            "protocol_version": {"min": 1, "max": 1},
            "name": "python-sink",
            "version": "1.0.0",
            "ports": [{
                "port": "sink",
                "config_schema": {"type": "object", "additionalProperties": False},
                "capabilities": ["batch"],
                "limits": {"max_in_flight": 16, "deadline_ms": 5000},
            }],
        }

    def health(self, _):
        return {"status": "STATUS_SERVING"}

    def publish(self, req):
        events = req.get("events", [])
        if any(not e.get("id") for e in events):
            raise Rpc(400, "INVALID_ARGUMENT", "every event needs an id")
        with self.lock:
            fresh = [e for e in events if e["id"] not in self.seen]
            if self.out and fresh:
                with open(self.out, "a", encoding="utf-8") as f:
                    for e in fresh:
                        f.write(json.dumps(e, sort_keys=True) + "\n")
            self.seen.update(e["id"] for e in fresh)
        return {"accepted": len(events)}


def make_handler(sink, token):
    methods = {
        "acplug.v1.Plugin/Manifest": sink.manifest,
        "acplug.v1.Plugin/Health": sink.health,
        "acplug.v1.EventSink/Publish": sink.publish,
    }

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        wbufsize = -1

        def log_message(self, *_):
            pass

        def reply(self, http, body):
            raw = json.dumps(body).encode()
            self.send_response(http)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            body = self.rfile.read(length)
            try:
                if token and not hmac.compare_digest(self.headers.get("Authorization", "").encode(), ("Bearer " + token).encode()):
                    raise Rpc(401, "UNAUTHENTICATED", "bad bearer token")
                handler = methods.get(self.path.lstrip("/"))
                if handler is None:
                    raise Rpc(404, "UNIMPLEMENTED", "no such method")
                try:
                    req = json.loads(body or b"{}")
                except ValueError:
                    raise Rpc(400, "INVALID_ARGUMENT", "body is not JSON")
                self.reply(200, handler(req))
            except Rpc as e:
                self.reply(e.http, {"error": {"code": e.code, "message": e.message}})

    return Handler


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=0)
    ap.add_argument("--token-env", default="", help="environment variable holding the bearer token")
    ap.add_argument("--out", default="", help="append delivered events to this JSON Lines file")
    args = ap.parse_args()
    token = os.environ.get(args.token_env, "") if args.token_env else ""
    server = ThreadingHTTPServer(("127.0.0.1", args.port), make_handler(Sink(args.out), token))
    server.daemon_threads = True
    print("LISTENING %d" % server.server_address[1], flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        sys.exit(0)


if __name__ == "__main__":
    main()
