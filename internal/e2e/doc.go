// Package e2e — сквозные тесты сервиса: весь сценарий из задания целиком, от счёта партнёра
// до зачисления или возврата. В пакете только тесты (package e2e_test), боевого кода нет.
//
// Окружение каждого теста собирается так же, как в cmd/server/main.go, на своей временной базе:
// миграции, два партнёра (alpha — комиссия 1,5%, beta — 0,5%), сеть-симулятор, AML-мок
// с чёрным списком, сканер с порогом в три подтверждения, обработчик платежей и HTTP-слой
// из httpapi.New с включёнными ручками /sim/*, поднятый через httptest.NewServer.
//
// Правила тестов:
//   - действия партнёра (/v1/*) и «сети» (/sim/*) идут только по HTTP, как в ручном сценарии
//     из README;
//   - время не ждём: сканер и обработчик платежей двигаются прямыми вызовами Tick, а паузу
//     между повторными проверками AML заменяет один UPDATE в helpers_test.go;
//   - деньги проверяем дважды: через HTTP (баланс, счёт) и через ledger (Totals, CheckBalanced).
//
// Что обещает система, по тестам:
//   - TestPaymentIsCreditedAfterConfirmationsAndAML — счёт, перевод, три подтверждения, AML,
//     зачисление за вычетом комиссии; чужой партнёр счёта не видит;
//   - TestHighRiskPaymentIsRejectedAndRefundedOnce — отправитель из чёрного списка: возврат
//     на его адрес, партнёру ничего, возврат один;
//   - TestReorgBeforeThresholdLeavesNoCredit — откат блока до порога: платёж orphaned, проводок нет;
//   - TestReorgBeforeThresholdThenReincludedIsCreditedOnce — та же транзакция в новом блоке:
//     тот же платёж, зачисление ровно одно;
//   - TestAMLOutageKeepsPaymentOnHoldUntilAMLReturns — AML недоступен: платёж ждёт в холде;
//   - TestScenarioSurvivesServiceRestarts — рестарт сервиса после каждого шага ничего не теряет
//     и не дублирует;
//   - TestConcurrentWorkersCreditEachPaymentOnce — несколько сканеров и обработчиков одновременно:
//     каждый платёж зачислен один раз;
//   - TestUnderAndOverpayment — недоплата, доплата и переплата;
//   - TestFakeTokenAndUnknownAddressAreIgnored — поддельный токен и чужой адрес денег не двигают.
package e2e
