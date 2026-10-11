-- Модуль invoices: счета на оплату. partner_id — ссылка по id, без внешнего ключа.
CREATE SCHEMA invoices;

CREATE TABLE invoices.invoices (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  partner_id      BIGINT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_hash    BYTEA NOT NULL,                  -- sha256 тела запроса: тот же ключ с другим телом — ошибка
  amount          BIGINT NOT NULL CHECK (amount > 0),
  fee_bps         INT NOT NULL CHECK (fee_bps BETWEEN 0 AND 10000),  -- ставка, зафиксированная при создании
  deposit_address TEXT NOT NULL UNIQUE,
  expires_at      TIMESTAMPTZ NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (partner_id, idempotency_key)
);
