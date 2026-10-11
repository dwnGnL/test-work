-- Модуль ledger: двойная запись. Журнал только на добавление.
CREATE SCHEMA ledger;

CREATE TABLE ledger.accounts (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  code       TEXT NOT NULL UNIQUE,                 -- assets:deposits, hold:aml, partner:{id}, refunds:payable, revenue:fees
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ledger.transactions (
  id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,            -- {payment_id}:{шаг}; зачисление и возврат — один шаг
  kind            TEXT NOT NULL,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ledger.entries (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  transaction_id BIGINT NOT NULL REFERENCES ledger.transactions(id),
  account_id     BIGINT NOT NULL REFERENCES ledger.accounts(id),
  amount         BIGINT NOT NULL CHECK (amount <> 0)  -- дебет > 0, кредит < 0; сумма по транзакции равна нулю
);

CREATE INDEX entries_account_idx ON ledger.entries (account_id);

-- Баланс транзакции: в момент коммита сумма проводок обязана быть равна нулю.
-- sum(bigint) в Postgres возвращает numeric, поэтому переполнения нет.
CREATE FUNCTION ledger.check_transaction_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  total NUMERIC;
BEGIN
  SELECT sum(amount) INTO total FROM ledger.entries WHERE transaction_id = NEW.transaction_id;
  IF total <> 0 THEN
    RAISE EXCEPTION 'ledger: transaction % is not balanced (sum of entries = %)', NEW.transaction_id, total
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER entries_balanced
  AFTER INSERT ON ledger.entries
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION ledger.check_transaction_balanced();

-- Журнал только на добавление: UPDATE и DELETE запрещены.
CREATE FUNCTION ledger.forbid_modification() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'ledger: % on %.% is forbidden, the journal is append-only',
    TG_OP, TG_TABLE_SCHEMA, TG_TABLE_NAME
    USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER entries_append_only
  BEFORE UPDATE OR DELETE ON ledger.entries
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_modification();

CREATE TRIGGER transactions_append_only
  BEFORE UPDATE OR DELETE ON ledger.transactions
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_modification();

-- Системные счета.
INSERT INTO ledger.accounts (code) VALUES
  ('assets:deposits'),
  ('hold:aml'),
  ('refunds:payable'),
  ('revenue:fees');
