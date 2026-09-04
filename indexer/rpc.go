package indexer

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RPCChain reads chain state over JSON-RPC, in process.
//
// CastChain shells out to Foundry for every read, which is fine on a laptop and
// is not fine anywhere else: each call forks a ~28 MB binary, and a control
// plane polling a handful of markets every few seconds keeps several alive at
// once. On a 1 GB host that is enough to have them killed mid-read -- observed,
// not theorised: `cast call: signal: killed`, oracle confidence collapsing from
// 8,016 to 2,358 as sources dropped out, and a market that had earned leverage
// losing it again.
//
// The calls this codebase makes are all static ABI types -- one 32-byte word in,
// one or more words out -- so decoding them needs no ABI library, and selectors
// are taken from a table rather than computed, which keeps this dependency-free.
// A signature the table does not know is an error rather than a guess.
type RPCChain struct {
	RPC string
	// HTTP is optional; a sane client with a deadline is used when nil.
	HTTP *http.Client
}

func (c *RPCChain) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// selectors covers exactly the calls this package makes. Hardcoded rather than
// derived: computing them needs keccak256, and pulling in a hash dependency to
// recompute eight constants that have never changed is a poor trade.
var selectors = map[string]string{
	"getReserves()":                   "0x0902f1ac",
	"liquidity()":                     "0x1a686502",
	"slot0()":                         "0x3850c7bd",
	"token0()":                        "0x0dfe1681",
	"token1()":                        "0xd21220a7",
	"totalSupply()":                   "0x18160ddd",
	"decimals()":                      "0x313ce567",
	"balanceOf(address)":              "0x70a08231",
	"getPool(address,address,uint24)": "0x1698ee82",
}

// splitSig separates "name(argTypes)(returnTypes)" into its parts.
func splitSig(sig string) (selectorKey string, argTypes, retTypes []string, err error) {
	open := strings.Index(sig, "(")
	if open < 0 {
		return "", nil, nil, fmt.Errorf("indexer: %q is not a signature", sig)
	}
	close1 := strings.Index(sig[open:], ")")
	if close1 < 0 {
		return "", nil, nil, fmt.Errorf("indexer: %q has no argument list", sig)
	}
	close1 += open
	name := sig[:open]
	args := strings.TrimSpace(sig[open+1 : close1])
	rest := strings.TrimSpace(sig[close1+1:])
	if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
		rest = rest[1 : len(rest)-1]
	}
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		out := strings.Split(s, ",")
		for i := range out {
			out[i] = strings.TrimSpace(out[i])
		}
		return out
	}
	argTypes = split(args)
	retTypes = split(rest)
	return name + "(" + args + ")", argTypes, retTypes, nil
}

func encodeArg(typ, val string) (string, error) {
	switch {
	case typ == "address":
		h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(val)), "0x")
		if len(h) != 40 {
			return "", fmt.Errorf("indexer: %q is not an address", val)
		}
		return strings.Repeat("0", 24) + h, nil
	case strings.HasPrefix(typ, "uint") || strings.HasPrefix(typ, "int"):
		n, ok := new(big.Int).SetString(strings.TrimSpace(val), 10)
		if !ok {
			return "", fmt.Errorf("indexer: %q is not a number", val)
		}
		return fmt.Sprintf("%064s", n.Text(16)), nil
	}
	return "", fmt.Errorf("indexer: cannot encode argument type %q", typ)
}

// decodeWord renders one 32-byte word the way callers expect to read it:
// numbers in base 10, because that is what big.Int.SetString is given.
func decodeWord(word []byte, typ string) string {
	switch {
	case typ == "address":
		return "0x" + hex.EncodeToString(word[12:])
	case typ == "bool":
		if word[31] != 0 {
			return "true"
		}
		return "false"
	case strings.HasPrefix(typ, "uint"):
		return new(big.Int).SetBytes(word).String()
	case strings.HasPrefix(typ, "int"):
		n := new(big.Int).SetBytes(word)
		// Two's complement over the full word: every signed type here is
		// returned sign-extended to 32 bytes.
		if word[0]&0x80 != 0 {
			n.Sub(n, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		return n.String()
	}
	return "0x" + hex.EncodeToString(word)
}

type rpcReq struct {
	Jsonrpc string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *RPCChain) do(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(rpcReq{Jsonrpc: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.RPC, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	res, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: http %d", method, res.StatusCode)
	}
	var r rpcResp
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if r.Error != nil {
		return fmt.Errorf("%s: rpc %d: %s", method, r.Error.Code, r.Error.Message)
	}
	return json.Unmarshal(r.Result, out)
}

func (c *RPCChain) Call(ctx context.Context, to, sig string, args ...string) ([]string, error) {
	key, argTypes, retTypes, err := splitSig(sig)
	if err != nil {
		return nil, err
	}
	sel, ok := selectors[key]
	if !ok {
		return nil, fmt.Errorf("indexer: no selector known for %q; add it to the table", key)
	}
	if len(args) != len(argTypes) {
		return nil, fmt.Errorf("indexer: %s wants %d arguments, got %d", key, len(argTypes), len(args))
	}
	data := sel
	for i, a := range args {
		enc, err := encodeArg(argTypes[i], a)
		if err != nil {
			return nil, err
		}
		data += enc
	}

	var raw string
	if err := c.do(ctx, "eth_call", []any{
		map[string]string{"to": to, "data": data}, "latest",
	}, &raw); err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(strings.TrimPrefix(raw, "0x"))
	if err != nil {
		return nil, fmt.Errorf("indexer: %s returned unparseable data", key)
	}
	if len(retTypes) == 0 {
		retTypes = []string{"uint256"}
	}
	out := make([]string, 0, len(retTypes))
	for i, t := range retTypes {
		lo, hi := i*32, (i+1)*32
		if hi > len(b) {
			// A short return is a contract that is not what we think it is;
			// callers check length and treat it as an unusable venue.
			break
		}
		out = append(out, decodeWord(b[lo:hi], t))
	}
	return out, nil
}

func (c *RPCChain) BlockNumber(ctx context.Context) (int64, error) {
	var raw string
	if err := c.do(ctx, "eth_blockNumber", []any{}, &raw); err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(raw, "0x"), 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parse block number %q: %w", raw, err)
	}
	return n, nil
}

func (c *RPCChain) Logs(ctx context.Context, addr, topic string, from, to int64) ([]Log, error) {
	filter := map[string]any{
		"address":   addr,
		"topics":    []string{topic},
		"fromBlock": "0x" + strconv.FormatInt(from, 16),
		"toBlock":   "0x" + strconv.FormatInt(to, 16),
	}
	var raw []struct {
		BlockNumber string `json:"blockNumber"`
		Data        string `json:"data"`
	}
	if err := c.do(ctx, "eth_getLogs", []any{filter}, &raw); err != nil {
		return nil, err
	}
	logs := make([]Log, 0, len(raw))
	for _, r := range raw {
		b, err := hex.DecodeString(strings.TrimPrefix(r.Data, "0x"))
		if err != nil {
			continue
		}
		bn, err := strconv.ParseInt(strings.TrimPrefix(r.BlockNumber, "0x"), 16, 64)
		if err != nil {
			continue
		}
		logs = append(logs, Log{BlockNumber: bn, Data: b})
	}
	return logs, nil
}
