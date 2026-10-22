"""HTTP endpoint of the name model: text in, person spans out (seam S2).

POST /v1/spans takes {"model", "label", "threshold", "texts"} and answers
{"model", "label", "threshold", "spans"}, where spans[i] lists the half-open
UTF-8 byte ranges of texts[i] the model labels `label` at a score of
`threshold` or more. A request the server cannot answer as asked gets a 422
and no spans. The server reads and writes no database and no file, and its
log lines carry no text.
"""

import http.server
import json
import math
import re
import socketserver
import sys

MAX_BODY_BYTES = 32 * 1024 * 1024

# GLiNER's own word splitter: its input window is counted in these words.
WORD = re.compile(r"\w+(?:[-_]\w+)*|\S")


class RequestError(Exception):
    """A request the server cannot answer as asked: a 422."""


def parse_request(body, model_name):
    try:
        req = json.loads(body)
    except (ValueError, UnicodeDecodeError):
        raise RequestError("body is not JSON")
    if not isinstance(req, dict):
        raise RequestError("body is not a JSON object")
    if req.get("model") != model_name:
        raise RequestError(f"model must be {model_name}")
    label = req.get("label")
    if not isinstance(label, str) or not label:
        raise RequestError("label must be a non-empty string")
    threshold = req.get("threshold")
    if (
        isinstance(threshold, bool)
        or not isinstance(threshold, (int, float))
        or not math.isfinite(threshold)
        or not 0 < threshold <= 1
    ):
        raise RequestError("threshold must be a number in (0, 1]")
    texts = req.get("texts")
    if not isinstance(texts, list) or not all(isinstance(t, str) for t in texts):
        raise RequestError("texts must be a list of strings")
    for t in texts:
        try:
            t.encode("utf-8")
        except UnicodeEncodeError:
            raise RequestError("texts must be valid UTF-8")
    return label, threshold, texts


def windows(text, window_words, overlap_words):
    """Character ranges of text that each hold at most window_words words.

    The model ignores every word past its input window, so a long text is read
    in windows that overlap by overlap_words words: a name of up to that many
    words that one division splits lies whole inside the next window.
    """
    words = [m.span() for m in WORD.finditer(text)]
    if len(words) <= window_words:
        return [(0, len(text))] if words else []
    step = window_words - overlap_words
    ranges = []
    for first in range(0, len(words), step):
        last = min(first + window_words, len(words)) - 1
        ranges.append((words[first][0], words[last][1]))
        if last == len(words) - 1:
            break
    return ranges


def text_spans(model, text, label, threshold):
    """The model's spans of text at threshold or more, as UTF-8 byte ranges."""
    found = set()
    for lo, hi in windows(text, model.window_words, model.overlap_words):
        for start, end, score in model.predict(text[lo:hi], label, threshold):
            if score >= threshold and 0 <= start < end <= hi - lo:
                found.add((lo + start, lo + end))
    if not found:
        return []
    byte_at = [0]
    for ch in text:
        byte_at.append(byte_at[-1] + len(ch.encode("utf-8")))
    return [{"start": byte_at[s], "end": byte_at[e]} for s, e in sorted(found)]


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "name-model"
    sys_version = ""
    # Seconds a connection may stall mid-request before it is dropped, so one
    # stuck client cannot hold the one-request-at-a-time server.
    timeout = 60

    def do_POST(self):
        length = self.headers.get("Content-Length")
        if length is None or not length.isdigit():
            self._send(411, {"error": "Content-Length required"})
            return
        if int(length) > MAX_BODY_BYTES:
            self._send(413, {"error": "body too large"})
            return
        body = self.rfile.read(int(length))
        if self.path != "/v1/spans":
            self._send(404, {"error": "not found"})
            return
        model = self.server.model
        try:
            label, threshold, texts = parse_request(body, model.name)
        except RequestError as e:
            self._send(422, {"error": str(e)})
            return
        try:
            spans = [text_spans(model, t, label, threshold) for t in texts]
        except Exception as e:  # noqa: BLE001 - log the type only: a message may quote text
            self.log_message("inference failed: %s", type(e).__name__)
            self._send(500, {"error": "inference failed"})
            return
        self._send(200, {"model": model.name, "label": label, "threshold": threshold, "spans": spans})

    def do_GET(self):
        self._send(405 if self.path == "/v1/spans" else 404, {"error": "not found"})

    def _send(self, status, payload):
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(data)
        self.close_connection = True

    def log_message(self, format, *args):
        # The request line holds the method and path only; no body is logged.
        sys.stderr.write("name-model: " + (format % args) + "\n")


class Server(socketserver.UnixStreamServer):
    """Serves one request at a time on a listening socket it is handed."""

    def __init__(self, listener, model, idle_seconds):
        super().__init__(listener.getsockname(), Handler, bind_and_activate=False)
        self.socket.close()
        self.socket = listener
        self.model = model
        self.timeout = idle_seconds
        self.idle = False

    def handle_timeout(self):
        self.idle = True


def serve_until_idle(server):
    """Answer requests until none arrives for the server's idle period."""
    while not server.idle:
        server.handle_request()
    sys.stderr.write(f"name-model: no request for {server.timeout:g}s; exiting\n")
