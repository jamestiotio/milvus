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

package syncmgr

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/apache/arrow/go/v12/arrow"
	"github.com/apache/arrow/go/v12/arrow/array"
	"github.com/apache/arrow/go/v12/arrow/memory"
	"github.com/samber/lo"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/msgpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	milvus_storage "github.com/milvus-io/milvus-storage/go/storage"
	"github.com/milvus-io/milvus-storage/go/storage/options"
	"github.com/milvus-io/milvus-storage/go/storage/schema"
	"github.com/milvus-io/milvus/internal/allocator"
	"github.com/milvus-io/milvus/internal/datanode/broker"
	"github.com/milvus-io/milvus/internal/datanode/metacache"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/pkg/common"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/tsoutil"
)

type SyncTaskSuiteV2 struct {
	suite.Suite

	collectionID int64
	partitionID  int64
	segmentID    int64
	channelName  string

	metacache   *metacache.MockMetaCache
	allocator   *allocator.MockGIDAllocator
	schema      *schemapb.CollectionSchema
	arrowSchema *arrow.Schema
	broker      *broker.MockBroker

	space *milvus_storage.Space
}

func (s *SyncTaskSuiteV2) SetupSuite() {
	paramtable.Get().Init(paramtable.NewBaseTable())

	s.collectionID = 100
	s.partitionID = 101
	s.segmentID = 1001
	s.channelName = "by-dev-rootcoord-dml_0_100v0"

	s.schema = &schemapb.CollectionSchema{
		Name: "sync_task_test_col",
		Fields: []*schemapb.FieldSchema{
			{FieldID: common.RowIDField, Name: common.RowIDFieldName, DataType: schemapb.DataType_Int64},
			{FieldID: common.TimeStampField, Name: common.TimeStampFieldName, DataType: schemapb.DataType_Int64},
			{
				FieldID:      100,
				Name:         "pk",
				DataType:     schemapb.DataType_Int64,
				IsPrimaryKey: true,
			},
			{
				FieldID:  101,
				Name:     "vector",
				DataType: schemapb.DataType_FloatVector,
				TypeParams: []*commonpb.KeyValuePair{
					{Key: common.DimKey, Value: "128"},
				},
			},
		},
	}

	arrowSchema, err := metacache.ConvertToArrowSchema(s.schema.Fields)
	s.NoError(err)
	s.arrowSchema = arrowSchema
}

func (s *SyncTaskSuiteV2) SetupTest() {
	s.allocator = allocator.NewMockGIDAllocator()
	s.allocator.AllocF = func(count uint32) (int64, int64, error) {
		return time.Now().Unix(), int64(count), nil
	}
	s.allocator.AllocOneF = func() (allocator.UniqueID, error) {
		return time.Now().Unix(), nil
	}

	s.broker = broker.NewMockBroker(s.T())
	s.metacache = metacache.NewMockMetaCache(s.T())

	tmpDir := s.T().TempDir()
	space, err := milvus_storage.Open(fmt.Sprintf("file:///%s", tmpDir), options.NewSpaceOptionBuilder().
		SetSchema(schema.NewSchema(s.arrowSchema, &schema.SchemaOptions{
			PrimaryColumn: "pk", VectorColumn: "vector", VersionColumn: common.TimeStampFieldName,
		})).Build())
	s.Require().NoError(err)
	s.space = space
}

func (s *SyncTaskSuiteV2) getEmptyInsertBuffer() *storage.InsertData {
	buf, err := storage.NewInsertData(s.schema)
	s.Require().NoError(err)

	return buf
}

func (s *SyncTaskSuiteV2) getInsertBuffer() *storage.InsertData {
	buf := s.getEmptyInsertBuffer()

	// generate data
	for i := 0; i < 10; i++ {
		data := make(map[storage.FieldID]any)
		data[common.RowIDField] = int64(i + 1)
		data[common.TimeStampField] = int64(i + 1)
		data[100] = int64(i + 1)
		vector := lo.RepeatBy(128, func(_ int) float32 {
			return rand.Float32()
		})
		data[101] = vector
		err := buf.Append(data)
		s.Require().NoError(err)
	}
	return buf
}

