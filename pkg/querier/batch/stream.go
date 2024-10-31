// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/batch/stream.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package batch

import (
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/zeropool"

	"github.com/grafana/mimir/pkg/storage/chunk"
)

// batchStream deals with iterating through multiple, non-overlapping batches,
// and building new slices of non-overlapping batches.  Designed to be used
// without allocations.
type batchStream struct {
	batches    []chunk.Batch
	batchesBuf []chunk.Batch

	hPool  *zeropool.Pool[*histogram.Histogram]
	fhPool *zeropool.Pool[*histogram.FloatHistogram]
}

func newBatchStream(size int, hPool *zeropool.Pool[*histogram.Histogram], fhPool *zeropool.Pool[*histogram.FloatHistogram]) *batchStream {
	batches := make([]chunk.Batch, 0, size)
	batchesBuf := make([]chunk.Batch, size)
	return &batchStream{
		batches:    batches,
		batchesBuf: batchesBuf,
		hPool:      hPool,
		fhPool:     fhPool,
	}
}

func (bs *batchStream) putPointerValuesToThePool(batch *chunk.Batch) {
	if batch.ValueType == chunkenc.ValHistogram && bs.hPool != nil {
		for i := 0; i < batch.Length; i++ {
			bs.hPool.Put((*histogram.Histogram)(batch.PointerValues[i]))
		}
	} else if batch.ValueType == chunkenc.ValFloatHistogram && bs.fhPool != nil {
		for i := 0; i < batch.Length; i++ {
			bs.fhPool.Put((*histogram.FloatHistogram)(batch.PointerValues[i]))
		}
	}
}

func (bs *batchStream) removeFirst() {
	bs.putPointerValuesToThePool(bs.curr())
	copy(bs.batches, bs.batches[1:])
	bs.batches = bs.batches[:len(bs.batches)-1]
}

func (bs *batchStream) empty() {
	for i := range bs.batches {
		bs.putPointerValuesToThePool(&bs.batches[i])
	}
	bs.batches = bs.batches[:0]
}

func (bs *batchStream) len() int {
	return len(bs.batches)
}

func (bs *batchStream) reset() {
	for i := range bs.batches {
		bs.batches[i].Index = 0
	}
}

func (bs *batchStream) hasNext() chunkenc.ValueType {
	if bs.len() > 0 {
		return bs.curr().ValueType
	}
	return chunkenc.ValNone
}

func (bs *batchStream) next() {
	b := bs.curr()
	b.Index++
	if b.Index >= b.Length {
		bs.batches = bs.batches[1:]
	}
}

func (bs *batchStream) curr() *chunk.Batch {
	return &bs.batches[0]
}

