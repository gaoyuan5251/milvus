// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package querynode

/*
#cgo pkg-config: milvus_segcore

#include "segcore/collection_c.h"
#include "segcore/plan_c.h"
#include "segcore/reduce_c.h"
*/
import "C"
import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"unsafe"

	"github.com/milvus-io/milvus/internal/util/funcutil"
	"github.com/milvus-io/milvus/internal/util/typeutil"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/milvus-io/milvus/internal/metrics"
	"github.com/milvus-io/milvus/internal/util/timerecord"

	"github.com/golang/protobuf/proto"
	"go.uber.org/atomic"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/common"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/proto/segcorepb"
	"github.com/milvus-io/milvus/internal/storage"
)

type segmentType = commonpb.SegmentState

const (
	segmentTypeGrowing = commonpb.SegmentState_Growing
	segmentTypeSealed  = commonpb.SegmentState_Sealed
)

var (
	ErrSegmentUnhealthy = errors.New("segment unhealthy")
)

// IndexedFieldInfo contains binlog info of vector field
type IndexedFieldInfo struct {
	fieldBinlog *datapb.FieldBinlog
	indexInfo   *querypb.FieldIndexInfo
}

type DeleteRecord struct {
	pk        primaryKey
	timestamp Timestamp
}

type ByTimestamp []DeleteRecord

func (a ByTimestamp) Len() int      { return len(a) }
func (a ByTimestamp) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByTimestamp) Less(i, j int) bool {
	return a[i].timestamp < a[j].timestamp
}

type DeleteRecords struct {
	mut     sync.Mutex
	records []DeleteRecord
	flushed bool
}

// TryAppend appends the delete records to the buffer, and returns true
// if the buffer is flushed, it will do nothing, and returns false.
func (r *DeleteRecords) TryAppend(pks []primaryKey, timestamps []Timestamp) bool {
	r.mut.Lock()
	defer r.mut.Unlock()

	if r.flushed {
		return false
	}

	for i := range pks {
		r.records = append(r.records, DeleteRecord{pks[i], timestamps[i]})
	}
	return true
}

func (r *DeleteRecords) Flush(handler func([]primaryKey, []Timestamp) error) error {
	r.mut.Lock()
	defer r.mut.Unlock()

	sort.Sort(ByTimestamp(r.records))
	pks := make([]primaryKey, len(r.records))
	timestamps := make([]Timestamp, len(r.records))
	for i := range r.records {
		pks[i] = r.records[i].pk
		timestamps[i] = r.records[i].timestamp
	}

	err := handler(pks, timestamps)
	if err != nil {
		return err
	}

	r.records = nil
	r.flushed = true

	return nil
}

// Segment is a wrapper of the underlying C-structure segment.
type Segment struct {
	mut        sync.RWMutex // protects segmentPtr
	segmentPtr C.CSegmentInterface

	segmentID     UniqueID
	partitionID   UniqueID
	collectionID  UniqueID
	version       UniqueID
	startPosition *internalpb.MsgPosition // for growing segment release

	vChannelID              Channel
	lastMemSize             int64
	lastRowCount            int64
	deleteBuffer            DeleteRecords
	flushedDeletedTimestamp Timestamp

	recentlyModified  *atomic.Bool
	segmentType       *atomic.Int32
	destroyed         *atomic.Bool
	lazyLoading       *atomic.Bool
	indexedFieldInfos *typeutil.ConcurrentMap[UniqueID, *IndexedFieldInfo]

	statLock sync.Mutex
	// only used by sealed segments
	currentStat  *storage.PkStatistics
	historyStats []*storage.PkStatistics
}

// ID returns the identity number.
func (s *Segment) ID() UniqueID {
	return s.segmentID
}

func (s *Segment) setRecentlyModified(modify bool) {
	s.recentlyModified.Store(modify)
}

func (s *Segment) getRecentlyModified() bool {
	return s.recentlyModified.Load()
}

func (s *Segment) setType(segType segmentType) {
	s.segmentType.Store(int32(segType))
}

func (s *Segment) getType() segmentType {
	return commonpb.SegmentState(s.segmentType.Load())
}

func (s *Segment) setIndexedFieldInfo(fieldID UniqueID, info *IndexedFieldInfo) {
	s.indexedFieldInfos.InsertIfNotPresent(fieldID, info)
}

func (s *Segment) getIndexedFieldInfo(fieldID UniqueID) (*IndexedFieldInfo, error) {
	info, ok := s.indexedFieldInfos.Get(fieldID)
	if !ok {
		return nil, fmt.Errorf("Invalid fieldID %d", fieldID)
	}
	return &IndexedFieldInfo{
		fieldBinlog: info.fieldBinlog,
		indexInfo:   info.indexInfo,
	}, nil
}

