// Package wire is the byte-level contract with the Rust PerpVM.
//
// It is hand-written and mirrors perpvm/src/wire.rs field for field. Both
// sides feed a state root, so a reordered field or a varint would fork the
// chain rather than fail loudly — hence fixed-width big-endian everywhere, and
// the pinned-bytes tests on both sides.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Opcodes.
const (
	OpApplyBatch   uint8 = 1
	OpRollover     uint8 = 2
	OpStatus       uint8 = 3
	OpLiquidatable uint8 = 4
	OpAccountProof uint8 = 5
	OpSnapshot     uint8 = 6
	// OpState exports a lane's full state; OpRestore installs one. Distinct
	// from OpSnapshot, which is the public withdrawal payload and deliberately
	// carries balances and nothing else.
	OpState   uint8 = 7
	OpRestore uint8 = 8
	// OpBookDepth reports resting depth in the lane's own book. Distinct from
	// the spot venue's depth, which is a different pool of liquidity.
	OpBookDepth uint8 = 9
	// OpAccountOrders lists one account's resting orders.
	//
	// A read op rather than something the control plane tracks alongside: the
	// book is the authority on what is resting, because an order fills when
	// somebody else trades and anything keeping its own copy learns that late
	// or not at all. A trader who cannot see a resting order cannot cancel it.
	OpAccountOrders uint8 = 10
	// OpBookLevels reports the resting book aggregated by price, best first.
	//
	// OpBookDepth answers how much is resting inside a band, which is what the
	// risk engine needs. This answers what shape it is in, which is what a
	// trader needs: a book that is one order at one price behaves nothing like
	// the same total spread over twenty levels, and only the second can absorb
	// a close without walking the price.
	OpBookLevels uint8 = 11
)

// Response status.
const (
	StatusOK    uint8 = 0
	StatusError uint8 = 1
)

type Side uint8

const (
	Bid Side = 0
	Ask Side = 1
)

func (s Side) String() string {
	if s == Bid {
		return "bid"
	}
	return "ask"
}

type TimeInForce uint8

const (
	GoodTilCancel     TimeInForce = 0
	ImmediateOrCancel TimeInForce = 1
	FillOrKill        TimeInForce = 2
	PostOnly          TimeInForce = 3
)

// Account is a Robinhood Chain address.
type Account [20]byte

// RiskParams mirrors the VM struct. The control plane supplies these; the VM
// enforces them. The Market Health Engine will later emit this same shape.
type RiskParams struct {
	InitialMarginLong  Fixed
	InitialMarginShort Fixed
	MaintenanceMargin  Fixed
	OICapLong          Fixed
	OICapShort         Fixed
	TakerFee           Fixed
	MakerFee           Fixed
	LiquidationFee     Fixed
	TickSize           Fixed
	LotSize            Fixed
	// MarkBand caps how far the mark may deviate from the index, bounding what
	// book manipulation can do to margin and liquidation prices.
	MarkBand Fixed
	// MarginSlope is the extra margin fraction charged per unit of notional,
	// so the leverage a trader actually receives falls as their position grows.
	// Zero is the flat behaviour every lane had before it existed.
	MarginSlope Fixed
	// MaxOracleStaleness is measured in sequence numbers, not wall-clock: the
	// VM has no clock and a replay must reach the same conclusion.
	MaxOracleStaleness uint64
	// MinConfidence is the oracle confidence floor in basis points.
	MinConfidence uint16
	// Funding parameters. The VM derives the rate from these and its own
	// premium samples; the control plane never supplies a rate.
	FundingInterest       Fixed
	FundingClamp          Fixed
	FundingCap            Fixed
	FundingImpactNotional Fixed
	MinFundingInterval    uint64
	// Capacity rent: the price of scarce safe exposure. Unlike funding this is
	// not zero-sum among traders — both sides pay, and the proceeds go to
	// whoever bears the lane's risk.
	RentBase             Fixed
	RentKink             Fixed
	RentSlopeBelow       Fixed
	RentSlopeAbove       Fixed
	RentToUnderwriters   Fixed
	RentToInsurance      Fixed
	RentToProtocol       Fixed
	UnderwritingMultiple Fixed
	MinRentInterval      uint64
	MinUnderwritingRatio Fixed
	ReduceOnly           bool
}

