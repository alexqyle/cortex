package compactor

import (
	"context"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/oklog/ulid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/stretchr/testify/require"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/thanos/pkg/block/metadata"

	cortex_testutil "github.com/cortexproject/cortex/pkg/storage/tsdb/testutil"
)

func TestMarkBlocksVisited(t *testing.T) {
	ulid0 := ulid.MustNew(0, nil)
	ulid1 := ulid.MustNew(1, nil)
	ulid2 := ulid.MustNew(2, nil)
	now := time.Now().Unix()
	nowBefore1h := time.Now().Add(-1 * time.Hour).Unix()
	partitionedGroupID := uint32(12345)
	for _, tcase := range []struct {
		name        string
		visitMarker BlockVisitMarker
		blocks      []*metadata.Meta
	}{
		{
			name: "write visit marker succeeded",
			visitMarker: BlockVisitMarker{
				CompactorID:        "foo",
				Status:             Pending,
				PartitionedGroupID: partitionedGroupID,
				PartitionID:        0,
				VisitTime:          now,
				Version:            VisitMarkerVersion1,
			},
			blocks: []*metadata.Meta{
				{
					BlockMeta: tsdb.BlockMeta{
						ULID: ulid0,
					},
				},
				{
					BlockMeta: tsdb.BlockMeta{
						ULID: ulid1,
					},
				},
				{
					BlockMeta: tsdb.BlockMeta{
						ULID: ulid2,
					},
				},
			},
		},
		{
			name: "write visit marker succeeded 2",
			visitMarker: BlockVisitMarker{
				CompactorID:        "bar",
				Status:             Completed,
				PartitionedGroupID: partitionedGroupID,
				PartitionID:        1,
				VisitTime:          nowBefore1h,
				Version:            VisitMarkerVersion1,
			},
			blocks: []*metadata.Meta{
				{
					BlockMeta: tsdb.BlockMeta{
						ULID: ulid0,
					},
				},
				{
					BlockMeta: tsdb.BlockMeta{
						ULID: ulid1,
					},
				},
				{
					BlockMeta: tsdb.BlockMeta{
						ULID: ulid2,
					},
				},
			},
		},
	} {
		t.Run(tcase.name, func(t *testing.T) {
			ctx := context.Background()
			dummyCounter := prometheus.NewCounter(prometheus.CounterOpts{})
			bkt, _ := cortex_testutil.PrepareFilesystemBucket(t)
			logger := log.NewNopLogger()
			markBlocksVisited(ctx, bkt, logger, tcase.blocks, tcase.visitMarker, dummyCounter)
			for _, meta := range tcase.blocks {
				res, err := ReadBlockVisitMarker(ctx, objstore.WithNoopInstr(bkt), logger, meta.ULID.String(), tcase.visitMarker.PartitionID, dummyCounter)
				require.NoError(t, err)
				require.Equal(t, tcase.visitMarker, *res)
			}
		})
		t.Run(tcase.name, func(t *testing.T) {
			ctx := context.Background()
			dummyCounter := prometheus.NewCounter(prometheus.CounterOpts{})
			bkt, _ := cortex_testutil.PrepareFilesystemBucket(t)
			logger := log.NewNopLogger()
			markBlocksVisitMarkerCompleted(ctx, bkt, logger, tcase.blocks, tcase.visitMarker.PartitionedGroupID, tcase.visitMarker.PartitionID, tcase.visitMarker.CompactorID, dummyCounter)
			for _, meta := range tcase.blocks {
				res, err := ReadBlockVisitMarker(ctx, objstore.WithNoopInstr(bkt), logger, meta.ULID.String(), tcase.visitMarker.PartitionID, dummyCounter)
				require.NoError(t, err)
				require.True(t, res.isCompleted())
				require.Equal(t, tcase.visitMarker.CompactorID, res.CompactorID)
				require.Equal(t, tcase.visitMarker.PartitionedGroupID, res.PartitionedGroupID)
				require.Equal(t, tcase.visitMarker.PartitionID, res.PartitionID)
				require.Equal(t, tcase.visitMarker.Version, res.Version)
			}
		})
	}
}
