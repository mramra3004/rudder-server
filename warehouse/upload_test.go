//go:build !warehouse_integration

package warehouse

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/services/stats/memstats"
	"github.com/stretchr/testify/require"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ory/dockertest/v3"
	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	"github.com/rudderlabs/rudder-server/testhelper/destination"
	"github.com/rudderlabs/rudder-server/utils/logger"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

func TestExtractUploadErrorsByState(t *testing.T) {
	input := []struct {
		InitialErrorState []byte
		CurrentErrorState string
		CurrentError      error
		ErrorCount        int
	}{
		{
			InitialErrorState: []byte(`{}`),
			CurrentErrorState: InternalProcessingFailed,
			CurrentError:      errors.New("account locked"),
			ErrorCount:        1,
		},
		{
			InitialErrorState: []byte(`{"internal_processing_failed": {"errors": ["account locked"], "attempt": 1}}`),
			CurrentErrorState: InternalProcessingFailed,
			CurrentError:      errors.New("account locked again"),
			ErrorCount:        2,
		},
		{
			InitialErrorState: []byte(`{"internal_processing_failed": {"errors": ["account locked", "account locked again"], "attempt": 2}}`),
			CurrentErrorState: TableUploadExportingFailed,
			CurrentError:      errors.New("failed to load data because failed in earlier job"),
			ErrorCount:        1,
		},
	}

	for _, ip := range input {

		uploadErrors, err := extractAndUpdateUploadErrorsByState(ip.InitialErrorState, ip.CurrentErrorState, ip.CurrentError)
		if err != nil {
			t.Errorf("extracting upload errors by state should have passed: %v", err)
		}

		stateErrors := uploadErrors[ip.CurrentErrorState]
		// Below switch clause mirrors how we are
		// adding data in generic interface.

		var errorLength int
		switch stateErrors["errors"].(type) {
		case []string:
			errorLength = len(stateErrors["errors"].([]string))
		case []interface{}:
			errorLength = len(stateErrors["errors"].([]interface{}))
		}

		if errorLength != ip.ErrorCount {
			t.Errorf("expected error to be addded to list of state errors")
		}

		if stateErrors["attempt"].(int) != ip.ErrorCount {
			t.Errorf("expected attempts to be: %d, got: %d", ip.ErrorCount, stateErrors["attempt"].(int))
		}
	}
}

func TestColumnCountStat(t *testing.T) {
	Init()
	Init4()

	var (
		workspaceID     = "test-workspaceID"
		destinationID   = "test-desinationID"
		destinationName = "test-desinationName"
		sourceID        = "test-sourceID"
		sourceName      = "test-sourceName"
		tableName       = "test-table"
	)

	inputs := []struct {
		name             string
		columnCountLimit int
		destinationType  string
		statExpected     bool
	}{
		{
			name:             "Datalakes destination",
			destinationType:  warehouseutils.S3_DATALAKE,
			columnCountLimit: 1,
		},
		{
			name:            "Unknown destination",
			destinationType: "unknown-destination",
		},
		{
			name:             "Greater than threshold",
			destinationType:  "test-destination",
			columnCountLimit: 1,
			statExpected:     true,
		},
		{
			name:             "Lesser than threshold",
			destinationType:  "test-destination",
			columnCountLimit: 10,
			statExpected:     true,
		},
	}

	store := memstats.New()

	for _, tc := range inputs {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			columnCountLimitMap = map[string]int{
				"test-destination": tc.columnCountLimit,
			}

			j := UploadJobT{
				upload: &Upload{
					WorkspaceID:   workspaceID,
					DestinationID: destinationID,
					SourceID:      sourceID,
				},
				warehouse: warehouseutils.Warehouse{
					Type: tc.destinationType,
					Destination: backendconfig.DestinationT{
						ID:   destinationID,
						Name: destinationName,
					},
					Source: backendconfig.SourceT{
						ID:   sourceID,
						Name: sourceName,
					},
				},
				stats: store,
				schemaHandle: &SchemaHandleT{
					schemaInWarehouse: warehouseutils.SchemaT{
						tableName: map[string]string{
							"test-column-1": "string",
							"test-column-2": "string",
							"test-column-3": "string",
						},
					},
				},
			}

			tags := stats.Tags{
				"module":      moduleName,
				"destType":    tc.destinationType,
				"warehouseID": j.warehouseID(),
				"workspaceId": workspaceID,
				"destID":      destinationID,
				"sourceID":    sourceID,
				"tableName":   tableName,
			}

			j.columnCountStat(tableName)

			m1 := store.Get("warehouse_load_table_column_count", tags)
			m2 := store.Get("warehouse_load_table_column_limit", tags)

			if tc.statExpected {
				require.EqualValues(t, m1.LastValue(), len(j.schemaHandle.schemaInWarehouse[tableName]))
				require.EqualValues(t, m2.LastValue(), tc.columnCountLimit)
			} else {
				require.Nil(t, m1)
				require.Nil(t, m2)
			}
		})
	}
}

