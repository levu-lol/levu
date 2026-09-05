package wire

import "fmt"

// ReceiptKind discriminates the receipt union.
type ReceiptKind uint8

const (
	RCollateralCredited   ReceiptKind = 1
	ROrderAccepted        ReceiptKind = 2
	RFilled               ReceiptKind = 3
	ROrderCancelled       ReceiptKind = 4
	ROracleUpdated        ReceiptKind = 5
	RFundingSettled       ReceiptKind = 6
	RLiquidated           ReceiptKind = 7
	RRiskParamsUpdated    ReceiptKind = 8
	RWithdrawalRequested  ReceiptKind = 9
	RWithdrawalSettled    ReceiptKind = 16
	RRejected             ReceiptKind = 10
	RInsuranceExhausted   ReceiptKind = 11
	RUnderwritten         ReceiptKind = 12
	RUnderwritingRedeemed ReceiptKind = 13
	RRentAccrued          ReceiptKind = 14
)

// Reject mirrors the VM's rejection reasons.
type Reject uint8

const (
	RejOutOfOrder             Reject = 1
	RejInsufficientMargin     Reject = 2
	RejUnknownOrder           Reject = 3
	RejNotOrderOwner          Reject = 4
	RejNonPositiveQuantity    Reject = 5
	RejNonPositivePrice       Reject = 6
	RejTickViolation          Reject = 7
	RejLotViolation           Reject = 8
	RejOpenInterestCap        Reject = 9
	RejReduceOnlyViolation    Reject = 10
	RejWouldCross             Reject = 11
	RejUnfillable             Reject = 12
	RejNotLiquidatable        Reject = 13
	RejNoMarkPrice            Reject = 14
	RejArithmeticFailure      Reject = 15
	RejInsufficientFreeMargin Reject = 16
	RejInvalidRiskParams      Reject = 17
	RejLaneInsolvent          Reject = 18
	RejOracleStale            Reject = 19
	RejOracleLowConfidence    Reject = 20
	RejFundingTooSoon         Reject = 21
	RejRentTooSoon            Reject = 22
	RejInsufficientShares     Reject = 23
	RejUnderwritingLocked     Reject = 24
)

var rejectNames = map[Reject]string{
	RejOutOfOrder: "out of order", RejInsufficientMargin: "insufficient margin",
	RejUnknownOrder: "unknown order", RejNotOrderOwner: "not order owner",
	RejNonPositiveQuantity: "non-positive quantity", RejNonPositivePrice: "non-positive price",
	RejTickViolation: "tick violation", RejLotViolation: "lot violation",
	RejOpenInterestCap: "open interest cap", RejReduceOnlyViolation: "reduce-only violation",
	RejWouldCross: "would cross", RejUnfillable: "unfillable",
	RejNotLiquidatable: "not liquidatable", RejNoMarkPrice: "no mark price",
	RejArithmeticFailure: "arithmetic failure", RejInsufficientFreeMargin: "insufficient free margin",
	RejInvalidRiskParams: "invalid risk params", RejLaneInsolvent: "lane insolvent",
	RejOracleStale: "oracle stale", RejOracleLowConfidence: "oracle low confidence",
	RejFundingTooSoon: "funding settled too soon",
	RejRentTooSoon:    "rent accrued too soon", RejInsufficientShares: "insufficient shares",
	RejUnderwritingLocked: "underwriting capital is backing open exposure",
}

func (r Reject) String() string {
	if n, ok := rejectNames[r]; ok {
		return n
	}
	return fmt.Sprintf("reject(%d)", uint8(r))
}

// Receipt is the flattened union of everything the VM reports.
//
// Fills arrive here and only here. The shadow book is rebuilt from these, which
// is what keeps Go's view derived from the VM rather than parallel to it.
type Receipt struct {
	Kind ReceiptKind

	Account    Account
	Amount     Fixed
	OrderID    uint64
	RestingQty Fixed

	MakerOrder uint64
	Maker      Account
	Taker      Account
	Price      Fixed
	Qty        Fixed
	TakerSide  Side
	TakerFee   Fixed
	MakerFee   Fixed

	Mark        Fixed
	Index       Fixed
	SizeClosed  Fixed
	BadDebt     Fixed
	Refunded    Fixed
	RejectCause Reject

	Uncovered      Fixed
	TotalUncovered Fixed
	Confidence     uint16

	// Funding settlement.
	Rate           Fixed
	AveragePremium Fixed
	Samples        uint64

	// Underwriting and capacity rent.
	Shares           Fixed
	RateLong         Fixed
	RateShort        Fixed
	IndexLong        Fixed
	IndexShort       Fixed
	UtilisationLong  Fixed
	UtilisationShort Fixed
}