func (s *SyncTaskSuiteV2) getDeleteBuffer() *storage.DeleteData {
	buf := &storage.DeleteData{}
	for i := 0; i < 10; i++ {
		pk := storage.NewInt64PrimaryKey(int64(i + 1))
		ts := tsoutil.ComposeTSByTime(time.Now(), 0)
		buf.Append(pk, ts)
	}
	return buf
}

func (s *SyncTaskSuiteV2) getDeleteBufferZeroTs() *storage.DeleteData {
	buf := &storage.DeleteData{}
	for i := 0; i < 10; i++ {
		pk := storage.NewInt64PrimaryKey(int64(i + 1))
		buf.Append(pk, 0)
	}
	return buf
}

func (s *SyncTaskSuiteV2) getSuiteSyncTask() *SyncTaskV2 {
	log.Info("space", zap.Any("space", s.space))
	task := NewSyncTaskV2().
		WithArrowSchema(s.arrowSchema).
		WithSpace(s.space).
		WithCollectionID(s.collectionID).
		WithPartitionID(s.partitionID).
		WithSegmentID(s.segmentID).
		WithChannelName(s.channelName).
		WithSchema(s.schema).
		WithAllocator(s.allocator).
		WithMetaCache(s.metacache)

	return task
}

func (s *SyncTaskSuiteV2) TestRunNormal() {
	s.broker.EXPECT().SaveBinlogPaths(mock.Anything, mock.Anything).Return(nil)
	bfs := metacache.NewBloomFilterSet()
	fd, err := storage.NewFieldData(schemapb.DataType_Int64, &schemapb.FieldSchema{
		FieldID:      101,
		Name:         "ID",
		IsPrimaryKey: true,
		DataType:     schemapb.DataType_Int64,
	})
	s.Require().NoError(err)

	ids := []int64{1, 2, 3, 4, 5, 6, 7}
	for _, id := range ids {
		err = fd.AppendRow(id)
		s.Require().NoError(err)
	}

	bfs.UpdatePKRange(fd)
	seg := metacache.NewSegmentInfo(&datapb.SegmentInfo{}, bfs)
	metacache.UpdateNumOfRows(1000)(seg)
	s.metacache.EXPECT().GetSegmentsBy(mock.Anything).Return([]*metacache.SegmentInfo{seg})
	s.metacache.EXPECT().UpdateSegments(mock.Anything, mock.Anything).Return()

	s.Run("without_insert_delete", func() {
		task := s.getSuiteSyncTask()
		task.WithMetaWriter(BrokerMetaWriter(s.broker))
		task.WithTimeRange(50, 100)
		task.WithCheckpoint(&msgpb.MsgPosition{
			ChannelName: s.channelName,
			MsgID:       []byte{1, 2, 3, 4},
			Timestamp:   100,
		})

		err := task.Run()
		s.NoError(err)
	})

	s.Run("with_insert_delete_cp", func() {
		task := s.getSuiteSyncTask()
		task.WithInsertData(s.getInsertBuffer()).WithDeleteData(s.getDeleteBuffer())
		task.WithTimeRange(50, 100)
		task.WithMetaWriter(BrokerMetaWriter(s.broker))
		task.WithCheckpoint(&msgpb.MsgPosition{
			ChannelName: s.channelName,
			MsgID:       []byte{1, 2, 3, 4},
			Timestamp:   100,
		})

		err := task.Run()
		s.NoError(err)
	})

	s.Run("with_insert_delete_flush", func() {
		task := s.getSuiteSyncTask()
		task.WithInsertData(s.getInsertBuffer()).WithDeleteData(s.getDeleteBuffer())
		task.WithFlush()
		task.WithDrop()
		task.WithMetaWriter(BrokerMetaWriter(s.broker))
		task.WithCheckpoint(&msgpb.MsgPosition{
			ChannelName: s.channelName,
			MsgID:       []byte{1, 2, 3, 4},
			Timestamp:   100,
		})

		err := task.Run()
		s.NoError(err)
	})
}

