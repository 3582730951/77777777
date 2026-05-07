package server

import (
	"context"
	"math"
)

const bodyGateChunk = int64(1 << 20)

type bodyGate struct {
	tokens chan struct{}
}

type bodyReservation struct {
	gate     *bodyGate
	acquired int
	released bool
}

func newBodyGate(limitBytes int64) *bodyGate {
	if limitBytes <= 0 {
		return nil
	}
	n := int(math.Ceil(float64(limitBytes) / float64(bodyGateChunk)))
	if n < 1 {
		n = 1
	}
	return &bodyGate{tokens: make(chan struct{}, n)}
}

func (g *bodyGate) acquire(ctx context.Context, bytes int64) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	res := g.newReservation()
	if err := res.grow(ctx, bytes); err != nil {
		res.release()
		return nil, err
	}
	return res.release, nil
}

func (g *bodyGate) newReservation() *bodyReservation {
	return &bodyReservation{gate: g}
}

func (r *bodyReservation) grow(ctx context.Context, bytes int64) error {
	if r == nil || r.gate == nil {
		return nil
	}
	need := int(math.Ceil(float64(bytes) / float64(bodyGateChunk)))
	if need < 1 {
		need = 1
	}
	if need > cap(r.gate.tokens) {
		need = cap(r.gate.tokens)
	}
	for r.acquired < need {
		select {
		case r.gate.tokens <- struct{}{}:
			r.acquired++
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *bodyReservation) release() {
	if r == nil || r.gate == nil || r.released {
		return
	}
	r.released = true
	for r.acquired > 0 {
		<-r.gate.tokens
		r.acquired--
	}
}
