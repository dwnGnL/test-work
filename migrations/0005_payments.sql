-- Модуль payments: платежи и очередь их обработки. invoice_id, partner_id — ссылки по id, без внешних ключей.
CREATE SCHEMA payments;

CREATE TABLE payments.payments (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  invoice_id      UUID NOT NULL,
  partner_id      BIGINT NOT NULL,
  tx_hash         TEXT NOT NULL,
  log_index       INT NOT NULL,
  from_address    TEXT NOT NULL,
  amount          BIGINT NOT NULL CHECK (amount > 0),
  fee_bps         INT NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),                    -- копия ставки счёта
  block_number    BIGINT,                          -- блок — изменяемый атрибут, не часть ключа
  block_hash      TEXT,
  status          TEXT NOT NULL CHECK (status IN ('detected','orphaned','aml_pending','credited','rejected')),
  aml_attempts    INT NOT NULL DEFAULT 0,
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tx_hash, log_index)                      -- ключ платежа: один перевод — одна запись, даже после реорга
);

CREATE INDEX payments_work_idx ON payments.payments (status, next_attempt_at);
CREATE INDEX payments_invoice_idx ON payments.payments (invoice_id);