func (s *Segment) hasLoadIndexForIndexedField(fieldID int64) bool {
	fieldInfo, ok := s.indexedFieldInfos.Get(fieldID)
	if !ok {
		return false
	}
	return fieldInfo.indexInfo != nil && fieldInfo.indexInfo.EnableIndex
}

// healthy checks whether it's safe to use `segmentPtr`.
// shall acquire mut.RLock before check this flag.
func (s *Segment) healthy() bool {
	return !s.destroyed.Load()
}

func (s *Segment) setUnhealthy() {
	s.destroyed.Store(true)
}

func (s *Segment) hasRawData(fieldID int64) bool {
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return false
	}
	var ret bool
	GetDynamicPool().Submit(func() (any, error) {
		ret = bool(C.HasRawData(s.segmentPtr, C.int64_t(fieldID)))
		return struct{}{}, nil
	}).Await()
	return ret
}

func newSegment(collection *Collection,
	segmentID UniqueID,
	partitionID UniqueID,
	collectionID UniqueID,
	vChannelID Channel,
	segType segmentType,
	version UniqueID,
	startPosition *internalpb.MsgPosition,
) (*Segment, error) {
	/*
		CSegmentInterface
		NewSegment(CCollection collection, uint64_t segment_id, SegmentType seg_type);
	*/
	var segmentPtr C.CSegmentInterface
	var err error

	GetDynamicPool().Submit(func() (any, error) {
		switch segType {
		case segmentTypeSealed:
			segmentPtr = C.NewSegment(collection.collectionPtr, C.Sealed, C.int64_t(segmentID))
		case segmentTypeGrowing:
			segmentPtr = C.NewSegment(collection.collectionPtr, C.Growing, C.int64_t(segmentID))
		default:
			err = fmt.Errorf("illegal segment type %d when create segment  %d", segType, segmentID)
			log.Warn("create new segment error",
				zap.Int64("collectionID", collectionID),
				zap.Int64("partitionID", partitionID),
				zap.Int64("segmentID", segmentID),
				zap.String("segmentType", segType.String()),
				zap.Error(err))
		}
		return struct{}{}, err
	}).Await()
	if err != nil {
		return nil, err
	}

	log.Info("create segment",
		zap.Int64("collectionID", collectionID),
		zap.Int64("partitionID", partitionID),
		zap.Int64("segmentID", segmentID),
		zap.String("segmentType", segType.String()),
		zap.String("vchannel", vChannelID),
	)

	var segment = &Segment{
		segmentPtr:        segmentPtr,
		segmentType:       atomic.NewInt32(int32(segType)),
		segmentID:         segmentID,
		partitionID:       partitionID,
		collectionID:      collectionID,
		version:           version,
		startPosition:     startPosition,
		vChannelID:        vChannelID,
		indexedFieldInfos: typeutil.NewConcurrentMap[int64, *IndexedFieldInfo](),
		recentlyModified:  atomic.NewBool(false),
		destroyed:         atomic.NewBool(false),
		lazyLoading:       atomic.NewBool(false),
		historyStats:      []*storage.PkStatistics{},
	}

	return segment, nil
}

func deleteSegment(segment *Segment) {
	/*
		void
		deleteSegment(CSegmentInterface segment);
	*/
	var cPtr C.CSegmentInterface
	// wait all read ops finished
	segment.mut.Lock()
	segment.setUnhealthy()
	cPtr = segment.segmentPtr
	segment.segmentPtr = nil
	segment.mut.Unlock()

	if cPtr == nil {
		return
	}

	GetDynamicPool().Submit(func() (any, error) {
		C.DeleteSegment(cPtr)
		return struct{}{}, nil
	}).Await()

	segment.currentStat = nil
	segment.historyStats = nil

	log.Info("delete segment from memory",
		zap.Int64("collectionID", segment.collectionID),
		zap.Int64("partitionID", segment.partitionID),
		zap.Int64("segmentID", segment.ID()),
		zap.String("segmentType", segment.getType().String()))
}

func (s *Segment) getRealCount() int64 {
	/*
		int64_t
		GetRealCount(CSegmentInterface c_segment);
	*/
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return -1
	}

	var rowCount int64
	GetDynamicPool().Submit(func() (any, error) {
		rowCount = int64(C.GetRealCount(s.segmentPtr))
		return struct{}{}, nil
	}).Await()

	return rowCount
}

func (s *Segment) getRowCount() int64 {
	/*
		long int
		getRowCount(CSegmentInterface c_segment);
	*/
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return -1
	}

	var rowCount int64
	GetDynamicPool().Submit(func() (any, error) {
		rowCount = int64(C.GetRowCount(s.segmentPtr))
		return struct{}{}, nil
	}).Await()

	return rowCount
}