func decodeReceipt(d *Decoder) (Receipt, error) {
	var r Receipt
	k, err := d.U8()
	if err != nil {
		return r, err
	}
	r.Kind = ReceiptKind(k)

	get := func(fns ...func() error) error {
		for _, fn := range fns {
			if err := fn(); err != nil {
				return err
			}
		}
		return nil
	}
	acct := func(dst *Account) func() error {
		return func() error { v, e := d.Account(); *dst = v; return e }
	}
	fx := func(dst *Fixed) func() error {
		return func() error { v, e := d.Fixed(); *dst = v; return e }
	}
	u64 := func(dst *uint64) func() error {
		return func() error { v, e := d.U64(); *dst = v; return e }
	}

	switch r.Kind {
	case RCollateralCredited:
		err = get(acct(&r.Account), fx(&r.Amount))
	case ROrderAccepted:
		err = get(u64(&r.OrderID), acct(&r.Account), fx(&r.RestingQty))
	case RFilled:
		err = get(u64(&r.MakerOrder), acct(&r.Maker), acct(&r.Taker), fx(&r.Price), fx(&r.Qty))
		if err == nil {
			var s uint8
			if s, err = d.U8(); err == nil {
				r.TakerSide = Side(s)
				err = get(fx(&r.TakerFee), fx(&r.MakerFee))
			}
		}
	case ROrderCancelled:
		err = get(u64(&r.OrderID), acct(&r.Account), fx(&r.Refunded))
	case ROracleUpdated:
		err = get(fx(&r.Price), fx(&r.Mark))
		if err == nil {
			r.Confidence, err = d.U16()
		}
	case RFundingSettled:
		err = get(fx(&r.Rate), fx(&r.Index), fx(&r.AveragePremium))
		if err == nil {
			r.Samples, err = d.U64()
		}
	case RLiquidated:
		err = get(acct(&r.Account), fx(&r.SizeClosed), fx(&r.BadDebt))
	case RRiskParamsUpdated:
		// no payload
	case RWithdrawalRequested:
		err = get(acct(&r.Account), fx(&r.Amount))
	case RInsuranceExhausted:
		err = get(fx(&r.Uncovered), fx(&r.TotalUncovered))
	case RUnderwritten:
		err = get(acct(&r.Account), fx(&r.Amount), fx(&r.Shares))
	case RUnderwritingRedeemed:
		err = get(acct(&r.Account), fx(&r.Shares), fx(&r.Amount))
	case RRentAccrued:
		err = get(fx(&r.RateLong), fx(&r.RateShort), fx(&r.IndexLong),
			fx(&r.IndexShort), fx(&r.UtilisationLong), fx(&r.UtilisationShort))
	case RWithdrawalSettled:
		err = get(acct(&r.Account), fx(&r.Amount))
	case RRejected:
		// Whose intent was refused. A batch's receipts come back as one list
		// and every caller waiting on that batch is handed all of them, so
		// without this there is no way to tell whose rejection is whose, and
		// one trader's refusal gets reported to another as their own.
		if err = get(acct(&r.Account)); err == nil {
			var c uint8
			c, err = d.U8()
			r.RejectCause = Reject(c)
		}
	default:
		return r, fmt.Errorf("wire: unknown receipt discriminant %d", k)
	}
	return r, err
}

// Response is a decoded successful reply: where the lane now is, what it
// commits to, and what it did.
type Response struct {
	Seq   uint64
	Epoch uint64
	Root  [32]byte
	// UncoveredDebt is loss the lane's insurance could not absorb. Any value
	// above zero means the lane is insolvent: it will refuse new exposure while
	// still permitting exits, and its root must not be settled as clean.
	UncoveredDebt Fixed
	// FundingIndex is the lane's cumulative funding index, carried into
	// settlement so the hub can bound how much funding one commitment applies.
	FundingIndex Fixed
	Receipts     []Receipt
}

// DecodeResponse parses a reply frame.
func DecodeResponse(frame []byte) (*Response, error) {
	d := NewDecoder(frame)
	status, err := d.U8()
	if err != nil {
		return nil, err
	}
	if status == StatusError {
		msg, err := d.String16()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("vm: %s", msg)
	}
	if status != StatusOK {
		return nil, fmt.Errorf("wire: unknown status %d", status)
	}

	var r Response
	if r.Seq, err = d.U64(); err != nil {
		return nil, err
	}
	if r.Epoch, err = d.U64(); err != nil {
		return nil, err
	}
	if r.Root, err = d.Hash(); err != nil {
		return nil, err
	}
	if r.UncoveredDebt, err = d.Fixed(); err != nil {
		return nil, err
	}
	if r.FundingIndex, err = d.Fixed(); err != nil {
		return nil, err
	}
	n, err := d.U32()
	if err != nil {
		return nil, err
	}
	r.Receipts = make([]Receipt, 0, n)
	for i := uint32(0); i < n; i++ {
		rec, err := decodeReceipt(d)
		if err != nil {
			return nil, fmt.Errorf("receipt %d: %w", i, err)
		}
		r.Receipts = append(r.Receipts, rec)
	}
	if d.Remaining() != 0 {
		return nil, fmt.Errorf("wire: %d trailing bytes in response", d.Remaining())
	}
	return &r, nil
}

// AccountList is the reply to OpLiquidatable: the lane's position plus the
// accounts currently below maintenance margin.
type AccountList struct {
	Seq           uint64
	Epoch         uint64
	Root          [32]byte
	UncoveredDebt Fixed
	FundingIndex  Fixed
	Accounts      []Account
}

// DecodeAccountList parses a liquidatable-accounts reply.
func DecodeAccountList(frame []byte) (*AccountList, error) {
	d := NewDecoder(frame)
	status, err := d.U8()
	if err != nil {
		return nil, err
	}
	if status == StatusError {
		msg, err := d.String16()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("vm: %s", msg)
	}
	var r AccountList
	if r.Seq, err = d.U64(); err != nil {
		return nil, err
	}
	if r.Epoch, err = d.U64(); err != nil {
		return nil, err
	}
	if r.Root, err = d.Hash(); err != nil {
		return nil, err
	}
	if r.UncoveredDebt, err = d.Fixed(); err != nil {
		return nil, err
	}
	if r.FundingIndex, err = d.Fixed(); err != nil {
		return nil, err
	}
	n, err := d.U32()
	if err != nil {
		return nil, err
	}
	r.Accounts = make([]Account, 0, n)
	for i := uint32(0); i < n; i++ {
		a, err := d.Account()
		if err != nil {
			return nil, err
		}
		r.Accounts = append(r.Accounts, a)
	}
	if d.Remaining() != 0 {
		return nil, fmt.Errorf("wire: %d trailing bytes", d.Remaining())
	}
	return &r, nil
}
