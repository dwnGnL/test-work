package payments

// Status — статус платежа. Те же значения разрешает CHECK в миграции 0005.
type Status string

// Статусы платежа. Этот этап реализует переходы до aml_pending; решение AML (credited, rejected)
// — следующий этап. Переходы:
//
//	detected ──порог подтверждений──▶ aml_pending ──▶ credited | rejected
//	   ▲ │
//	   │ └──реорг до порога──▶ orphaned
//	   └────транзакция включена заново────┘
const (
	StatusDetected   Status = "detected"    // перевод замечен в блоке, подтверждений пока мало
	StatusOrphaned   Status = "orphaned"    // блок выброшен реоргом до порога, блока у платежа нет
	StatusAMLPending Status = "aml_pending" // порог пройден, деньги в холде, ждём ответа AML
	StatusCredited   Status = "credited"    // AML пропустил, платёж зачислен партнёру
	StatusRejected   Status = "rejected"    // AML отклонил, создан возврат
)