// ConservativeParams matches RiskParams::conservative_default() in the VM:
// 2x both sides, tight caps, no maker rebate.
func ConservativeParams() RiskParams {
	return RiskParams{
		InitialMarginLong:     FixedRawInt64(500_000_000_000_000_000),
		InitialMarginShort:    FixedRawInt64(500_000_000_000_000_000),
		MaintenanceMargin:     FixedRawInt64(300_000_000_000_000_000),
		OICapLong:             FixedWhole(1_000_000),
		OICapShort:            FixedWhole(1_000_000),
		TakerFee:              FixedRawInt64(500_000_000_000_000),
		MakerFee:              FixedZero(),
		LiquidationFee:        FixedRawInt64(10_000_000_000_000_000),
		TickSize:              FixedRawInt64(1),
		LotSize:               FixedRawInt64(1),
		MarkBand:              FixedRawInt64(5_000_000_000_000_000), // 0.005 == 50 bps
		MarginSlope:           Fixed{},                              // flat unless a lane sets it
		MaxOracleStaleness:    5_000,
		MinConfidence:         5_000,
		FundingInterest:       FixedRawInt64(100_000_000_000_000),   // 1 bp
		FundingClamp:          FixedRawInt64(500_000_000_000_000),   // 0.0005
		FundingCap:            FixedRawInt64(7_500_000_000_000_000), // 0.0075
		FundingImpactNotional: FixedWhole(10_000),
		MinFundingInterval:    1_000,
		RentBase:              FixedRawInt64(10_000_000_000_000),
		RentKink:              FixedRawInt64(800_000_000_000_000_000),
		RentSlopeBelow:        FixedRawInt64(50_000_000_000_000),
		RentSlopeAbove:        FixedRawInt64(2_000_000_000_000_000),
		RentToUnderwriters:    FixedRawInt64(700_000_000_000_000_000),
		RentToInsurance:       FixedRawInt64(200_000_000_000_000_000),
		RentToProtocol:        FixedRawInt64(100_000_000_000_000_000),
		UnderwritingMultiple:  FixedWhole(3),
		MinRentInterval:       1_000,
		MinUnderwritingRatio:  FixedRawInt64(50_000_000_000_000_000),
		ReduceOnly:            false,
	}
}

// ---------------------------------------------------------------------------
// Intents
// ---------------------------------------------------------------------------

// Intent is one instruction for the VM. There is deliberately no Fill variant:
// the control plane decides order, the VM decides outcome.
type Intent interface{ encode(*Encoder) }

type CreditCollateral struct {
	Account Account
	Amount  Fixed
}

type PlaceOrder struct {
	Account Account
	Side    Side
	Price   Fixed
	Qty     Fixed
	TIF     TimeInForce
}

type CancelOrder struct {
	Account Account
	OrderID uint64
}

// UpdateOracle carries the aggregated index and its confidence in basis
// points. Aggregation happens in the control plane; the VM records the figure
// and enforces the consequences.
type UpdateOracle struct {
	Price      Fixed
	Confidence uint16
}

// SettleFunding settles one funding interval.
//
// It deliberately carries no rate: the VM derives one from the premium samples
// its own proof covers. A rate supplied here would let whoever sequences
// transfer value between longs and shorts at will.
type SettleFunding struct{}

// Underwrite commits collateral as first-loss capital backing a lane.
type Underwrite struct {
	Account Account
	Amount  Fixed
}

// RedeemUnderwriting returns first-loss capital to collateral. Refused while it
// is still backing open exposure.
type RedeemUnderwriting struct {
	Account Account
	Shares  Fixed
}

// AccrueRent advances the capacity rent indices. Carries no rate: the VM
// derives one from utilisation, for the same reason funding does.
type AccrueRent struct{}
type Liquidate struct{ Account Account }
type SetRiskParams struct{ Params RiskParams }

type RequestWithdrawal struct {
	Account Account
	Amount  Fixed
}

func (i CreditCollateral) encode(e *Encoder) {
	e.U8(1).Bytes(i.Account[:]).Fixed(i.Amount)
}
func (i PlaceOrder) encode(e *Encoder) {
	e.U8(2).Bytes(i.Account[:]).U8(uint8(i.Side)).Fixed(i.Price).Fixed(i.Qty).U8(uint8(i.TIF))
}
func (i CancelOrder) encode(e *Encoder) {
	e.U8(3).Bytes(i.Account[:]).U64(i.OrderID)
}
func (i UpdateOracle) encode(e *Encoder)  { e.U8(4).Fixed(i.Price).U16(i.Confidence) }
func (i SettleFunding) encode(e *Encoder) { e.U8(5) }
func (i Underwrite) encode(e *Encoder)    { e.U8(9).Bytes(i.Account[:]).Fixed(i.Amount) }
func (i RedeemUnderwriting) encode(e *Encoder) {
	e.U8(10).Bytes(i.Account[:]).Fixed(i.Shares)
}
func (i AccrueRent) encode(e *Encoder)    { e.U8(11) }
func (i Liquidate) encode(e *Encoder)     { e.U8(6).Bytes(i.Account[:]) }
func (i SetRiskParams) encode(e *Encoder) { e.U8(7); encodeParams(e, i.Params) }
func (i RequestWithdrawal) encode(e *Encoder) {
	e.U8(8).Bytes(i.Account[:]).Fixed(i.Amount)
}

