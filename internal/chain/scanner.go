package chain

import (
	"context"
	"errors"
)

type Scanner struct {
	Source   Reader
	Deposits Deposits
}

type ScanResult struct {
	Blocks   int
	Isolated int
}

func (s Scanner) Scan(ctx context.Context) (ScanResult, error) {
	for range 3 {
		batch, err := s.Source.ReadBatch(ctx)
		if err != nil {
			return ScanResult{}, err
		}
		result := ScanResult{Blocks: len(batch.Blocks)}
		var candidates []Candidate
		for _, block := range batch.Blocks {
			for _, event := range block.Events {
				amount, accepted, overflow := event.DepositAmount()
				if overflow {
					result.Isolated++
				}
				if accepted {
					candidates = append(candidates, Candidate{Event: event, BlockHash: block.Hash, Amount: amount})
				}
			}
		}
		if err = s.Deposits.ApplyScan(ctx, batch, candidates); errors.Is(err, ErrStale) {
			continue
		}
		return result, err
	}
	return ScanResult{}, ErrStale
}
