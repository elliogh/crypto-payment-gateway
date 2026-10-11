package aml

import (
	"context"
	"errors"
	"testing"
)

func TestMockRiskAvailabilityAndCancellation(t *testing.T) {
	m := NewMock([]string{"0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, false)
	for _, tc := range []struct {
		sender string
		risk   Risk
	}{{"0xBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", High}, {"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Low}} {
		if risk, err := m.Check(context.Background(), Subject{Sender: tc.sender}); err != nil || risk != tc.risk {
			t.Fatal(risk, err)
		}
	}
	m.SetUnavailable(true)
	if _, err := m.Check(context.Background(), Subject{}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Check(ctx, Subject{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