func (s *Segment) getDeletedCount() int64 {
	/*
		long int
		getDeletedCount(CSegmentInterface c_segment);
	*/
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return -1
	}

	var deletedCount int64
	GetDynamicPool().Submit(func() (any, error) {
		deletedCount = int64(C.GetDeletedCount(s.segmentPtr))
		return struct{}{}, nil
	}).Await()

	return deletedCount
}

func (s *Segment) getMemSize() int64 {
	/*
		long int
		GetMemoryUsageInBytes(CSegmentInterface c_segment);
	*/
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return -1
	}

	var memoryUsageInBytes int64
	GetDynamicPool().Submit(func() (any, error) {
		memoryUsageInBytes = int64(C.GetMemoryUsageInBytes(s.segmentPtr))
		return struct{}{}, nil
	}).Await()

	return memoryUsageInBytes
}

func (s *Segment) search(ctx context.Context, searchReq *searchRequest) (*SearchResult, error) {
	/*
		CStatus
		Search(void* plan,
			void* placeholder_groups,
			uint64_t* timestamps,
			int num_groups,
			long int* result_ids,
			float* result_distances);
	*/
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return nil, fmt.Errorf("%w(segmentID=%d)", ErrSegmentUnhealthy, s.segmentID)
	}

	if searchReq.plan == nil {
		return nil, fmt.Errorf("nil search plan")
	}

	loadIndex := s.hasLoadIndexForIndexedField(searchReq.searchFieldID)
	var searchResult SearchResult
	log.Ctx(ctx).Debug("start do search on segment",
		zap.Int64("msgID", searchReq.msgID),
		zap.Int64("segmentID", s.segmentID),
		zap.String("segmentType", s.segmentType.String()),
		zap.Bool("loadIndex", loadIndex))

	var status C.CStatus
	GetSQPool().Submit(func() (any, error) {
		tr := timerecord.NewTimeRecorder("cgoSearch")
		status = C.Search(s.segmentPtr, searchReq.plan.cSearchPlan, searchReq.cPlaceholderGroup,
			C.uint64_t(searchReq.timestamp), &searchResult.cSearchResult)
		metrics.QueryNodeSQSegmentLatencyInCore.WithLabelValues(fmt.Sprint(Params.QueryNodeCfg.GetNodeID()), metrics.SearchLabel).Observe(float64(tr.ElapseSpan().Milliseconds()))
		return struct{}{}, nil
	}).Await()

	if err := HandleCStatus(&status, "Search failed"); err != nil {
		return nil, err
	}
	log.Ctx(ctx).Debug("do search on segment done",
		zap.Int64("msgID", searchReq.msgID),
		zap.Int64("segmentID", s.segmentID),
		zap.String("segmentType", s.segmentType.String()),
		zap.Bool("loadIndex", loadIndex))
	return &searchResult, nil
}

func (s *Segment) retrieve(plan *RetrievePlan) (*segcorepb.RetrieveResults, error) {
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return nil, fmt.Errorf("%w(segmentID=%d)", ErrSegmentUnhealthy, s.segmentID)
	}

	var retrieveResult RetrieveResult
	ts := C.uint64_t(plan.Timestamp)

	var status C.CStatus
	GetSQPool().Submit(func() (any, error) {
		tr := timerecord.NewTimeRecorder("cgoRetrieve")
		maxLimitSize := Params.QuotaConfig.MaxOutputSize
		status = C.Retrieve(s.segmentPtr, plan.cRetrievePlan, ts, &retrieveResult.cRetrieveResult, C.int64_t(maxLimitSize))
		metrics.QueryNodeSQSegmentLatencyInCore.WithLabelValues(fmt.Sprint(Params.QueryNodeCfg.GetNodeID()),
			metrics.QueryLabel).Observe(float64(tr.ElapseSpan().Milliseconds()))
		return struct{}{}, nil
	}).Await()

	log.Debug("do retrieve on segment",
		zap.Int64("msgID", plan.msgID),
		zap.Int64("segmentID", s.segmentID), zap.String("segmentType", s.segmentType.String()))

	if err := HandleCStatus(&status, "Retrieve failed"); err != nil {
		return nil, err
	}
	result := new(segcorepb.RetrieveResults)
	if err := HandleCProto(&retrieveResult.cRetrieveResult, result); err != nil {
		return nil, err
	}

	sort.Sort(&byPK{result})
	return result, nil
}

func (s *Segment) getFieldDataPath(indexedFieldInfo *IndexedFieldInfo, offset int64) (dataPath string, offsetInBinlog int64) {
	offsetInBinlog = offset
	for _, binlog := range indexedFieldInfo.fieldBinlog.Binlogs {
		if offsetInBinlog < binlog.EntriesNum {
			dataPath = binlog.GetLogPath()
			break
		} else {
			offsetInBinlog -= binlog.EntriesNum
		}
	}
	return dataPath, offsetInBinlog
}

func fillBinVecFieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	dim := fieldData.GetVectors().GetDim()
	rowBytes := dim / 8
	content, err := vcm.ReadAt(ctx, dataPath, offset*rowBytes, rowBytes)
	if err != nil {
		return err
	}
	x := fieldData.GetVectors().GetData().(*schemapb.VectorField_BinaryVector)
	resultLen := dim / 8
	copy(x.BinaryVector[i*int(resultLen):(i+1)*int(resultLen)], content)
	return nil
}

func fillFloatVecFieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	dim := fieldData.GetVectors().GetDim()
	rowBytes := dim * 4
	content, err := vcm.ReadAt(ctx, dataPath, offset*rowBytes, rowBytes)
	if err != nil {
		return err
	}
	x := fieldData.GetVectors().GetData().(*schemapb.VectorField_FloatVector)
	floatResult := make([]float32, dim)
	buf := bytes.NewReader(content)
	if err = binary.Read(buf, endian, &floatResult); err != nil {
		return err
	}
	resultLen := dim
	copy(x.FloatVector.Data[i*int(resultLen):(i+1)*int(resultLen)], floatResult)
	return nil
}

func fillBoolFieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	// read whole file.
	// TODO: optimize here.
	content, err := vcm.Read(ctx, dataPath)
	if err != nil {
		return err
	}
	var arr schemapb.BoolArray
	err = proto.Unmarshal(content, &arr)
	if err != nil {
		return err
	}
	fieldData.GetScalars().GetBoolData().GetData()[i] = arr.Data[offset]
	return nil
}

func fillStringFieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	// read whole file.
	// TODO: optimize here.
	content, err := vcm.Read(ctx, dataPath)
	if err != nil {
		return err
	}
	var arr schemapb.StringArray
	err = proto.Unmarshal(content, &arr)
	if err != nil {
		return err
	}
	fieldData.GetScalars().GetStringData().GetData()[i] = arr.Data[offset]
	return nil
}

func fillInt8FieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	// read by offset.
	rowBytes := int64(1)
	content, err := vcm.ReadAt(ctx, dataPath, offset*rowBytes, rowBytes)
	if err != nil {
		return err
	}
	var i8 int8
	if err := funcutil.ReadBinary(endian, content, &i8); err != nil {
		return err
	}
	fieldData.GetScalars().GetIntData().GetData()[i] = int32(i8)
	return nil
}

func fillInt16FieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	// read by offset.
	rowBytes := int64(2)
	content, err := vcm.ReadAt(ctx, dataPath, offset*rowBytes, rowBytes)
	if err != nil {
		return err
	}
	var i16 int16
	if err := funcutil.ReadBinary(endian, content, &i16); err != nil {
		return err
	}
	fieldData.GetScalars().GetIntData().GetData()[i] = int32(i16)
	return nil
}

func fillInt32FieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	// read by offset.
	rowBytes := int64(4)
	content, err := vcm.ReadAt(ctx, dataPath, offset*rowBytes, rowBytes)
	if err != nil {
		return err
	}
	return funcutil.ReadBinary(endian, content, &(fieldData.GetScalars().GetIntData().GetData()[i]))
}

func fillInt64FieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	// read by offset.
	rowBytes := int64(8)
	content, err := vcm.ReadAt(ctx, dataPath, offset*rowBytes, rowBytes)
	if err != nil {
		return err
	}
	return funcutil.ReadBinary(endian, content, &(fieldData.GetScalars().GetLongData().GetData()[i]))
}

func fillFloatFieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	// read by offset.
	rowBytes := int64(4)
	content, err := vcm.ReadAt(ctx, dataPath, offset*rowBytes, rowBytes)
	if err != nil {
		return err
	}
	return funcutil.ReadBinary(endian, content, &(fieldData.GetScalars().GetFloatData().GetData()[i]))
}

func fillDoubleFieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	// read by offset.
	rowBytes := int64(8)
	content, err := vcm.ReadAt(ctx, dataPath, offset*rowBytes, rowBytes)
	if err != nil {
		return err
	}
	return funcutil.ReadBinary(endian, content, &(fieldData.GetScalars().GetDoubleData().GetData()[i]))
}

