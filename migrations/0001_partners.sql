-- Модуль partners: партнёры и их настройки.
CREATE SCHEMA partners;

CREATE TABLE partners.partners (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name         TEXT NOT NULL,
  api_key_hash BYTEA NOT NULL UNIQUE,              -- sha256 от ключа, сам ключ не храним
  fee_bps      INT NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
