// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing pemissions and
// limitations under the License.

package redo

import (
	"context"
	"math"
	"net/url"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/ticdc/cdc/redo/writer"
	"github.com/pingcap/ticdc/pkg/config"
	cerror "github.com/pingcap/ticdc/pkg/errors"
	"github.com/pingcap/ticdc/pkg/util"
	"go.uber.org/zap"
)

var updateRtsInterval = time.Second

type consistentLevelType string

const (
	consistentLevelNormal   consistentLevelType = "normal"
	consistentLevelEventual consistentLevelType = "eventual"
)

type consistentStorage string

const (
	consistentStorageLocal     consistentStorage = "local"
	consistentStorageS3        consistentStorage = "s3"
	consistentStorageBlackhole consistentStorage = "blackhole"
)

// IsValidConsistentLevel checks whether a give consistent level is valid
func IsValidConsistentLevel(level string) bool {
	switch consistentLevelType(level) {
	case consistentLevelNormal, consistentLevelEventual:
		return true
	default:
		return false
	}
}

// IsValidConsistentStorage checks whether a give consistent storage is valid
func IsValidConsistentStorage(storage string) bool {
	switch consistentStorage(storage) {
	case consistentStorageLocal, consistentStorageS3, consistentStorageBlackhole:
		return true
	default:
		return false
	}
}

// LogManager defines an interface that is used to manage redo log
type LogManager interface {
	// Enabled returns whether the log manager is enabled
	Enabled() bool

	// The following 5 APIs are called from processor only
	EmitRowChangedEvents(ctx context.Context, tableID model.TableID, rows ...*model.RowChangedEvent) error
	FlushLog(ctx context.Context, tableID model.TableID, resolvedTs uint64) error
	AddTable(tableID model.TableID, startTs uint64)
	RemoveTable(tableID model.TableID)
	GetMinResolvedTs() uint64

	// EmitDDLEvent and FlushResolvedAndCheckpointTs are called from owner only
	EmitDDLEvent(ctx context.Context, ddl *model.DDLEvent) error
	FlushResolvedAndCheckpointTs(ctx context.Context, resolvedTs, checkpointTs uint64) (err error)
}

// ManagerOptions defines options for redo log manager
type ManagerOptions struct {
	// whether to run background goroutine to fetch table resolved ts
	EnableBgRunner bool
	ErrCh          chan<- error
}

// ManagerImpl manages redo log writer, buffers un-persistent redo logs, calculates
// redo log resolved ts. It implements LogManager interface.
type ManagerImpl struct {
	enabled       bool
	level         consistentLevelType
	storage       consistentStorage
	writer        writer.RedoLogWriter
	minResolvedTs uint64
	tableIDs      []model.TableID
	rtsMap        map[model.TableID]uint64
	rtsMapMu      sync.RWMutex
}

// NewManager creates a new Manager
func NewManager(ctx context.Context, cfg *config.ConsistentConfig, opts *ManagerOptions) (*ManagerImpl, error) {
	// return a nil Manager if no consistent config or normal consistent level
	if cfg == nil || consistentLevelType(cfg.Level) == consistentLevelNormal {
		return &ManagerImpl{enabled: false}, nil
	}
	m := &ManagerImpl{
		enabled: true,
		level:   consistentLevelType(cfg.Level),
		storage: consistentStorage(cfg.Storage),
		rtsMap:  make(map[model.TableID]uint64),
	}
	switch m.storage {
	case consistentStorageBlackhole:
		m.writer = writer.NewBlackHoleWriter()
	case consistentStorageLocal, consistentStorageS3:
		globalConf := config.GetGlobalServerConfig()
		redoDir := filepath.Join(globalConf.DataDir, config.DefaultRedoDir)
		writerCfg := &writer.LogWriterConfig{
			Dir:               redoDir,
			CaptureID:         util.CaptureAddrFromCtx(ctx),
			ChangeFeedID:      util.ChangefeedIDFromCtx(ctx),
			CreateTime:        time.Now(),
			MaxLogSize:        cfg.MaxLogSize,
			FlushIntervalInMs: cfg.FlushIntervalInMs,
			S3Storage:         cfg.Storage == string(consistentStorageS3),
		}
		if writerCfg.S3Storage {
			s3URI, err := url.Parse(cfg.S3URI)
			if err != nil {
				return nil, cerror.WrapError(cerror.ErrInvalidS3URI, err)
			}
			writerCfg.S3URI = s3URI
		}
		m.writer = writer.NewLogWriter(ctx, writerCfg)
	default:
		return nil, cerror.ErrConsistentStorage.GenWithStackByArgs(m.storage)
	}
	if opts.EnableBgRunner {
		go m.run(ctx, opts.ErrCh)
	}
	return m, nil
}

