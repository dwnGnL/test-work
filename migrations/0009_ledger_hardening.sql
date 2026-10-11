-- Модуль ledger: закрываем обходные пути вокруг журнала (правки по итогам ревью каркаса).
-- Файл 0006 не менялся: он уже применён, исправления идут новой миграцией.

-- 1. Проводки пишутся только в транзакцию, созданную в этой же транзакции базы.
--    Без этого повтор, нашедший заголовок по ключу идемпотентности, мог бы дописать в него
--    вторую сбалансированную пару: сумма по-прежнему ноль, а зачисление удвоено.
--    xid8 — 64-битный номер транзакции без оборота счётчика; pg_current_xact_id() возвращает
--    номер верхнего уровня, поэтому SAVEPOINT внутри транзакции проверку не ломает.
ALTER TABLE ledger.transactions
  ADD COLUMN xact_id xid8 NOT NULL DEFAULT pg_current_xact_id();

CREATE FUNCTION ledger.check_entry_in_own_transaction() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM ledger.transactions t
    WHERE t.id = NEW.transaction_id AND t.xact_id = pg_current_xact_id()
  ) THEN
    RAISE EXCEPTION 'ledger: entry for transaction % rejected: the transaction was not created in the current database transaction',
      NEW.transaction_id
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER entries_own_transaction_only
  BEFORE INSERT ON ledger.entries
  FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_in_own_transaction();

-- 2. TRUNCATE не вызывает построчные и операторные триггеры UPDATE/DELETE,
--    поэтому запрет из 0006 его не останавливал. Отдельные триггеры на каждую таблицу.
CREATE TRIGGER entries_no_truncate
  BEFORE TRUNCATE ON ledger.entries
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_modification();

CREATE TRIGGER transactions_no_truncate
  BEFORE TRUNCATE ON ledger.transactions
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_modification();

-- 3. Счета только добавляются: переименование или удаление счёта переписало бы смысл
--    уже записанных проводок. Запрещаем UPDATE, DELETE и TRUNCATE.
CREATE TRIGGER accounts_append_only
  BEFORE UPDATE OR DELETE OR TRUNCATE ON ledger.accounts
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_modification();

-- 4. Транзакция без проводок «занимала» бы ключ идемпотентности, ничего не проведя.
--    На коммите у каждой транзакции должно быть не меньше двух проводок.
--    (Одна проводка и так не пройдёт баланс: amount <> 0.)
CREATE FUNCTION ledger.check_transaction_has_entries() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  n BIGINT;
BEGIN
  SELECT count(*) INTO n FROM ledger.entries WHERE transaction_id = NEW.id;
  IF n < 2 THEN
    RAISE EXCEPTION 'ledger: transaction % has % entries, at least 2 are required', NEW.id, n
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER transactions_min_entries
  AFTER INSERT ON ledger.transactions
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION ledger.check_transaction_has_entries();

-- Обе отложенные проверки (баланс и число проводок) ищут проводки по transaction_id.
-- Без индекса каждая из них читала бы всю таблицу entries.
CREATE INDEX entries_transaction_idx ON ledger.entries (transaction_id);