func fillFieldData(ctx context.Context, vcm storage.ChunkManager, dataPath string, fieldData *schemapb.FieldData, i int, offset int64, endian binary.ByteOrder) error {
	switch fieldData.Type {
	case schemapb.DataType_BinaryVector:
		return fillBinVecFieldData(ctx, vcm, dataPath, fieldData, i, offset, endian)
	case schemapb.DataType_FloatVector:
		return fillFloatVecFieldData(ctx, vcm, dataPath, fieldData, i, offset, endian)
	case schemapb.DataType_Bool:
		return fillBoolFieldData(ctx, vcm, dataPath, fieldData, i, offset, endian)
	case schemapb.DataType_String, schemapb.DataType_VarChar:
		return fillStringFieldData(ctx, vcm, dataPath, fieldData, i, offset, endian)
	case schemapb.DataType_Int8:
		return fillInt8FieldData(ctx, vcm, dataPath, fieldData, i, offset, endian)
	case schemapb.DataType_Int16:
		return fillInt16FieldData(ctx, vcm, dataPath, fieldData, i, offset, endian)
	case schemapb.DataType_Int32:
		return fillInt32FieldData(ctx, vcm, dataPath, fieldData, i, offset, endian)
	case schemapb.DataType_Int64:
		return fillInt64FieldData(ctx, vcm, dataPath, fieldData, i, offset, endian)
	case schemapb.DataType_Float:
		return fillFloatFieldData(ctx, vcm, dataPath, fieldData, i, offset, endian)
	case schemapb.DataType_Double:
		return fillDoubleFieldData(ctx, vcm, dataPath, fieldData, i, offset, endian)
	default:
		return fmt.Errorf("invalid data type: %s", fieldData.Type.String())
	}
}

func (s *Segment) fillIndexedFieldsData(ctx context.Context, collectionID UniqueID,
	vcm storage.ChunkManager, result *segcorepb.RetrieveResults) error {

	for _, fieldData := range result.FieldsData {
		// If the field is not vector field, no need to download data from remote.
		if !typeutil.IsVectorType(fieldData.GetType()) {
			continue
		}
		// If the vector field doesn't have indexed, vector data is in memory
		// for brute force search, no need to download data from remote.
		if !s.hasLoadIndexForIndexedField(fieldData.FieldId) {
			continue
		}
		// If the index has raw data, vector data could be obtained from index,
		// no need to download data from remote.
		if s.hasRawData(fieldData.FieldId) {
			continue
		}

		indexedFieldInfo, err := s.getIndexedFieldInfo(fieldData.FieldId)
		if err != nil {
			continue
		}

		// TODO: optimize here. Now we'll read a whole file from storage every time we retrieve raw data by offset.
		for i, offset := range result.Offset {
			dataPath, offsetInBinlog := s.getFieldDataPath(indexedFieldInfo, offset)
			endian := common.Endian

			// fill field data that fieldData[i] = dataPath[offsetInBinlog*rowBytes, (offsetInBinlog+1)*rowBytes]
			if err := fillFieldData(ctx, vcm, dataPath, fieldData, i, offsetInBinlog, endian); err != nil {
				return err
			}
		}
	}

	return nil
}

func (s *Segment) updateBloomFilter(pks []primaryKey) {
	s.statLock.Lock()
	defer s.statLock.Unlock()
	s.InitCurrentStat()
	buf := make([]byte, 8)
	for _, pk := range pks {
		s.currentStat.UpdateMinMax(pk)
		switch pk.Type() {
		case schemapb.DataType_Int64:
			int64Value := pk.(*int64PrimaryKey).Value
			common.Endian.PutUint64(buf, uint64(int64Value))
			s.currentStat.PkFilter.Add(buf)
		case schemapb.DataType_VarChar:
			stringValue := pk.(*varCharPrimaryKey).Value
			s.currentStat.PkFilter.AddString(stringValue)
		default:
			log.Error("failed to update bloomfilter", zap.Any("PK type", pk.Type()))
			panic("failed to update bloomfilter")
		}
	}
}

func (s *Segment) InitCurrentStat() {
	if s.currentStat == nil {
		s.currentStat = &storage.PkStatistics{
			PkFilter: bloom.NewWithEstimates(storage.BloomFilterSize, storage.MaxBloomFalsePositive),
		}
	}
}

func (s *Segment) isLazyLoading() bool {
	if s.lazyLoading == nil {
		return false
	}
	return s.lazyLoading.Load()
}

// check if PK exists is current
func (s *Segment) isPKExist(pk primaryKey) bool {
	if s.isLazyLoading() {
		log.Warn("processing delete while lazy loading BF, may affect performance", zap.Any("pk", pk), zap.Int64("segmentID", s.segmentID))
		return true
	}
	s.statLock.Lock()
	defer s.statLock.Unlock()

	if s.currentStat != nil && s.currentStat.PkExist(pk) {
		return true
	}

	// for sealed, if one of the stats shows it exist, then we have to check it
	for _, historyStat := range s.historyStats {
		if historyStat.PkExist(pk) {
			return true
		}
	}
	return false
}