// NewDisabledManager returns a disabled log manger instance, used in test only
func NewDisabledManager() *ManagerImpl {
	return &ManagerImpl{enabled: false}
}

// Enabled returns whether this log manager is enabled
func (m *ManagerImpl) Enabled() bool {
	return m.enabled
}

// EmitRowChangedEvents converts RowChangedEvents to RedoLogs, and sends to redo log writer
func (m *ManagerImpl) EmitRowChangedEvents(
	ctx context.Context,
	tableID model.TableID,
	rows ...*model.RowChangedEvent,
) error {
	logs := make([]*model.RedoRowChangedEvent, 0, len(rows))
	for _, row := range rows {
		logs = append(logs, RowToRedo(row))
	}
	_, err := m.writer.WriteLog(ctx, tableID, logs)
	return err
}

// FlushLog emits resolved ts of a single table
func (m *ManagerImpl) FlushLog(
	ctx context.Context,
	tableID model.TableID,
	resolvedTs uint64,
) error {
	return m.writer.FlushLog(ctx, tableID, resolvedTs)
}

// EmitDDLEvent sends DDL event to redo log writer
func (m *ManagerImpl) EmitDDLEvent(ctx context.Context, ddl *model.DDLEvent) error {
	return m.writer.SendDDL(ctx, DDLToRedo(ddl))
}

// GetMinResolvedTs returns the minimum resolved ts of all tables in this redo log manager
func (m *ManagerImpl) GetMinResolvedTs() uint64 {
	return atomic.LoadUint64(&m.minResolvedTs)
}

// FlushResolvedAndCheckpointTs flushes resolved-ts and checkpoint-ts to redo log writer
func (m *ManagerImpl) FlushResolvedAndCheckpointTs(ctx context.Context, resolvedTs, checkpointTs uint64) (err error) {
	err = m.writer.EmitResolvedTs(ctx, resolvedTs)
	if err != nil {
		return
	}
	err = m.writer.EmitCheckpointTs(ctx, checkpointTs)
	return
}

// AddTable adds a new table in redo log manager
func (m *ManagerImpl) AddTable(tableID model.TableID, startTs uint64) {
	m.rtsMapMu.Lock()
	defer m.rtsMapMu.Unlock()
	i := sort.Search(len(m.tableIDs), func(i int) bool {
		return m.tableIDs[i] >= tableID
	})
	if i < len(m.tableIDs) && m.tableIDs[i] == tableID {
		log.Warn("add duplicated table in redo log manager", zap.Int64("table-id", tableID))
		return
	}
	if i == len(m.tableIDs) {
		m.tableIDs = append(m.tableIDs, tableID)
	} else {
		m.tableIDs = append(m.tableIDs[:i+1], m.tableIDs[i:]...)
		m.tableIDs[i] = tableID
	}
	m.rtsMap[tableID] = startTs
}

// RemoveTable removes a table from redo log manager
func (m *ManagerImpl) RemoveTable(tableID model.TableID) {
	m.rtsMapMu.Lock()
	defer m.rtsMapMu.Unlock()
	i := sort.Search(len(m.tableIDs), func(i int) bool {
		return m.tableIDs[i] >= tableID
	})
	if i < len(m.tableIDs) && m.tableIDs[i] == tableID {
		copy(m.tableIDs[i:], m.tableIDs[i+1:])
		m.tableIDs = m.tableIDs[:len(m.tableIDs)-1]
		delete(m.rtsMap, tableID)
	} else {
		log.Warn("remove a table not maintained in redo log manager", zap.Int64("table-id", tableID))
	}
	// TODO: send remove table command to redo log writer
}

// updatertsMap reads rtsMap from redo log writer and calculate the minimum
// resolved ts of all maintaining tables.
func (m *ManagerImpl) updateTableResolvedTs(ctx context.Context) error {
	m.rtsMapMu.Lock()
	defer m.rtsMapMu.Unlock()
	rtsMap, err := m.writer.GetCurrentResolvedTs(ctx, m.tableIDs)
	if err != nil {
		return err
	}
	minResolvedTs := uint64(math.MaxUint64)
	for tableID, rts := range rtsMap {
		m.rtsMap[tableID] = rts
		if rts < minResolvedTs {
			minResolvedTs = rts
		}
	}
	atomic.StoreUint64(&m.minResolvedTs, minResolvedTs)
	return nil
}

func (m *ManagerImpl) run(ctx context.Context, errCh chan<- error) {
	ticker := time.NewTicker(updateRtsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := m.updateTableResolvedTs(ctx)
			if err != nil {
				errCh <- err
				return
			}
		}
	}
}