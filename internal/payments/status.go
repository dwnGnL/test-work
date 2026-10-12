package payments

// Status — статус платежа. Те же значения разрешает CHECK в миграции 0005.
type Status string

// Статусы платежа. Переходы:
//
//	detected ──порог подтверждений──▶ aml_pending ──низкий риск──▶ credited
//	   ▲ │                                │
//	   │ └──реорг до порога──▶ orphaned   └──высокий риск──▶ rejected (создан возврат)
//	   └────транзакция включена заново────┘
//
// Средний риск (manual_review) и реорг после порога описаны в документе, в коде их нет.
const (
	StatusDetected   Status = "detected"    // перевод замечен в блоке, подтверждений пока мало
	StatusOrphaned   Status = "orphaned"    // блок выброшен реоргом до порога, блока у платежа нет
	StatusAMLPending Status = "aml_pending" // порог пройден, деньги в холде, ждём ответа AML
	StatusCredited   Status = "credited"    // AML пропустил, платёж зачислен партнёру
	StatusRejected   Status = "rejected"    // AML отклонил, создан возврат
)
