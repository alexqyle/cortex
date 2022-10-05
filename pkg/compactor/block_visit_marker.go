package compactor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/objstore/providers/s3"
	"github.com/thanos-io/thanos/pkg/block/metadata"

	"github.com/cortexproject/cortex/pkg/util/runutil"
)

const (
	// BlockVisitMarkerFileSuffix is the known suffix of json filename for representing the most recent compactor visit.
	BlockVisitMarkerFileSuffix = "-visit-mark.json"
	// BlockVisitMarkerFilePrefix is the known prefix of json filename for representing the most recent compactor visit.
	BlockVisitMarkerFilePrefix = "partition-"
	// VisitMarkerVersion1 is the current supported version of visit-mark file.
	VisitMarkerVersion1    = 1
	CompactionErrorChanKey = "compaction-error-chan"
)

var (
	ErrorBlockVisitMarkerNotFound  = errors.New("block visit marker not found")
	ErrorUnmarshalBlockVisitMarker = errors.New("unmarshal block visit marker JSON")
)

type VisitStatus string

const (
	Pending   VisitStatus = "pending"
	Completed VisitStatus = "completed"
)

type BlockVisitMarker struct {
	CompactorID        string      `json:"compactorID"`
	Status             VisitStatus `json:"status"`
	PartitionedGroupID uint32      `json:"partitionedGroupID"`
	PartitionID        int         `json:"partitionID"`
	// VisitTime is a unix timestamp of when the block was visited (mark updated).
	VisitTime int64 `json:"visitTime"`
	// Version of the file.
	Version int `json:"version"`
}

func (b *BlockVisitMarker) isVisited(blockVisitMarkerTimeout time.Duration, partitionID int) bool {
	return partitionID == b.PartitionID && time.Now().Before(time.Unix(b.VisitTime, 0).Add(blockVisitMarkerTimeout))
}

func (b *BlockVisitMarker) isVisitedByCompactor(blockVisitMarkerTimeout time.Duration, partitionID int, compactorID string) bool {
	return b.CompactorID == compactorID && b.isVisited(blockVisitMarkerTimeout, partitionID)
}

func (b *BlockVisitMarker) isCompleted() bool {
	return b.Status == Completed
}

func getBlockVisitMarkerFile(blockID string, partitionID int) string {
	return path.Join(blockID, fmt.Sprintf("%s%d%s", BlockVisitMarkerFilePrefix, partitionID, BlockVisitMarkerFileSuffix))
}

func getPartitionIDFromVisitMarkerFileName(filePath string) (int, error) {
	delimIndex := strings.LastIndex(filePath, s3.DirDelim)
	partitionIDStr := strings.TrimSuffix(strings.TrimPrefix(filePath[delimIndex+1:], BlockVisitMarkerFilePrefix), BlockVisitMarkerFileSuffix)
	return strconv.Atoi(partitionIDStr)
}

func ReadBlockVisitMarker(ctx context.Context, bkt objstore.InstrumentedBucketReader, logger log.Logger, blockID string, partitionID int, blockVisitMarkerReadFailed prometheus.Counter) (*BlockVisitMarker, error) {
	visitMarkerFile := getBlockVisitMarkerFile(blockID, partitionID)
	return readBlockVisitMarker(ctx, bkt, logger, visitMarkerFile, blockVisitMarkerReadFailed)
}

func readBlockVisitMarker(ctx context.Context, bkt objstore.InstrumentedBucketReader, logger log.Logger, visitMarkerFile string, blockVisitMarkerReadFailed prometheus.Counter) (*BlockVisitMarker, error) {
	visitMarkerFileReader, err := bkt.ReaderWithExpectedErrs(bkt.IsObjNotFoundErr).Get(ctx, visitMarkerFile)
	if err != nil {
		if bkt.IsObjNotFoundErr(err) {
			return nil, errors.Wrapf(ErrorBlockVisitMarkerNotFound, "block visit marker file: %s", visitMarkerFile)
		}
		blockVisitMarkerReadFailed.Inc()
		return nil, errors.Wrapf(err, "get block visit marker file: %s", visitMarkerFile)
	}
	defer runutil.CloseWithLogOnErr(logger, visitMarkerFileReader, "close block visit marker reader")
	b, err := io.ReadAll(visitMarkerFileReader)
	if err != nil {
		blockVisitMarkerReadFailed.Inc()
		return nil, errors.Wrapf(err, "read block visit marker file: %s", visitMarkerFile)
	}
	blockVisitMarker := BlockVisitMarker{}
	if err = json.Unmarshal(b, &blockVisitMarker); err != nil {
		blockVisitMarkerReadFailed.Inc()
		return nil, errors.Wrapf(ErrorUnmarshalBlockVisitMarker, "block visit marker file: %s, error: %v", visitMarkerFile, err.Error())
	}
	if blockVisitMarker.Version != VisitMarkerVersion1 {
		return nil, errors.Errorf("unexpected block visit mark file version %d, expected %d", blockVisitMarker.Version, VisitMarkerVersion1)
	}
	return &blockVisitMarker, nil
}

