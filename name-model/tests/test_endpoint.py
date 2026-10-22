"""Tests of the name model container's HTTP endpoint (KNW-091).

These prove the knowledge behavior KNW-091, the name model container's promise.
The coverage scanner reads no Python, so the behavior is referenced here in
prose rather than by a citation marker.

Each test serves the real endpoint over a Unix socket, as the container does,
with a fake model in place of GLiNER: the weights cannot run in CI. The fake
reproduces the two traits of GLiNER the endpoint must handle: it reports
character offsets into the Python string it was given, and it silently ignores
every word past its input window.
"""

import http.client
import json
import os
import re
import socket
import sys
import tempfile
import threading
import types
import unittest
from unittest import mock

from name_model.gliner_model import GlinerModel
from name_model.server import Server, serve_until_idle

MODEL = "urchade/gliner_multi_pii-v1"

# GLiNER's word splitter, which decides where its input window ends.
_WORD = re.compile(r"\w+(?:[-_]\w+)*|\S")


class FakeModel:
    """Finds fixed names, each with a fixed score, within its input window."""

    name = MODEL
    window_words = 8
    overlap_words = 3

    def __init__(self, scores):
        self.scores = scores

    def predict(self, text, label, threshold):
        words = list(_WORD.finditer(text))
        if len(words) > self.window_words:
            text = text[: words[self.window_words - 1].end()]
        found = []
        for name, score in self.scores.items():
            for m in re.finditer(re.escape(name), text):
                found.append((m.start(), m.end(), score))
        return found


class _UnixConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost")
        self._path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(self._path)