// -------------------------------------------------------------------------------------- interfaces for growing segment
func (s *Segment) segmentPreInsert(numOfRecords int) (int64, error) {
	/*
		long int
		PreInsert(CSegmentInterface c_segment, long int size);
	*/
	if s.getType() != segmentTypeGrowing {
		return 0, nil
	}

	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return -1, fmt.Errorf("%w(segmentID=%d)", ErrSegmentUnhealthy, s.segmentID)
	}

	var offset int64
	cOffset := (*C.int64_t)(&offset)
	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.PreInsert(s.segmentPtr, C.int64_t(int64(numOfRecords)), cOffset)
		return struct{}{}, nil
	}).Await()

	if err := HandleCStatus(&status, "PreInsert failed"); err != nil {
		return 0, err
	}
	return offset, nil
}

func (s *Segment) segmentInsert(offset int64, entityIDs []UniqueID, timestamps []Timestamp, record *segcorepb.InsertRecord) error {
	if s.getType() != segmentTypeGrowing {
		return fmt.Errorf("unexpected segmentType when segmentInsert, segmentType = %s", s.segmentType.String())
	}

	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return fmt.Errorf("%w(segmentID=%d)", ErrSegmentUnhealthy, s.segmentID)
	}

	insertRecordBlob, err := proto.Marshal(record)
	if err != nil {
		return fmt.Errorf("failed to marshal insert record: %s", err)
	}

	var numOfRow = len(entityIDs)
	var cOffset = C.int64_t(offset)
	var cNumOfRows = C.int64_t(numOfRow)
	var cEntityIdsPtr = (*C.int64_t)(&(entityIDs)[0])
	var cTimestampsPtr = (*C.uint64_t)(&(timestamps)[0])

	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.Insert(s.segmentPtr,
			cOffset,
			cNumOfRows,
			cEntityIdsPtr,
			cTimestampsPtr,
			(*C.uint8_t)(unsafe.Pointer(&insertRecordBlob[0])),
			(C.uint64_t)(len(insertRecordBlob)))
		return struct{}{}, nil
	}).Await()

	if err := HandleCStatus(&status, "Insert failed"); err != nil {
		return err
	}
	metrics.QueryNodeNumEntities.WithLabelValues(
		fmt.Sprint(Params.QueryNodeCfg.GetNodeID()),
		fmt.Sprint(s.collectionID),
		fmt.Sprint(s.partitionID),
		s.getType().String(),
		fmt.Sprint(0),
	).Add(float64(numOfRow))
	s.setRecentlyModified(true)
	return nil
}

func (s *Segment) segmentDelete(entityIDs []primaryKey, timestamps []Timestamp) error {
	/*
		CStatus
		Delete(CSegmentInterface c_segment,
		           long int reserved_offset,
		           long size,
		           const long* primary_keys,
		           const unsigned long* timestamps);
	*/
	if len(entityIDs) <= 0 {
		return fmt.Errorf("empty pks to delete")
	}

	if len(entityIDs) != len(timestamps) {
		return errors.New("length of entityIDs not equal to length of timestamps")
	}

	if s.deleteBuffer.TryAppend(entityIDs, timestamps) {
		return nil
	}

	return s.deleteImpl(entityIDs, timestamps)
}

func (s *Segment) deleteImpl(pks []primaryKey, timestamps []Timestamp) error {
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return fmt.Errorf("%w(segmentID=%d)", ErrSegmentUnhealthy, s.segmentID)
	}

	start := sort.Search(len(timestamps), func(i int) bool {
		return timestamps[i] >= s.flushedDeletedTimestamp+1
	})
	// all delete records have been applied, skip them
	if start == len(timestamps) {
		return nil
	}
	pks = pks[start:]
	timestamps = timestamps[start:]

	var cSize = C.int64_t(len(pks))
	var cTimestampsPtr = (*C.uint64_t)(&(timestamps)[0])
	offset := C.int64_t(0)

	ids := &schemapb.IDs{}
	pkType := pks[0].Type()
	switch pkType {
	case schemapb.DataType_Int64:
		int64Pks := make([]int64, len(pks))
		for index, entity := range pks {
			int64Pks[index] = entity.(*int64PrimaryKey).Value
		}
		ids.IdField = &schemapb.IDs_IntId{
			IntId: &schemapb.LongArray{
				Data: int64Pks,
			},
		}
	case schemapb.DataType_VarChar:
		varCharPks := make([]string, len(pks))
		for index, entity := range pks {
			varCharPks[index] = entity.(*varCharPrimaryKey).Value
		}
		ids.IdField = &schemapb.IDs_StrId{
			StrId: &schemapb.StringArray{
				Data: varCharPks,
			},
		}
	default:
		return fmt.Errorf("invalid data type of primary keys")
	}

	dataBlob, err := proto.Marshal(ids)
	if err != nil {
		return fmt.Errorf("failed to marshal ids: %s", err)
	}

	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.Delete(s.segmentPtr, offset, cSize, (*C.uint8_t)(unsafe.Pointer(&dataBlob[0])), (C.uint64_t)(len(dataBlob)), cTimestampsPtr)
		return struct{}{}, nil
	}).Await()

	if err := HandleCStatus(&status, "flush delete records failed"); err != nil {
		return err
	}

	return nil
}

