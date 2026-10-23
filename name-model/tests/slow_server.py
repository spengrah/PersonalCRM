"""Run the real Unix-socket endpoint with synthetic, slow window inference.

The Go client regression tests launch this process; no GLiNER weights are loaded.
"""

import socket
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from name_model.gliner_model import GlinerModel
from name_model.server import Server, serve_until_idle


class SlowModel:
    name = GlinerModel.name
    window_words = GlinerModel.window_words
    overlap_words = GlinerModel.overlap_words

    def __init__(self):
        self.calls = 0

    def predict(self, text, label, threshold):
        self.calls += 1
        if text.startswith("begin "):
            print("started", flush=True)
        time.sleep(0.01)
        marker = "Testperson"
        start = text.find(marker)
        if start < 0:
            return []
        print(f"finished after {self.calls} windows", file=sys.stderr, flush=True)
        self.calls = 0
        return [(start, start + len(marker), 1.0)]


if __name__ == "__main__":
    listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    listener.bind(sys.argv[1])
    listener.listen()
    with Server(listener, SlowModel(), idle_seconds=60) as server:
        print("ready", flush=True)
        serve_until_idle(server)
