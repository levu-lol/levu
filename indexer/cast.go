package indexer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// CastChain reads chain state through Foundry's `cast`.
//
// Same reasoning as the settlement client: shelling out keeps the package
// dependency-free while the Chain interface stays exactly what a production RPC
// client would implement.
type CastChain struct {
	RPC string
}

func (c *CastChain) run(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "cast", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("cast %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *CastChain) Call(ctx context.Context, to, sig string, args ...string) ([]string, error) {
	full := append([]string{"call", to, sig}, args...)
	full = append(full, "--rpc-url", c.RPC)
	out, err := c.run(ctx, full...)
	if err != nil {
		return nil, err
	}
	var vals []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			// cast annotates some values, e.g. "1699999999 [1.699e9]".
			vals = append(vals, strings.Fields(line)[0])
		}
	}
	return vals, nil
}

func (c *CastChain) BlockNumber(ctx context.Context) (int64, error) {
	out, err := c.run(ctx, "block-number", "--rpc-url", c.RPC)
	if err != nil {
		return 0, err
	}
	var n int64
	if _, err := fmt.Sscan(out, &n); err != nil {
		return 0, fmt.Errorf("parse block number %q: %w", out, err)
	}
	return n, nil
}

func (c *CastChain) Logs(ctx context.Context, addr, topic string, from, to int64) ([]Log, error) {
	out, err := c.run(ctx, "logs", "--from-block", fmt.Sprint(from), "--to-block", fmt.Sprint(to),
		"--address", addr, topic, "--json", "--rpc-url", c.RPC)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	var raw []struct {
		BlockNumber string `json:"blockNumber"`
		Data        string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse logs: %w", err)
	}
	logs := make([]Log, 0, len(raw))
	for _, r := range raw {
		b, err := hex.DecodeString(strings.TrimPrefix(r.Data, "0x"))
		if err != nil {
			continue
		}
		var bn int64
		fmt.Sscanf(strings.TrimPrefix(r.BlockNumber, "0x"), "%x", &bn)
		logs = append(logs, Log{BlockNumber: bn, Data: b})
	}
	return logs, nil
}
