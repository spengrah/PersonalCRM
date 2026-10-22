"""Entry point: `serve` (the container's command) or `check` (the image build's).

`serve` answers on the listening socket systemd hands it (socket activation,
LISTEN_FDS=1, file descriptor 3) and exits once no request has arrived for
NAME_MODEL_IDLE_SECONDS; the next connection to the socket starts it again.
The model loads before the first accept, so a connection that started the
container waits in the socket's backlog and is answered once the model is up.

`check` loads the model and answers one synthetic text: run once while the
image builds with network, to fetch the weights into the image, and again with
the hub offline and the weights read-only, to prove the container needs
neither the network nor a writable disk.
"""

import os
import socket
import sys

from name_model.gliner_model import GlinerModel
from name_model.server import Server, serve_until_idle

LISTEN_FDS_START = 3


def serve():
    if os.environ.get("LISTEN_FDS") != "1":
        sys.exit("name-model: expected one socket from systemd (LISTEN_FDS=1)")
    listener = socket.socket(fileno=LISTEN_FDS_START)
    idle_seconds = float(os.environ.get("NAME_MODEL_IDLE_SECONDS", "300"))
    model = GlinerModel()
    sys.stderr.write("name-model: model loaded; serving\n")
    serve_until_idle(Server(listener, model, idle_seconds))


def check():
    model = GlinerModel()
    spans = model.predict("Alice Smith met Bob Jones in Paris.", "person", 0.5)
    if not spans:
        sys.exit("name-model: check found no person in the synthetic text")
    print(f"name-model: check found {len(spans)} person span(s)")


def main():
    command = sys.argv[1] if len(sys.argv) > 1 else "serve"
    if command == "serve":
        serve()
    elif command == "check":
        check()
    else:
        sys.exit(f"name-model: unknown command {command!r} (serve | check)")


if __name__ == "__main__":
    main()
