-- A stand-in for a service's own domain table, so the atomicity tests have real state to
-- commit or roll back alongside the outbox row.
CREATE TABLE thing (
  id   uuid PRIMARY KEY,
  name text NOT NULL
);
