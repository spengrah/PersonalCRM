# PersonalCRM name model image: GLiNER behind a socket-activated HTTP endpoint
# (seam S2, POST /v1/spans). arm64, for the Pi and the staging VPS.
#
# Build context = name-model/, which is the whole of the model's sources: the
# image tag is the git tree hash of name-model/ (image-tag.sh), so the image is
# rebuilt only when something here changes.
#
# The weights are fetched while the image builds and are read-only in it; the
# container runs with no network (Network=none in its unit) and the hub offline.

FROM python:3.12-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    HF_HOME=/opt/hf \
    HF_HUB_DISABLE_TELEMETRY=1 \
    OMP_NUM_THREADS=2

WORKDIR /opt/name-model

# CPU-only torch first, from PyTorch's CPU index, so gliner's torch dependency
# is already met and no CUDA build is pulled.
RUN pip install --index-url https://download.pytorch.org/whl/cpu torch==2.5.1
COPY requirements.txt .
RUN pip install -r requirements.txt

COPY name_model ./name_model
COPY units ./units

# Fetch the weights (and the encoder's tokenizer) into HF_HOME.
RUN python -m name_model check

# Prove the container needs neither the network nor a writable disk: load and
# predict again as the runtime user, with the hub offline and the weights
# read-only.
RUN useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin namemodel \
    && chmod -R a+rX,a-w /opt/hf /opt/name-model
ENV HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1
USER namemodel
RUN python -m name_model check

ENTRYPOINT ["python", "-m", "name_model"]
CMD ["serve"]