class _ServedEndpoint(unittest.TestCase):
    """Serves the real endpoint over a Unix socket with the model make_model gives."""

    def make_model(self):
        raise NotImplementedError

    def setUp(self):
        self._dir = tempfile.TemporaryDirectory()
        self.path = os.path.join(self._dir.name, "name-model.sock")
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.bind(self.path)
        listener.listen()
        self.server = Server(listener, self.make_model(), idle_seconds=60)
        self.thread = threading.Thread(target=serve_until_idle, args=(self.server,), daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.idle = True
        # Wake the server from its wait for a connection so it sees the flag.
        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as wake:
                wake.connect(self.path)
        except OSError:
            pass
        self.thread.join(timeout=5)
        self.server.server_close()
        self._dir.cleanup()

    def request(self, method, path, body=None):
        conn = _UnixConnection(self.path)
        payload = None if body is None else json.dumps(body).encode()
        headers = {} if payload is None else {"Content-Type": "application/json"}
        conn.request(method, path, body=payload, headers=headers)
        resp = conn.getresponse()
        data = resp.read()
        conn.close()
        return resp.status, data

    def spans(self, texts, threshold=0.5):
        status, data = self.request(
            "POST",
            "/v1/spans",
            {"model": MODEL, "label": "person", "threshold": threshold, "texts": texts},
        )
        self.assertEqual(status, 200, data)
        body = json.loads(data)
        self.assertEqual(len(body["spans"]), len(texts))
        return body["spans"]

    def named(self, text, spans):
        """The substrings of text, as UTF-8 bytes, that the spans address."""
        raw = text.encode("utf-8")
        return [raw[s["start"] : s["end"]].decode("utf-8") for s in spans]


class EndpointTest(_ServedEndpoint):
    scores = {"Emily Rose": 0.9, "Bartholomew": 0.8, "Kit": 0.3}

    def make_model(self):
        return FakeModel(self.scores)

    # --- Spans cover the whole text, as UTF-8 byte offsets (PR2.c1) ---

    def test_name_past_the_input_window_is_returned(self):
        text = " ".join(["word"] * 40) + " then Bartholomew arrived."
        self.assertIn("Bartholomew", self.named(text, self.spans([text])[0]))

    def test_name_straddling_a_window_division_is_returned_whole(self):
        # Every placement of the two-word name across the first windows'
        # boundaries: some placement splits it between two windows.
        for lead in range(4, 14):
            text = " ".join(["word"] * lead) + " Emily Rose " + " ".join(["word"] * 10)
            with self.subTest(lead=lead):
                self.assertIn("Emily Rose", self.named(text, self.spans([text])[0]))

    def test_non_ascii_before_a_name_leaves_offsets_on_its_bytes(self):
        text = "“Café” 🎉 Bartholomew said “hi”"
        spans = self.spans([text])[0]
        self.assertEqual(self.named(text, spans), ["Bartholomew"])
        start = text.encode("utf-8").index("Bartholomew".encode("utf-8"))
        self.assertEqual(spans, [{"start": start, "end": start + len("Bartholomew")}])

    def test_empty_text_has_no_spans(self):
        self.assertEqual(self.spans([""]), [[]])

    def test_several_texts_align_by_index(self):
        texts = ["Bartholomew wrote.", "", "nothing here", "Emily Rose and Bartholomew"]
        got = [self.named(t, s) for t, s in zip(texts, self.spans(texts))]
        self.assertEqual(got, [["Bartholomew"], [], [], ["Emily Rose", "Bartholomew"]])

    def test_span_below_the_threshold_is_left_out(self):
        text = "Kit and Bartholomew"
        self.assertEqual(self.named(text, self.spans([text], threshold=0.5)[0]), ["Bartholomew"])
        self.assertEqual(
            self.named(text, self.spans([text], threshold=0.3)[0]), ["Kit", "Bartholomew"]
        )

    # --- A request it cannot answer as asked gets no spans (PR2.c2) ---

    def assertRefused(self, body, status=422):
        got, data = self.request("POST", "/v1/spans", body)
        self.assertEqual(got, status, data)
        self.assertNotIn("spans", json.loads(data))

    def test_other_model_is_refused(self):
        self.assertRefused({"model": "other/model", "label": "person", "threshold": 0.5, "texts": ["Kit"]})

    def test_threshold_outside_zero_to_one_is_refused(self):
        for threshold in [0, -0.1, 1.01, 2, "0.5", True, None]:
            with self.subTest(threshold=threshold):
                self.assertRefused({"model": MODEL, "label": "person", "threshold": threshold, "texts": ["Kit"]})

    def test_missing_texts_or_non_string_text_is_refused(self):
        for texts in [None, "Kit", ["Kit", 3], [None]]:
            body = {"model": MODEL, "label": "person", "threshold": 0.5}
            if texts is not None:
                body["texts"] = texts
            with self.subTest(texts=texts):
                self.assertRefused(body)

    def test_unknown_path_is_not_answered_with_spans(self):
        body = {"model": MODEL, "label": "person", "threshold": 0.5, "texts": ["Kit"]}
        for method, path in [("POST", "/v1/other"), ("POST", "/spans"), ("GET", "/v1/spans"), ("GET", "/")]:
            with self.subTest(method=method, path=path):
                status, data = self.request(method, path, body if method == "POST" else None)
                self.assertGreaterEqual(status, 400)
                self.assertNotIn(b'"spans"', data)



class _StrictGliner:
    """Stands in for the gliner library's model, which cannot run in CI. Like
    GLiNER's span decoder, it keeps a span only when its score is strictly
    greater than the threshold it is given."""

    scores = {"Bartholomew": 0.5, "Kit": 0.4}

    @classmethod
    def from_pretrained(cls, name):
        assert name == MODEL
        return cls()

    def eval(self):
        pass

    def predict_entities(self, text, labels, threshold):
        return [
            {"start": m.start(), "end": m.end(), "score": score}
            for name, score in self.scores.items()
            for m in re.finditer(re.escape(name), text)
            if score > threshold
        ]


class AdapterThresholdTest(_ServedEndpoint):
    """The endpoint through the image's GLiNER adapter, with the gliner library
    faked: the threshold promise holds across the adapter (PR2.c1, KNW-091)."""

    def make_model(self):
        with mock.patch.dict(sys.modules, {"gliner": types.SimpleNamespace(GLiNER=_StrictGliner)}):
            return GlinerModel()

    def test_span_scored_exactly_at_the_threshold_is_returned(self):
        text = "Kit and Bartholomew"
        self.assertEqual(self.named(text, self.spans([text], threshold=0.5)[0]), ["Bartholomew"])


if __name__ == "__main__":
    unittest.main()
