-- Модуль refunds: адрес получателя возврата хранится только в нижнем регистре. Причина — см. 0011.
ALTER TABLE refunds.refunds
  ADD CONSTRAINT refunds_to_address_lowercase CHECK (to_address = lower(to_address));
