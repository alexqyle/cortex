// Copyright (c) The Thanos Community Authors.
// Licensed under the Apache License 2.0.

package aggregate

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/thanos-io/promql-engine/execution/model"
	"github.com/thanos-io/promql-engine/execution/telemetry"
	"github.com/thanos-io/promql-engine/query"

	"github.com/efficientgo/core/errors"
	prommodel "github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
)

type countValuesOperator struct {
	pool  *model.VectorPool
	next  model.VectorOperator
	param string

	by       bool
	grouping []string

	stepsBatch int
	curStep    int

	ts     []int64
	counts []map[int]int
	series []labels.Labels

	once sync.Once
}

func NewCountValues(pool *model.VectorPool, next model.VectorOperator, param string, by bool, grouping []string, opts *query.Options) model.VectorOperator {
	// Grouping labels need to be sorted in order for metric hashing to work.
	// https://github.com/prometheus/prometheus/blob/8ed39fdab1ead382a354e45ded999eb3610f8d5f/model/labels/labels.go#L162-L181
	slices.Sort(grouping)

	op := &countValuesOperator{
		pool:       pool,
		next:       next,
		param:      param,
		stepsBatch: opts.StepsBatch,
		by:         by,
		grouping:   grouping,
	}
	return telemetry.NewOperator(telemetry.NewTelemetry(op, opts), op)
}

func (c *countValuesOperator) Explain() []model.VectorOperator {
	return []model.VectorOperator{c.next}
}

func (c *countValuesOperator) GetPool() *model.VectorPool {
	return c.pool
}

func (c *countValuesOperator) String() string {
	if c.by {
		return fmt.Sprintf("[countValues] by (%v) - param (%v)", c.grouping, c.param)
	}
	return fmt.Sprintf("[countValues] without (%v) - param (%v)", c.grouping, c.param)
}

func (c *countValuesOperator) Series(ctx context.Context) ([]labels.Labels, error) {
	var err error
	c.once.Do(func() { err = c.initSeriesOnce(ctx) })
	return c.series, err
}

func (c *countValuesOperator) Next(ctx context.Context) ([]model.StepVector, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	var err error
	c.once.Do(func() { err = c.initSeriesOnce(ctx) })
	if err != nil {
		return nil, err
	}

	if c.curStep >= len(c.ts) {
		return nil, nil
	}

	batch := c.pool.GetVectorBatch()
	for range c.stepsBatch {
		if c.curStep >= len(c.ts) {
			break
		}
		sv := c.pool.GetStepVector(c.ts[c.curStep])
		for i, v := range c.counts[c.curStep] {
			sv.AppendSample(c.pool, uint64(i), float64(v))
		}
		batch = append(batch, sv)
		c.curStep++
	}
	return batch, nil
}

func (c *countValuesOperator) initSeriesOnce(ctx context.Context) error {
	if !prommodel.LabelName(c.param).IsValid() {
		return errors.Newf("invalid label name %q", c.param)
	}

	nextSeries, err := c.next.Series(ctx)
	if err != nil {
		return err
	}
	var (
		inputIdToHashBucket = make(map[int]uint64)
		hashToBucketLabels  = make(map[uint64]labels.Labels)
		hashToOutputId      = make(map[uint64]int)

		hashingBuf = make([]byte, 1024)
		builder    labels.ScratchBuilder
		labelsMap  = make(map[string]struct{})
	)
	for _, lblName := range c.grouping {
		labelsMap[lblName] = struct{}{}
	}
	for i := range nextSeries {
		hash, lbls := hashMetric(builder, nextSeries[i], !c.by, c.grouping, labelsMap, hashingBuf)
		inputIdToHashBucket[i] = hash
		if _, ok := hashToBucketLabels[hash]; !ok {
			hashToBucketLabels[hash] = lbls
		}
	}

	ts := make([]int64, 0)
	counts := make([]map[int]int, 0)
	series := make([]labels.Labels, 0)

	b := labels.NewBuilder(labels.EmptyLabels())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		in, err := c.next.Next(ctx)
		if err != nil {
			return err
		}
		if in == nil {
			break
		}
		for i := range in {
			ts = append(ts, in[i].T)
			countPerHashbucket := make(map[uint64]map[string]int, len(inputIdToHashBucket))
			for j := range in[i].Samples {
				hash := inputIdToHashBucket[int(in[i].SampleIDs[j])]
				if _, ok := countPerHashbucket[hash]; !ok {
					countPerHashbucket[hash] = make(map[string]int)
				}
				// Using string as the key to the map so that -0 and 0 are treated as separate values.
				fStr := strconv.FormatFloat(in[i].Samples[j], 'f', -1, 64)
				countPerHashbucket[hash][fStr]++
			}

			for j := range in[i].Histograms {
				hash := inputIdToHashBucket[int(in[i].HistogramIDs[j])]
				if _, ok := countPerHashbucket[hash]; !ok {
					countPerHashbucket[hash] = make(map[string]int)
				}
				// Using string as the key to the map so that -0 and 0 are treated as separate values.
				fStr := in[i].Histograms[j].String()
				countPerHashbucket[hash][fStr]++
			}

			countsPerOutputId := make(map[int]int)
			for hash, counts := range countPerHashbucket {
				b.Reset(hashToBucketLabels[hash])
				for f, count := range counts {
					// TODO: Probably we should issue a warning if we override a label here
					lbls := b.Set(c.param, f).Labels()
					hash := lbls.Hash()
					outputId, ok := hashToOutputId[hash]
					if !ok {
						series = append(series, lbls)
						outputId = len(series) - 1
						hashToOutputId[hash] = outputId
					}
					countsPerOutputId[outputId] += count
				}
			}
			counts = append(counts, countsPerOutputId)
		}
		c.next.GetPool().PutVectors(in)
	}

	c.ts = ts
	c.counts = counts
	c.series = series

	return nil
}
