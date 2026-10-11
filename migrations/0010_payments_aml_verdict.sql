-- Модуль payments: вердикт AML хранится в платеже. Модуль aml состояния не ведёт,
-- поэтому ответ проверки и время запроса записывает обработчик платежей.
-- Файл 0005 не менялся: он уже применён, исправления идут новой миграцией.
--
-- Условие задания: деньги не попадают на баланс партнёра до ответа AML.
-- Теперь это держит база: в credited платёж попадает только с вердиктом low,
-- в rejected (возврат) — только с вердиктом high.
--
-- IS NOT DISTINCT FROM обязателен: обычное сравнение с NULL даёт NULL, а CHECK
-- считает NULL выполненным, и платёж без вердикта проскочил бы в credited.
ALTER TABLE payments.payments
  ADD COLUMN aml_verdict    TEXT CHECK (aml_verdict IN ('low','high')),
  ADD COLUMN aml_checked_at TIMESTAMPTZ,
  ADD CONSTRAINT credited_only_low  CHECK (status <> 'credited' OR aml_verdict IS NOT DISTINCT FROM 'low'),
  ADD CONSTRAINT rejected_only_high CHECK (status <> 'rejected' OR aml_verdict IS NOT DISTINCT FROM 'high');
