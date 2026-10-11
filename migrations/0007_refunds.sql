-- Модуль refunds: возвраты отправителям. payment_id — ссылка по id, без внешнего ключа.
CREATE SCHEMA refunds;

CREATE TABLE refunds.refunds (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  payment_id BIGINT NOT NULL UNIQUE,               -- один платёж — один возврат
  to_address TEXT NOT NULL,
  amount     BIGINT NOT NULL CHECK (amount > 0),
  status     TEXT NOT NULL CHECK (status IN ('created','ready_to_send')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
