-- Симулятор сети (chainsim): адреса и контракт токена в переводах — только в нижнем регистре,
-- как их отдаёт сеть в логах. Причина — см. 0011.
ALTER TABLE chainsim.transfers
  ADD CONSTRAINT transfers_from_address_lowercase CHECK (from_address = lower(from_address)),
  ADD CONSTRAINT transfers_to_address_lowercase   CHECK (to_address = lower(to_address)),
  ADD CONSTRAINT transfers_token_lowercase        CHECK (token = lower(token));
