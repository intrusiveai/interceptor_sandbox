FROM python:3.12-slim
WORKDIR /app
RUN pip install --no-cache-dir 'psycopg[binary,pool]==3.2.10' 'pgvector==0.4.1' 'numpy==2.2.6'
COPY support_app.py /app/support_app.py
ENV PYTHONDONTWRITEBYTECODE=1