// merge merges this streams of chunk.Batch objects and the given chunk.Batch of the same series over time.
// Samples are simply merged by time when they are the same type (float/histogram/...), with the left stream taking precedence if the timestamps are equal.
// When sample are different type, batches are not merged. In case of equal timestamps, histograms take precedence since they have more information.
func (bs *batchStream) merge(batch *chunk.Batch, size int) {
	// We store this at the beginning to avoid additional allocations.
	// Namely, the merge method will go through all the batches from bs.batch,
	// check whether their elements should be kept (and copy them to the result)
	// or discarded (and put them in the pool in order to reuse them), and then
	// remove the batches from bs.batch.
	// Eventually, at the end of the merge method, the resulting merged batches
	// will be appended to the previously emptied bs.batches. At that point
	// the cap(bs.batches) will be 0, so in order to save some allocations,
	// we will use origBatches, i.e., bs.bathces' capacity from the beginning of
	// the merge method.
	origBatches := bs.batches[:0]

	// Reset the Index and Length of existing batches.
	for i := range bs.batchesBuf {
		bs.batchesBuf[i].Index = 0
		bs.batchesBuf[i].Length = 0
	}

	resultLen := 1 // Number of batches in the final result.
	b := &bs.batchesBuf[0]

	// Step to the next Batch in the result, create it if it does not exist
	nextBatch := func(valueType chunkenc.ValueType) {
		// The Index is the place at which new sample
		// has to be appended, hence it tells the length.
		b.Length = b.Index
		resultLen++
		if resultLen > len(bs.batchesBuf) {
			// It is possible that result can grow longer
			// then the one provided.
			bs.batchesBuf = append(bs.batchesBuf, chunk.Batch{})
		}
		b = &bs.batchesBuf[resultLen-1]
		b.ValueType = valueType
	}

	populate := func(batch *chunk.Batch, valueType chunkenc.ValueType, clearHint bool) {
		if b.Index == 0 {
			// Starting to write this Batch, it is safe to set the value type
			b.ValueType = valueType
		} else if b.Index == size || b.ValueType != valueType {
			// The batch reached its intended size or is of a different value type
			// Add another batch to the result and use it for further appending.
			nextBatch(valueType)
		}

		switch valueType {
		case chunkenc.ValFloat:
			b.Timestamps[b.Index], b.Values[b.Index] = batch.At()
		case chunkenc.ValHistogram:
			b.Timestamps[b.Index], b.PointerValues[b.Index] = batch.AtHistogram()
			if clearHint {
				(*histogram.Histogram)(b.PointerValues[b.Index]).CounterResetHint = histogram.UnknownCounterReset
			}
			b.SetPrevT(batch.PrevT())
		case chunkenc.ValFloatHistogram:
			b.Timestamps[b.Index], b.PointerValues[b.Index] = batch.AtFloatHistogram()
			if clearHint {
				(*histogram.FloatHistogram)(b.PointerValues[b.Index]).CounterResetHint = histogram.UnknownCounterReset
			}
			b.SetPrevT(batch.PrevT())
		}
		b.Index++
	}

	for lt, rt := bs.hasNext(), batch.HasNext(); lt != chunkenc.ValNone && rt != chunkenc.ValNone; lt, rt = bs.hasNext(), batch.HasNext() {
		t1, t2 := bs.curr().AtTime(), batch.AtTime()

		if t1 < t2 {
			// We have a definite next sample in the left stream.
			// Reset the counter hint if the previous sample was unknown.
			populate(bs.curr(), lt, lt != chunkenc.ValFloat && bs.curr().PrevT() == 0)
			bs.next()
			// Trigger the reset of the hint on the right side to indicate that the
			// right side had a discontinuity.
			batch.SetPrevT(0)
			continue
		}
		if t2 < t1 {
			// Reset the counter hint if the previous sample was unknown.
			// We have a definite next sample in the right stream.
			populate(batch, rt, rt != chunkenc.ValFloat && batch.PrevT() == 0)
			batch.Next()
			// Trigger the reset of the hint on the left side to indicate that the
			// left side had a discontinuity.
			bs.curr().SetPrevT(0)
			continue
		}

		// We have samples at the same time.
		// Happy case is that they are of the same type.
		if lt == rt {
			// We need to reset the counter hint if the previous sample was unknown.
			// Do not trigger resetting the next sample hint, as we are at the same
			// time.
			populate(bs.curr(), lt, lt != chunkenc.ValFloat && (bs.curr().PrevT() == 0 || batch.PrevT() == 0))
			// if bs.hPool is not nil, we put there the discarded histogram.Histogram object from batch, so it can be reused.
			if rt == chunkenc.ValHistogram && bs.hPool != nil {
				_, h := batch.AtHistogram()
				bs.hPool.Put((*histogram.Histogram)(h))
			}
			// if bs.fhPool is not nil, we put there the discarded histogram.FloatHistogram object from batch, so it can be reused.
			if rt == chunkenc.ValFloatHistogram && bs.fhPool != nil {
				_, fh := batch.AtFloatHistogram()
				bs.fhPool.Put((*histogram.FloatHistogram)(fh))
			}
			bs.next()
			batch.Next()
			continue
		}

		// The more exotic cases:
		switch {
		case lt == chunkenc.ValFloat:
			// Left side is float, right side is histogram.
			// Take the right side.
			populate(batch, rt, true)
		case rt == chunkenc.ValFloat:
			// Left side is histogram, right side is float.
			// Take the left side.
			populate(bs.curr(), lt, true)
		default: // Both are histograms, take the left side.
			populate(bs.curr(), lt, true)
			// if bs.hPool is not nil, we put there the discarded histogram.Histogram object from batch, so it can be reused.
			if rt == chunkenc.ValHistogram && bs.hPool != nil {
				_, h := batch.AtHistogram()
				bs.hPool.Put((*histogram.Histogram)(h))
			}
			// if bs.fhPool is not nil, we put there the discarded histogram.FloatHistogram object from batch, so it can be reused.
			if rt == chunkenc.ValFloatHistogram && bs.fhPool != nil {
				_, fh := batch.AtFloatHistogram()
				bs.fhPool.Put((*histogram.FloatHistogram)(fh))
			}
		}
		bs.next()
		batch.Next()
	}

	for t := bs.hasNext(); t != chunkenc.ValNone; t = bs.hasNext() {
		populate(bs.curr(), t, bs.curr().PrevT() == 0)
		bs.next()
	}

	for t := batch.HasNext(); t != chunkenc.ValNone; t = batch.HasNext() {
		populate(batch, t, batch.PrevT() == 0)
		batch.Next()
	}

	// The Index is the place at which new sample
	// has to be appended, hence it tells the length.
	b.Length = b.Index

	bs.batches = append(origBatches, bs.batchesBuf[:resultLen]...)
	bs.reset()
}
