CREATE TABLE authors(id text PRIMARY KEY, name text NOT NULL);
CREATE TABLE documents(id text PRIMARY KEY, body text NOT NULL, metadata jsonb NOT NULL,
  author_id text REFERENCES authors(id), embedding vector(3));
CREATE INDEX ON documents USING hnsw (embedding vector_l2_ops);
CREATE TABLE counters(id text PRIMARY KEY, count integer NOT NULL);
CREATE TABLE refunds(id text PRIMARY KEY, amount integer NOT NULL CHECK(amount>0));
GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO agent;