func (s *Segment) FlushDelete() error {
	return s.deleteBuffer.Flush(func(pks []primaryKey, tss []Timestamp) error {
		if len(pks) == 0 {
			return nil
		}

		err := s.deleteImpl(pks, tss)
		if err != nil {
			return err
		}

		s.flushedDeletedTimestamp = tss[len(pks)-1]
		return nil
	})
}

// -------------------------------------------------------------------------------------- interfaces for sealed segment
//func (s *Segment) segmentLoadFieldData(fieldID int64, rowCount int64, data *schemapb.FieldData) error {
//	/*
//		CStatus
//		LoadFieldData(CSegmentInterface c_segment, CLoadFieldDataInfo load_field_data_info);
//	*/
//	if s.getType() != segmentTypeSealed {
//		errMsg := fmt.Sprintln("segmentLoadFieldData failed, illegal segment type ", s.segmentType, "segmentID = ", s.ID())
//		return errors.New(errMsg)
//	}
//	s.mut.RLock()
//	defer s.mut.RUnlock()
//	if !s.healthy() {
//		return fmt.Errorf("%w(segmentID=%d)", ErrSegmentUnhealthy, s.segmentID)
//	}
//
//	dataBlob, err := proto.Marshal(data)
//	if err != nil {
//		return err
//	}
//
//	loadInfo := C.CLoadFieldDataInfo{
//		field_id:  C.int64_t(fieldID),
//		blob:      (*C.uint8_t)(unsafe.Pointer(&dataBlob[0])),
//		blob_size: C.uint64_t(len(dataBlob)),
//		row_count: C.int64_t(rowCount),
//	}
//
//	status := C.LoadFieldData(s.segmentPtr, loadInfo)
//
//	if err := HandleCStatus(&status, "LoadFieldData failed"); err != nil {
//		return err
//	}
//
//	log.Info("load field done",
//		zap.Int64("fieldID", fieldID),
//		zap.Int64("row count", rowCount),
//		zap.Int64("segmentID", s.ID()))
//
//	return nil
//}

func (s *Segment) LoadMultiFieldData(rowCount int64, fields []*datapb.FieldBinlog) error {
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return fmt.Errorf("%w(segmentID=%d)", ErrSegmentUnhealthy, s.segmentID)
	}

	loadFieldDataInfo, err := newLoadFieldDataInfo()
	defer deleteFieldDataInfo(loadFieldDataInfo)
	if err != nil {
		return err
	}

	for _, field := range fields {
		fieldID := field.FieldID
		err = loadFieldDataInfo.appendLoadFieldInfo(fieldID, rowCount)
		if err != nil {
			return err
		}

		for _, binlog := range field.Binlogs {
			err = loadFieldDataInfo.appendLoadFieldDataPath(fieldID, binlog.GetLogPath())
			if err != nil {
				return err
			}
		}
	}

	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.LoadFieldData(s.segmentPtr, loadFieldDataInfo.cLoadFieldDataInfo)
		return struct{}{}, nil
	}).Await()
	if err := HandleCStatus(&status, "LoadFieldData failed"); err != nil {
		return err
	}

	log.Info("load mutil field done",
		zap.Int64("row count", rowCount),
		zap.Int64("segmentID", s.ID()))

	return nil
}

func (s *Segment) LoadFieldData(fieldID int64, rowCount int64, field *datapb.FieldBinlog) error {
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return fmt.Errorf("%w(segmentID=%d)", ErrSegmentUnhealthy, s.segmentID)
	}

	loadFieldDataInfo, err := newLoadFieldDataInfo()
	defer deleteFieldDataInfo(loadFieldDataInfo)
	if err != nil {
		return err
	}

	err = loadFieldDataInfo.appendLoadFieldInfo(fieldID, rowCount)
	if err != nil {
		return err
	}

	for _, binlog := range field.Binlogs {
		err = loadFieldDataInfo.appendLoadFieldDataPath(fieldID, binlog.GetLogPath())
		if err != nil {
			return err
		}
	}

	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.LoadFieldData(s.segmentPtr, loadFieldDataInfo.cLoadFieldDataInfo)
		return struct{}{}, nil
	}).Await()
	if err := HandleCStatus(&status, "LoadFieldData failed"); err != nil {
		return err
	}

	log.Info("load field done",
		zap.Int64("fieldID", fieldID),
		zap.Int64("row count", rowCount),
		zap.Int64("segmentID", s.ID()))

	return nil
}

