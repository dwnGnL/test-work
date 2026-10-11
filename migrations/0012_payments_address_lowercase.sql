-- Модуль payments: адрес отправителя хранится только в нижнем регистре
-- (по нему сверяется чёрный список AML и адрес возврата). Причина — см. 0011.
ALTER TABLE payments.payments
  ADD CONSTRAINT payments_from_address_lowercase CHECK (from_address = lower(from_address));
