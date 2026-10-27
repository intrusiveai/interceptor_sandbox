FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
# Sandbox execution has no external network. Bake the SDK tokenizer into the
# application image, with a cache readable by the unprivileged application user.
ENV TIKTOKEN_CACHE_DIR=/app/tokenizer-cache
RUN mkdir -p "$TIKTOKEN_CACHE_DIR" && python -c 'import tiktoken; tiktoken.get_encoding("cl100k_base")' && chmod -R a+rX "$TIKTOKEN_CACHE_DIR"
COPY support_app.py ./