// SettleWithdrawal pays out a requested withdrawal: the collateral leaves.
//
// RequestWithdrawal only records the request, freezing the amount out of free
// margin until a settlement layer pays it. On a lane running without one that
// is a ratchet -- the collateral is frozen and never released -- so the caller
// has to perform the settlement the hub would.
type SettleWithdrawal struct {
	Account Account
}

func (i SettleWithdrawal) encode(e *Encoder) {
	e.U8(13).Bytes(i.Account[:])
}

func encodeParams(e *Encoder, p RiskParams) {
	e.Fixed(p.InitialMarginLong).
		Fixed(p.InitialMarginShort).
		Fixed(p.MaintenanceMargin).
		Fixed(p.OICapLong).
		Fixed(p.OICapShort).
		Fixed(p.TakerFee).
		Fixed(p.MakerFee).
		Fixed(p.LiquidationFee).
		Fixed(p.TickSize).
		Fixed(p.LotSize).
		Fixed(p.MarkBand).
		Fixed(p.MarginSlope).
		U64(p.MaxOracleStaleness).
		U16(p.MinConfidence).
		Fixed(p.FundingInterest).
		Fixed(p.FundingClamp).
		Fixed(p.FundingCap).
		Fixed(p.FundingImpactNotional).
		U64(p.MinFundingInterval).
		Fixed(p.RentBase).
		Fixed(p.RentKink).
		Fixed(p.RentSlopeBelow).
		Fixed(p.RentSlopeAbove).
		Fixed(p.RentToUnderwriters).
		Fixed(p.RentToInsurance).
		Fixed(p.RentToProtocol).
		Fixed(p.UnderwritingMultiple).
		U64(p.MinRentInterval).
		Fixed(p.MinUnderwritingRatio)
	if p.ReduceOnly {
		e.U8(1)
	} else {
		e.U8(0)
	}
}

// SequencedIntent binds an intent to its position in the lane's canonical order.
type SequencedIntent struct {
	Seq    uint64
	Intent Intent
	// Sig is a 65-byte secp256k1 signature (r‖s‖v) authorising this intent, or
	// nil while the sequencer is trusted.
	//
	// Carried and committed, not verified. The field exists now because the
	// encoding is about to become load-bearing: the VM commits it in the
	// transaction root, so an inclusion proof covers who authorised an order
	// and not only that it was ordered. Adding it after users held proofs would
	// move every leaf they had proved against.
	Sig *[65]byte
}

// ---------------------------------------------------------------------------
// Encoder
// ---------------------------------------------------------------------------

type Encoder struct{ buf []byte }

func NewEncoder() *Encoder { return &Encoder{buf: make([]byte, 0, 256)} }

func (e *Encoder) U8(v uint8) *Encoder { e.buf = append(e.buf, v); return e }
func (e *Encoder) U16(v uint16) *Encoder {
	e.buf = binary.BigEndian.AppendUint16(e.buf, v)
	return e
}
func (e *Encoder) U32(v uint32) *Encoder {
	e.buf = binary.BigEndian.AppendUint32(e.buf, v)
	return e
}
func (e *Encoder) U64(v uint64) *Encoder {
	e.buf = binary.BigEndian.AppendUint64(e.buf, v)
	return e
}
func (e *Encoder) Fixed(f Fixed) *Encoder  { e.buf = append(e.buf, f[:]...); return e }
func (e *Encoder) Bytes(b []byte) *Encoder { e.buf = append(e.buf, b...); return e }
func (e *Encoder) Reset()                  { e.buf = e.buf[:0] }
func (e *Encoder) Bytes_() []byte          { return e.buf }

// EncodeApplyBatch builds an ApplyBatch request frame body.
func EncodeApplyBatch(e *Encoder, marketID uint32, batch []SequencedIntent) []byte {
	e.Reset()
	e.U8(OpApplyBatch).U32(marketID).U32(uint32(len(batch)))
	for _, si := range batch {
		si.encode(e)
	}
	return e.buf
}