func (s *Segment) segmentLoadDeletedRecord(primaryKeys []primaryKey, timestamps []Timestamp, rowCount int64) error {
	if s.deleteBuffer.TryAppend(primaryKeys, timestamps) {
		return nil
	}

	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return fmt.Errorf("%w(segmentID=%d)", ErrSegmentUnhealthy, s.segmentID)
	}

	if len(primaryKeys) <= 0 {
		return fmt.Errorf("empty pks to delete")
	}
	pkType := primaryKeys[0].Type()
	ids := &schemapb.IDs{}
	switch pkType {
	case schemapb.DataType_Int64:
		int64Pks := make([]int64, len(primaryKeys))
		for index, pk := range primaryKeys {
			int64Pks[index] = pk.(*int64PrimaryKey).Value
		}
		ids.IdField = &schemapb.IDs_IntId{
			IntId: &schemapb.LongArray{
				Data: int64Pks,
			},
		}
	case schemapb.DataType_VarChar:
		varCharPks := make([]string, len(primaryKeys))
		for index, pk := range primaryKeys {
			varCharPks[index] = pk.(*varCharPrimaryKey).Value
		}
		ids.IdField = &schemapb.IDs_StrId{
			StrId: &schemapb.StringArray{
				Data: varCharPks,
			},
		}
	default:
		return fmt.Errorf("invalid data type of primary keys")
	}

	idsBlob, err := proto.Marshal(ids)
	if err != nil {
		return err
	}

	loadInfo := C.CLoadDeletedRecordInfo{
		timestamps:        unsafe.Pointer(&timestamps[0]),
		primary_keys:      (*C.uint8_t)(unsafe.Pointer(&idsBlob[0])),
		primary_keys_size: C.uint64_t(len(idsBlob)),
		row_count:         C.int64_t(rowCount),
	}
	/*
		CStatus
		LoadDeletedRecord(CSegmentInterface c_segment, CLoadDeletedRecordInfo deleted_record_info)
	*/
	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.LoadDeletedRecord(s.segmentPtr, loadInfo)
		return struct{}{}, nil
	}).Await()

	if err := HandleCStatus(&status, "LoadDeletedRecord failed"); err != nil {
		return err
	}

	log.Info("load deleted record done",
		zap.Int64("row count", rowCount),
		zap.Int64("segmentID", s.ID()),
		zap.String("segmentType", s.getType().String()))
	return nil
}

func (s *Segment) segmentLoadIndexData(indexInfo *querypb.FieldIndexInfo, fieldType schemapb.DataType) error {
	loadIndexInfo, err := newLoadIndexInfo()
	defer deleteLoadIndexInfo(loadIndexInfo)
	if err != nil {
		return err
	}

	err = loadIndexInfo.appendLoadIndexInfo(nil, indexInfo, s.collectionID, s.partitionID, s.segmentID, fieldType)
	if err != nil {
		if loadIndexInfo.cleanLocalData() != nil {
			log.Warn("failed to clean cached data on disk after append index failed",
				zap.Int64("buildID", indexInfo.BuildID),
				zap.Int64("index version", indexInfo.IndexVersion))
		}
		return err
	}
	if s.getType() != segmentTypeSealed {
		errMsg := fmt.Sprintln("updateSegmentIndex failed, illegal segment type ", s.segmentType, "segmentID = ", s.ID())
		return errors.New(errMsg)
	}
	s.mut.RLock()
	defer s.mut.RUnlock()
	if !s.healthy() {
		return fmt.Errorf("%w(segmentID=%d)", ErrSegmentUnhealthy, s.segmentID)
	}

	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.UpdateSealedSegmentIndex(s.segmentPtr, loadIndexInfo.cLoadIndexInfo)
		return struct{}{}, nil
	}).Await()

	if err := HandleCStatus(&status, "UpdateSealedSegmentIndex failed"); err != nil {
		return err
	}

	log.Info("updateSegmentIndex done", zap.Int64("segmentID", s.ID()), zap.Int64("fieldID", indexInfo.FieldID))

	return nil
}

func (s *Segment) UpdateFieldRawDataSize(numRows int64, fieldBinlog *datapb.FieldBinlog) error {
	fieldID := fieldBinlog.FieldID
	fieldDataSize := int64(0)
	for _, binlog := range fieldBinlog.GetBinlogs() {
		fieldDataSize += binlog.LogSize
	}

	var status C.CStatus
	GetDynamicPool().Submit(func() (any, error) {
		status = C.UpdateFieldRawDataSize(s.segmentPtr, C.int64_t(fieldID), C.int64_t(numRows), C.int64_t(fieldDataSize))
		return nil, nil
	}).Await()

	if err := HandleCStatus(&status, "updateFieldRawDataSize failed"); err != nil {
		return err
	}

	log.Info("updateFieldRawDataSize done", zap.Int64("segmentID", s.ID()), zap.Int64("fieldID", fieldBinlog.FieldID))

	return nil
}