func ReadAllBlockVisitMarker(ctx context.Context, bkt objstore.InstrumentedBucketReader, logger log.Logger, blockID string, blockVisitMarkerReadFailed prometheus.Counter) ([]BlockVisitMarker, error) {
	var blockVisitMarkers []BlockVisitMarker
	err := bkt.Iter(ctx, blockID, func(filePath string) error {
		if strings.HasSuffix(filePath, BlockVisitMarkerFileSuffix) {
			if blockVisitMarker, err := readBlockVisitMarker(ctx, bkt, logger, filePath, blockVisitMarkerReadFailed); err != nil {
				return err
			} else {
				blockVisitMarkers = append(blockVisitMarkers, *blockVisitMarker)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return blockVisitMarkers, nil
}

func UpdateBlockVisitMarker(ctx context.Context, bkt objstore.Bucket, blockID string, partitionID int, reader io.Reader, blockVisitMarkerWriteFailed prometheus.Counter) error {
	blockVisitMarkerFilePath := getBlockVisitMarkerFile(blockID, partitionID)
	if err := bkt.Upload(ctx, blockVisitMarkerFilePath, reader); err != nil {
		blockVisitMarkerWriteFailed.Inc()
		return err
	}
	return nil
}

func markBlocksVisited(
	ctx context.Context,
	bkt objstore.Bucket,
	logger log.Logger,
	blocks []*metadata.Meta,
	marker BlockVisitMarker,
	blockVisitMarkerWriteFailed prometheus.Counter,
) {
	visitMarkerFileContent, err := json.Marshal(marker)
	if err != nil {
		blockVisitMarkerWriteFailed.Inc()
		return
	}
	reader := bytes.NewReader(visitMarkerFileContent)
	for _, block := range blocks {
		blockID := block.ULID.String()
		if err := UpdateBlockVisitMarker(ctx, bkt, blockID, marker.PartitionID, reader, blockVisitMarkerWriteFailed); err != nil {
			level.Error(logger).Log("msg", "unable to upsert visit marker file content for block", "blockID", blockID, "partitionID", marker.PartitionID, "err", err)
		}
		reader.Reset(visitMarkerFileContent)
	}
}

func markBlocksVisitedHeartBeat(
	ctx context.Context,
	bkt objstore.Bucket,
	logger log.Logger,
	blocks []*metadata.Meta,
	partitionedGroupID uint32,
	partitionID int,
	compactorID string,
	blockVisitMarkerFileUpdateInterval time.Duration,
	blockVisitMarkerWriteFailed prometheus.Counter,
) {
	compactErrChan := ctx.Value(CompactionErrorChanKey).(chan error)
	var blockIds []string
	for _, block := range blocks {
		blockIds = append(blockIds, block.ULID.String())
	}
	blocksInfo := strings.Join(blockIds, ",")
	level.Info(logger).Log("msg", fmt.Sprintf("start heart beat for partition: %d, blocks: %s", partitionID, blocksInfo))
	ticker := time.NewTicker(blockVisitMarkerFileUpdateInterval)
	defer ticker.Stop()
heartBeat:
	for {
		level.Debug(logger).Log("msg", fmt.Sprintf("heart beat for partition: %d, blocks: %s", partitionID, blocksInfo))
		blockVisitMarker := BlockVisitMarker{
			VisitTime:          time.Now().Unix(),
			CompactorID:        compactorID,
			Status:             Pending,
			PartitionedGroupID: partitionedGroupID,
			PartitionID:        partitionID,
			Version:            VisitMarkerVersion1,
		}
		markBlocksVisited(ctx, bkt, logger, blocks, blockVisitMarker, blockVisitMarkerWriteFailed)

		select {
		case <-ctx.Done():
			markBlocksVisitMarkerCompleted(ctx, bkt, logger, blocks, partitionID, compactorID, blockVisitMarkerWriteFailed)
			break heartBeat
		case <-ticker.C:
			continue
		case err := <-compactErrChan:
			if err != nil {
				level.Debug(logger).Log("msg", "stop heart beat due to error", "err", err)
				break heartBeat
			}
			continue
		}
	}
	level.Info(logger).Log("msg", fmt.Sprintf("stop heart beat for partition: %d, blocks: %s", partitionID, blocksInfo))
}

func markBlocksVisitMarkerCompleted(
	ctx context.Context,
	bkt objstore.Bucket,
	logger log.Logger,
	blocks []*metadata.Meta,
	partitionID int,
	compactorID string,
	blockVisitMarkerWriteFailed prometheus.Counter,
) {
	blockVisitMarker := BlockVisitMarker{
		VisitTime:   time.Now().Unix(),
		CompactorID: compactorID,
		Status:      Completed,
		PartitionID: partitionID,
		Version:     VisitMarkerVersion1,
	}
	visitMarkerFileContent, err := json.Marshal(blockVisitMarker)
	if err != nil {
		blockVisitMarkerWriteFailed.Inc()
		return
	}
	reader := bytes.NewReader(visitMarkerFileContent)
	for _, block := range blocks {
		blockID := block.ULID.String()
		if err := UpdateBlockVisitMarker(ctx, bkt, blockID, blockVisitMarker.PartitionID, reader, blockVisitMarkerWriteFailed); err != nil {
			level.Error(logger).Log("msg", "unable to upsert completed visit marker file content for block", "blockID", blockID, "partitionID", blockVisitMarker.PartitionID, "err", err)
		}
		reader.Reset(visitMarkerFileContent)
	}
}
