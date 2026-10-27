FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt ./
RUN python -m venv /app/.venv && python -m venv /opt/worker-venv \
    && /app/.venv/bin/pip install --no-cache-dir -r requirements.txt \
    && /opt/worker-venv/bin/pip install --no-cache-dir -r requirements.txt
COPY support_app.py web_client.py ./
# Exercise build-time CA installation even when the target defaults to non-root.
USER 20000:20000