// encode writes one sequenced intent: seq, then an optional signature, then
// the intent. The layout is pinned against the Rust decoder in
// TestTheSequencedIntentEnvelopeMatchesRust; a field added on one side and not
// the other forks the transaction root silently.
func (si SequencedIntent) encode(e *Encoder) {
	e.U64(si.Seq)
	if si.Sig == nil {
		e.U8(0)
	} else {
		e.U8(1).Bytes(si.Sig[:])
	}
	si.Intent.encode(e)
}

// EncodeRestore frames a restore request: the root the caller expects the
// state to reproduce, then the state itself.
func EncodeRestore(e *Encoder, marketID uint32, expectedRoot [32]byte, state []byte) []byte {
	e.Reset()
	e.U8(OpRestore).U32(marketID).Bytes(expectedRoot[:]).U32(uint32(len(state))).Bytes(state)
	return e.buf
}

// DecodeStateBlob pulls the state payload out of an OpState reply, after the
// standard ok-header the VM puts on every successful response.
func DecodeStateBlob(frame []byte) ([]byte, error) {
	// Parsed by hand rather than through DecodeResponse: the trailing u32 here
	// is a byte length, where every other reply puts a receipt count.
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
	for i := 0; i < 2; i++ { // seq, epoch
		if _, err := d.U64(); err != nil {
			return nil, err
		}
	}
	if _, err := d.Hash(); err != nil { // root
		return nil, err
	}
	for i := 0; i < 2; i++ { // uncoveredDebt, fundingIndex
		if _, err := d.Fixed(); err != nil {
			return nil, err
		}
	}
	n, err := d.U32()
	if err != nil {
		return nil, err
	}
	blob, err := d.Take(int(n))
	if err != nil {
		return nil, err
	}
	if d.Remaining() != 0 {
		return nil, fmt.Errorf("wire: %d trailing bytes after state", d.Remaining())
	}
	return blob, nil
}

// DecodeBookDepth parses an OpBookDepth reply: the standard ok-header, then a
// count and two fixed values. Parsed by hand for the same reason as the state
// reply -- the trailing u32 is a field count here, where every other reply puts
// a receipt count.
func DecodeBookDepth(frame []byte) (bid, ask Fixed, err error) {
	d := NewDecoder(frame)
	status, err := d.U8()
	if err != nil {
		return Fixed{}, Fixed{}, err
	}
	if status == StatusError {
		msg, err := d.String16()
		if err != nil {
			return Fixed{}, Fixed{}, err
		}
		return Fixed{}, Fixed{}, fmt.Errorf("vm: %s", msg)
	}
	if status != StatusOK {
		return Fixed{}, Fixed{}, fmt.Errorf("wire: unknown status %d", status)
	}
	for i := 0; i < 2; i++ { // seq, epoch
		if _, err := d.U64(); err != nil {
			return Fixed{}, Fixed{}, err
		}
	}
	if _, err := d.Hash(); err != nil { // root
		return Fixed{}, Fixed{}, err
	}
	for i := 0; i < 2; i++ { // uncoveredDebt, fundingIndex
		if _, err := d.Fixed(); err != nil {
			return Fixed{}, Fixed{}, err
		}
	}
	n, err := d.U32()
	if err != nil {
		return Fixed{}, Fixed{}, err
	}
	if n != 2 {
		return Fixed{}, Fixed{}, fmt.Errorf("wire: book depth reply has %d fields, want 2", n)
	}
	if bid, err = d.Fixed(); err != nil {
		return Fixed{}, Fixed{}, err
	}
	if ask, err = d.Fixed(); err != nil {
		return Fixed{}, Fixed{}, err
	}
	return bid, ask, nil
}

func EncodeSimple(e *Encoder, op uint8, marketID uint32) []byte {
	e.Reset()
	e.U8(op).U32(marketID)
	return e.buf
}

// ---------------------------------------------------------------------------
// Decoder
// ---------------------------------------------------------------------------

var ErrTruncated = errors.New("wire: frame truncated")

type Decoder struct {
	buf []byte
	pos int
}

func NewDecoder(b []byte) *Decoder { return &Decoder{buf: b} }

func (d *Decoder) Remaining() int { return len(d.buf) - d.pos }

// Take reads n raw bytes, for payloads whose shape the caller already knows.
func (d *Decoder) Take(n int) ([]byte, error) { return d.take(n) }

func (d *Decoder) take(n int) ([]byte, error) {
	if d.Remaining() < n {
		return nil, ErrTruncated
	}
	s := d.buf[d.pos : d.pos+n]
	d.pos += n
	return s, nil
}