var _ = Describe("Upload", Ordered, func() {
	var (
		sourceID        = "test-sourceID"
		destinationID   = "test-destinationID"
		destinationName = "test-destinationName"
		namespace       = "test-namespace"
		destinationType = "POSTGRES"
		g               = GinkgoT()
	)

	var (
		pgResource *destination.PostgresResource
		job        *UploadJobT
	)

	BeforeAll(func() {
		pool, err := dockertest.NewPool("")
		Expect(err).To(BeNil())

		pgResource = setupWarehouseJobs(pool, GinkgoT())

		initWarehouse()

		err = setupDB(context.TODO(), getConnectionString())
		Expect(err).To(BeNil())

		sqlStatement, err := os.ReadFile("testdata/sql/3.sql")
		Expect(err).To(BeNil())

		_, err = pgResource.DB.Exec(string(sqlStatement))
		Expect(err).To(BeNil())

		pkgLogger = logger.NOP
	})

	BeforeEach(func() {
		job = &UploadJobT{
			warehouse: warehouseutils.Warehouse{
				Type: destinationType,
				Destination: backendconfig.DestinationT{
					ID:   destinationID,
					Name: destinationName,
				},
				Source: backendconfig.SourceT{
					ID:   sourceID,
					Name: destinationName,
				},
			},
			upload: &Upload{
				ID:                 1,
				DestinationID:      destinationID,
				SourceID:           sourceID,
				StartStagingFileID: 1,
				EndStagingFileID:   5,
				Namespace:          namespace,
			},
			stagingFileIDs: []int64{1, 2, 3, 4, 5},
			dbHandle:       pgResource.DB,
		}
	})

	It("Total rows in load files", func() {
		count := job.getTotalRowsInLoadFiles()
		Expect(count).To(BeEquivalentTo(5))
	})

	It("Total rows in staging files", func() {
		count := job.getTotalRowsInStagingFiles()
		Expect(count).To(BeEquivalentTo(5))
	})

	It("Fetch pending upload status", func() {
		job.upload.ID = 5

		tus := job.fetchPendingUploadTableStatus()
		Expect(tus).NotTo(BeNil())
		Expect(tus).Should(HaveLen(2))
	})

	DescribeTable("Are all table skip errors", func(loadErrors []error, expected bool) {
		Expect(areAllTableSkipErrors(loadErrors)).To(Equal(expected))
	},
		Entry(nil, []error{}, true),
		Entry(nil, []error{&TableSkipError{}}, true),
		Entry(nil, []error{errors.New("some-error")}, false),
	)

	DescribeTable("Get table upload status map", func(tableUploadStatuses []*TableUploadStatusT, expected map[int64]map[string]*TableUploadStatusInfoT) {
		Expect(getTableUploadStatusMap(tableUploadStatuses)).To(Equal(expected))
	},
		Entry(nil, []*TableUploadStatusT{}, map[int64]map[string]*TableUploadStatusInfoT{}),

		Entry(nil,
			[]*TableUploadStatusT{
				{
					uploadID:  1,
					tableName: "test-tableName-1",
				},
				{
					uploadID:  2,
					tableName: "test-tableName-2",
				},
			},
			map[int64]map[string]*TableUploadStatusInfoT{
				1: {
					"test-tableName-1": {},
				},
				2: {
					"test-tableName-2": {},
				},
			},
		),
	)

	It("Getting tables to skip", func() {
		job.upload.ID = 5

		previousFailedMap, currentSuceededMap := job.getTablesToSkip()
		Expect(previousFailedMap).Should(HaveLen(1))
		Expect(currentSuceededMap).Should(HaveLen(0))
	})

	It("Get uploads timings", func() {
		Expect(job.getUploadTimings()).To(BeEquivalentTo([]map[string]string{
			{
				"exported_data":  "2020-04-21 15:26:34.344356",
				"exporting_data": "2020-04-21 15:16:19.687716",
			},
		}))
	})

	Describe("Staging files and load files events match", func() {
		When("Matched", func() {
			It("Should not send stats", func() {
				job.matchRowsInStagingAndLoadFiles()
			})
		})

		When("Not matched", func() {
			It("Should send stats", func() {
				mockStats, mockMeasurement := getMockStats(g)
				mockStats.EXPECT().NewTaggedStat(gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(mockMeasurement)
				mockMeasurement.EXPECT().Gauge(gomock.Any()).Times(1)

				job.stats = mockStats
				job.stagingFileIDs = []int64{1, 2}
				job.matchRowsInStagingAndLoadFiles()
			})
		})
	})
})
