-- ДЕМО-ДАННЫЕ. Только для разработки и проверки сценария: API-ключи ниже публичны.
-- Это единственная миграция, которая пишет сразу в схемы двух модулей (partners и ledger):
-- счёт partner:{id} подставляется подзапросом, чтобы не зашивать id в файл.

-- Ключи: demo-key-alpha и demo-key-beta; в базе только их sha256.
INSERT INTO partners.partners (name, api_key_hash, fee_bps) VALUES
  ('alpha', sha256('demo-key-alpha'::bytea), 150),
  ('beta',  sha256('demo-key-beta'::bytea),  50);

-- Счета партнёров в ledger.
INSERT INTO ledger.accounts (code)
SELECT 'partner:' || id
FROM partners.partners
WHERE name IN ('alpha', 'beta')
ORDER BY id;