func (s *SyncTaskSuiteV2) TestBuildRecord() {
	fieldSchemas := []*schemapb.FieldSchema{
		{FieldID: 1, Name: "field0", DataType: schemapb.DataType_Bool},
		{FieldID: 2, Name: "field1", DataType: schemapb.DataType_Int8},
		{FieldID: 3, Name: "field2", DataType: schemapb.DataType_Int16},
		{FieldID: 4, Name: "field3", DataType: schemapb.DataType_Int32},
		{FieldID: 5, Name: "field4", DataType: schemapb.DataType_Int64},
		{FieldID: 6, Name: "field5", DataType: schemapb.DataType_Float},
		{FieldID: 7, Name: "field6", DataType: schemapb.DataType_Double},
		{FieldID: 8, Name: "field7", DataType: schemapb.DataType_String},
		{FieldID: 9, Name: "field8", DataType: schemapb.DataType_VarChar},
		{FieldID: 10, Name: "field9", DataType: schemapb.DataType_BinaryVector, TypeParams: []*commonpb.KeyValuePair{{Key: "dim", Value: "8"}}},
		{FieldID: 11, Name: "field10", DataType: schemapb.DataType_FloatVector, TypeParams: []*commonpb.KeyValuePair{{Key: "dim", Value: "4"}}},
		{FieldID: 12, Name: "field11", DataType: schemapb.DataType_JSON},
		{FieldID: 13, Name: "field12", DataType: schemapb.DataType_Float16Vector, TypeParams: []*commonpb.KeyValuePair{{Key: "dim", Value: "4"}}},
		{FieldID: 14, Name: "field13", DataType: schemapb.DataType_Array, ElementType: schemapb.DataType_Int32},
		{FieldID: 15, Name: "field14", DataType: schemapb.DataType_Array, ElementType: schemapb.DataType_Bool},
		{FieldID: 16, Name: "field15", DataType: schemapb.DataType_Array, ElementType: schemapb.DataType_Int8},
		{FieldID: 17, Name: "field16", DataType: schemapb.DataType_Array, ElementType: schemapb.DataType_Int16},
		{FieldID: 18, Name: "field17", DataType: schemapb.DataType_Array, ElementType: schemapb.DataType_Int64},
		{FieldID: 19, Name: "field18", DataType: schemapb.DataType_Array, ElementType: schemapb.DataType_Float},
		{FieldID: 20, Name: "field19", DataType: schemapb.DataType_Array, ElementType: schemapb.DataType_Double},
		{FieldID: 21, Name: "field20", DataType: schemapb.DataType_Array, ElementType: schemapb.DataType_String},
	}

	schema, err := metacache.ConvertToArrowSchema(fieldSchemas)
	s.NoError(err)

	b := array.NewRecordBuilder(memory.NewGoAllocator(), schema)
	defer b.Release()

	data := &storage.InsertData{
		Data: map[int64]storage.FieldData{
			1:  &storage.BoolFieldData{Data: []bool{true, false}},
			2:  &storage.Int8FieldData{Data: []int8{3, 4}},
			3:  &storage.Int16FieldData{Data: []int16{3, 4}},
			4:  &storage.Int32FieldData{Data: []int32{3, 4}},
			5:  &storage.Int64FieldData{Data: []int64{3, 4}},
			6:  &storage.FloatFieldData{Data: []float32{3, 4}},
			7:  &storage.DoubleFieldData{Data: []float64{3, 4}},
			8:  &storage.StringFieldData{Data: []string{"3", "4"}},
			9:  &storage.StringFieldData{Data: []string{"3", "4"}},
			10: &storage.BinaryVectorFieldData{Data: []byte{0, 255}, Dim: 8},
			11: &storage.FloatVectorFieldData{
				Data: []float32{4, 5, 6, 7, 4, 5, 6, 7},
				Dim:  4,
			},
			12: &storage.JSONFieldData{
				Data: [][]byte{
					[]byte(`{"batch":2}`),
					[]byte(`{"key":"world"}`),
				},
			},
			13: &storage.Float16VectorFieldData{
				Data: []byte{0, 255, 0, 255, 0, 255, 0, 255, 0, 255, 0, 255, 0, 255, 0, 255},
				Dim:  4,
			},
			14: &storage.ArrayFieldData{
				ElementType: schemapb.DataType_Int32,
				Data: []*schemapb.ScalarField{
					{
						Data: &schemapb.ScalarField_IntData{
							IntData: &schemapb.IntArray{Data: []int32{3, 2, 1}},
						},
					},
					{
						Data: &schemapb.ScalarField_IntData{
							IntData: &schemapb.IntArray{Data: []int32{6, 5, 4}},
						},
					},
				},
			},
			15: &storage.ArrayFieldData{
				ElementType: schemapb.DataType_Bool,
				Data: []*schemapb.ScalarField{
					{
						Data: &schemapb.ScalarField_BoolData{
							BoolData: &schemapb.BoolArray{Data: []bool{false, false, false}},
						},
					},
					{
						Data: &schemapb.ScalarField_BoolData{
							BoolData: &schemapb.BoolArray{Data: []bool{false, false, false}},
						},
					},
				},
			},
			16: &storage.ArrayFieldData{
				ElementType: schemapb.DataType_Int8,
				Data: []*schemapb.ScalarField{
					{
						Data: &schemapb.ScalarField_IntData{
							IntData: &schemapb.IntArray{Data: []int32{3, 2, 1}},
						},
					},
					{
						Data: &schemapb.ScalarField_IntData{
							IntData: &schemapb.IntArray{Data: []int32{6, 5, 4}},
						},
					},
				},
			},
			17: &storage.ArrayFieldData{
				ElementType: schemapb.DataType_Int16,
				Data: []*schemapb.ScalarField{
					{
						Data: &schemapb.ScalarField_IntData{
							IntData: &schemapb.IntArray{Data: []int32{3, 2, 1}},
						},
					},
					{
						Data: &schemapb.ScalarField_IntData{
							IntData: &schemapb.IntArray{Data: []int32{6, 5, 4}},
						},
					},
				},
			},
			18: &storage.ArrayFieldData{
				ElementType: schemapb.DataType_Int64,
				Data: []*schemapb.ScalarField{
					{
						Data: &schemapb.ScalarField_LongData{
							LongData: &schemapb.LongArray{Data: []int64{3, 2, 1}},
						},
					},
					{
						Data: &schemapb.ScalarField_LongData{
							LongData: &schemapb.LongArray{Data: []int64{3, 2, 1}},
						},
					},
				},
			},
			19: &storage.ArrayFieldData{
				ElementType: schemapb.DataType_Float,
				Data: []*schemapb.ScalarField{
					{
						Data: &schemapb.ScalarField_FloatData{
							FloatData: &schemapb.FloatArray{Data: []float32{3, 2, 1}},
						},
					},
					{
						Data: &schemapb.ScalarField_FloatData{
							FloatData: &schemapb.FloatArray{Data: []float32{3, 2, 1}},
						},
					},
				},
			},
			20: &storage.ArrayFieldData{
				ElementType: schemapb.DataType_Double,
				Data: []*schemapb.ScalarField{
					{
						Data: &schemapb.ScalarField_DoubleData{
							DoubleData: &schemapb.DoubleArray{Data: []float64{3, 2, 1}},
						},
					},
					{
						Data: &schemapb.ScalarField_DoubleData{
							DoubleData: &schemapb.DoubleArray{Data: []float64{3, 2, 1}},
						},
					},
				},
			},
			21: &storage.ArrayFieldData{
				ElementType: schemapb.DataType_String,
				Data: []*schemapb.ScalarField{
					{
						Data: &schemapb.ScalarField_StringData{
							StringData: &schemapb.StringArray{Data: []string{"a", "b", "c"}},
						},
					},
					{
						Data: &schemapb.ScalarField_StringData{
							StringData: &schemapb.StringArray{Data: []string{"a", "b", "c"}},
						},
					},
				},
			},
		},
	}

	err = buildRecord(b, data, fieldSchemas)
	s.NoError(err)
	s.EqualValues(2, b.NewRecord().NumRows())
}

func (s *SyncTaskSuiteV2) TestAppendLists() {
}

func TestSyncTaskV2(t *testing.T) {
	suite.Run(t, new(SyncTaskSuiteV2))
}