func (d *Decoder) U8() (uint8, error) {
	b, err := d.take(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (d *Decoder) U16() (uint16, error) {
	b, err := d.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}

func (d *Decoder) U32() (uint32, error) {
	b, err := d.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b), nil
}

func (d *Decoder) U64() (uint64, error) {
	b, err := d.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(b), nil
}

func (d *Decoder) Fixed() (Fixed, error) {
	var f Fixed
	b, err := d.take(16)
	if err != nil {
		return f, err
	}
	copy(f[:], b)
	return f, nil
}

func (d *Decoder) Account() (Account, error) {
	var a Account
	b, err := d.take(20)
	if err != nil {
		return a, err
	}
	copy(a[:], b)
	return a, nil
}

func (d *Decoder) Hash() ([32]byte, error) {
	var h [32]byte
	b, err := d.take(32)
	if err != nil {
		return h, err
	}
	copy(h[:], b)
	return h, nil
}

func (d *Decoder) String16() (string, error) {
	n, err := d.U16()
	if err != nil {
		return "", err
	}
	b, err := d.take(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (d *Decoder) Errorf(format string, a ...any) error {
	return fmt.Errorf(format, a...)
}

// RestingOrder is one of an account's live orders, as the book holds it.
type RestingOrder struct {
	ID    uint64
	Side  Side
	Price Fixed
	Qty   Fixed
}

// EncodeAccountOrders frames a request for one account's resting orders.
func EncodeAccountOrders(e *Encoder, marketID uint32, account Account) []byte {
	e.Reset()
	e.U8(OpAccountOrders).U32(marketID).Bytes(account[:])
	return e.buf
}

// DecodeAccountOrders parses the reply: the standard ok-header, a count, then
// that many (id, side, price, qty) records.
// Level is one price in the book and everything resting at it.
type Level struct {
	Price Fixed
	Qty   Fixed
}

// DecodeBookLevels parses an OpBookLevels reply: the ok-header, then the bids
// length-prefixed, then the asks.
func DecodeBookLevels(frame []byte) (bids, asks []Level, err error) {
	d := NewDecoder(frame)
	status, err := d.U8()
	if err != nil {
		return nil, nil, err
	}
	if status == StatusError {
		msg, err := d.String16()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("vm: %s", msg)
	}
	if status != StatusOK {
		return nil, nil, fmt.Errorf("wire: unknown status %d", status)
	}
	for i := 0; i < 2; i++ { // seq, epoch
		if _, err := d.U64(); err != nil {
			return nil, nil, err
		}
	}
	if _, err := d.Hash(); err != nil { // root
		return nil, nil, err
	}
	for i := 0; i < 2; i++ { // uncoveredDebt, fundingIndex
		if _, err := d.Fixed(); err != nil {
			return nil, nil, err
		}
	}
	side := func() ([]Level, error) {
		n, err := d.U32()
		if err != nil {
			return nil, err
		}
		out := make([]Level, 0, n)
		for i := uint32(0); i < n; i++ {
			px, err := d.Fixed()
			if err != nil {
				return nil, err
			}
			qty, err := d.Fixed()
			if err != nil {
				return nil, err
			}
			out = append(out, Level{Price: px, Qty: qty})
		}
		return out, nil
	}
	if bids, err = side(); err != nil {
		return nil, nil, err
	}
	if asks, err = side(); err != nil {
		return nil, nil, err
	}
	if d.Remaining() != 0 {
		return nil, nil, fmt.Errorf("wire: %d trailing bytes after book levels", d.Remaining())
	}
	return bids, asks, nil
}

func DecodeAccountOrders(frame []byte) ([]RestingOrder, error) {
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
	for i := 0; i < 2; i++ { // seq, epoch
		if _, err := d.U64(); err != nil {
			return nil, err
		}
	}
	if _, err := d.Hash(); err != nil { // root
		return nil, err
	}
	for i := 0; i < 2; i++ { // uncoveredDebt, fundingIndex
		if _, err := d.Fixed(); err != nil {
			return nil, err
		}
	}
	n, err := d.U32()
	if err != nil {
		return nil, err
	}
	out := make([]RestingOrder, 0, n)
	for i := uint32(0); i < n; i++ {
		id, err := d.U64()
		if err != nil {
			return nil, err
		}
		sd, err := d.U8()
		if err != nil {
			return nil, err
		}
		px, err := d.Fixed()
		if err != nil {
			return nil, err
		}
		qty, err := d.Fixed()
		if err != nil {
			return nil, err
		}
		side := Bid
		if sd == 1 {
			side = Ask
		}
		out = append(out, RestingOrder{ID: id, Side: side, Price: px, Qty: qty})
	}
	return out, nil
}
