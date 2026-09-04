package health

import "github.com/levu-lol/levu/wire"

func wireBase() wire.RiskParams { return wire.ConservativeParams() }
func markBand() wire.Fixed      { return wire.FixedRawInt64(5_000_000_000_000_000) }
