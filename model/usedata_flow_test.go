package model

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func seedFlowQuotaData(t *testing.T, quotaData QuotaData) {
	t.Helper()
	require.NoError(t, DB.Create(&quotaData).Error)
}

func resetFlowQuotaDataCache(t *testing.T) {
	t.Helper()
	CacheQuotaDataLock.Lock()
	previousCache := CacheQuotaData
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaDataLock.Unlock()
	t.Cleanup(func() {
		CacheQuotaDataLock.Lock()
		CacheQuotaData = previousCache
		CacheQuotaDataLock.Unlock()
	})
}

func seedFlowLookupData(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.Create(&Channel{Id: 1, Name: "east"}).Error)
	require.NoError(t, DB.Create(&Channel{Id: 2, Name: "west"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 11, UserId: 1, Key: "sk-primary", Name: "primary"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 22, UserId: 2, Key: "sk-backup", Name: "backup"}).Error)
	require.NoError(t, DB.Delete(&Token{Id: 11}).Error)
}

func TestGetFlowQuotaDataUsesQuotaDataRoleSpecificDimensions(t *testing.T) {
	truncateTables(t)
	seedFlowLookupData(t)

	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 1,
		CreatedAt: 1000,
		Count:     2,
		Quota:     100,
		TokenUsed: 40,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 1,
		CreatedAt: 1100,
		Count:     1,
		Quota:     50,
		TokenUsed: 20,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		NodeName:  "node-a",
		TokenID:   11,
		UseGroup:  "vip",
		ModelName: "gpt-a",
		ChannelID: 2,
		CreatedAt: 1200,
		Count:     1,
		Quota:     25,
		TokenUsed: 10,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    2,
		Username:  "bob",
		NodeName:  "node-b",
		TokenID:   22,
		UseGroup:  "default",
		ModelName: "gpt-b",
		ChannelID: 1,
		CreatedAt: 1300,
		Count:     3,
		Quota:     70,
		TokenUsed: 30,
	})
	seedFlowQuotaData(t, QuotaData{
		UserID:    1,
		Username:  "alice",
		ModelName: "legacy",
		CreatedAt: 1400,
		Count:     99,
		Quota:     999,
		TokenUsed: 999,
	})

	rootRows, err := GetFlowQuotaData(900, 2000, "", 0, common.RoleRootUser)
	require.NoError(t, err)
	require.Len(t, rootRows, 3)
	// Token 11 was soft-deleted, so its name is intentionally left empty for the
	// frontend to render a localized "deleted (id)" label instead.
	require.Equal(t, FlowQuotaData{
		UserID:      1,
		Username:    "alice",
		NodeName:    "node-a",
		TokenID:     11,
		TokenName:   "",
		UseGroup:    "vip",
		ChannelID:   1,
		ChannelName: "east",
		ModelName:   "gpt-a",
		TokenUsed:   60,
		Count:       3,
		Quota:       150,
	}, *rootRows[0])
	// A token that still exists resolves to its current name.
	require.Equal(t, 22, rootRows[1].TokenID)
	require.Equal(t, "backup", rootRows[1].TokenName)

	adminRows, err := GetFlowQuotaData(900, 2000, "alice", 0, common.RoleAdminUser)
	require.NoError(t, err)
	require.Len(t, adminRows, 2)
	require.Equal(t, 0, adminRows[0].TokenID)
	require.Empty(t, adminRows[0].TokenName)
	require.Empty(t, adminRows[0].NodeName)
	require.Equal(t, "alice", adminRows[0].Username)
	require.Equal(t, "vip", adminRows[0].UseGroup)
	require.Equal(t, "east", adminRows[0].ChannelName)
	require.Equal(t, 150, adminRows[0].Quota)

	selfRows, err := GetFlowQuotaData(900, 2000, "", 1, common.RoleCommonUser)
	require.NoError(t, err)
	require.Len(t, selfRows, 1)
	require.Empty(t, selfRows[0].Username)
	require.Equal(t, 0, selfRows[0].ChannelID)
	require.Empty(t, selfRows[0].ChannelName)
	require.Empty(t, selfRows[0].TokenName)
	require.Equal(t, "vip", selfRows[0].UseGroup)
	require.Equal(t, 175, selfRows[0].Quota)
}

func TestLogQuotaDataSplitsRowsByUseGroupTokenChannelAndNode(t *testing.T) {
	truncateTables(t)
	resetFlowQuotaDataCache(t)

	LogQuotaData(QuotaDataLogParams{
		UserID:    1,
		Username:  "alice",
		ModelName: "gpt-a",
		CreatedAt: 3661,
		UseGroup:  "vip",
		TokenID:   11,
		ChannelID: 1,
		NodeName:  "node-a",
		Quota:     100,
		TokenUsed: 40,
	})
	LogQuotaData(QuotaDataLogParams{
		UserID:    1,
		Username:  "alice",
		ModelName: "gpt-a",
		CreatedAt: 3700,
		UseGroup:  "vip",
		TokenID:   11,
		ChannelID: 1,
		NodeName:  "node-a",
		Quota:     50,
		TokenUsed: 20,
	})
	LogQuotaData(QuotaDataLogParams{
		UserID:    1,
		Username:  "alice",
		ModelName: "gpt-a",
		CreatedAt: 3700,
		UseGroup:  "default",
		TokenID:   11,
		ChannelID: 1,
		NodeName:  "node-a",
		Quota:     25,
		TokenUsed: 10,
	})

	SaveQuotaDataCache()

	var rows []QuotaData
	require.NoError(t, DB.Order("quota DESC").Find(&rows).Error)
	require.Len(t, rows, 2)
	require.Equal(t, int64(3600), rows[0].CreatedAt)
	require.Equal(t, "vip", rows[0].UseGroup)
	require.Equal(t, 11, rows[0].TokenID)
	require.Equal(t, 1, rows[0].ChannelID)
	require.Equal(t, "node-a", rows[0].NodeName)
	require.Equal(t, 2, rows[0].Count)
	require.Equal(t, 150, rows[0].Quota)
	require.Equal(t, 60, rows[0].TokenUsed)
	require.Equal(t, "default", rows[1].UseGroup)
	require.Equal(t, 25, rows[1].Quota)
}

func TestSaveQuotaDataCacheWrites(t *testing.T) {
	for _, tc := range []struct {
		name          string
		existing      bool
		noChange      bool
		failAt        string
		afterCommit   bool
		anotherBucket bool
		wantOps       []string
		wantRows      int
		wantSaved     int
		wantTotals    [3]int
	}{
		{name: "existing bucket", existing: true, wantOps: []string{"UPDATE"}, wantRows: 1, wantSaved: 1, wantTotals: [3]int{3, 150, 60}},
		{name: "new bucket", wantOps: []string{"UPDATE", "SELECT", "INSERT"}, wantRows: 1, wantSaved: 1, wantTotals: [3]int{1, 50, 20}},
		{name: "unchanged bucket with zero affected rows", existing: true, noChange: true, wantOps: []string{"UPDATE", "SELECT"}, wantRows: 1, wantSaved: 1, wantTotals: [3]int{2, 100, 40}},
		{name: "new bucket with zero increment", noChange: true, wantOps: []string{"UPDATE", "SELECT", "INSERT"}, wantRows: 1, wantSaved: 1},
		{name: "update error", failAt: "UPDATE", wantOps: []string{"UPDATE"}},
		{name: "existence query error", failAt: "SELECT", wantOps: []string{"UPDATE", "SELECT"}},
		{name: "insert error", failAt: "INSERT", wantOps: []string{"UPDATE", "SELECT", "INSERT"}},
		{name: "update committed but result unknown", existing: true, failAt: "UPDATE", afterCommit: true, wantOps: []string{"UPDATE"}, wantRows: 1, wantTotals: [3]int{3, 150, 60}},
		{name: "insert committed but result unknown", failAt: "INSERT", afterCommit: true, wantOps: []string{"UPDATE", "SELECT", "INSERT"}, wantRows: 1, wantTotals: [3]int{1, 50, 20}},
		{name: "continue after a bucket fails", failAt: "UPDATE", anotherBucket: true, wantOps: []string{"UPDATE", "UPDATE", "SELECT", "INSERT"}, wantRows: 1, wantSaved: 1, wantTotals: [3]int{1, 50, 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncateTables(t)
			resetFlowQuotaDataCache(t)
			bucket := QuotaData{
				UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3600,
				UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-a",
				Count: 2, Quota: 100, TokenUsed: 40,
			}
			if tc.existing {
				seedFlowQuotaData(t, bucket)
			}
			bucket.Count, bucket.Quota, bucket.TokenUsed = 1, 50, 20
			if tc.noChange {
				bucket.Count, bucket.Quota, bucket.TokenUsed = 0, 0, 0
			}
			CacheQuotaDataLock.Lock()
			logQuotaDataCache(&bucket)
			if tc.anotherBucket {
				next := bucket
				next.ModelName = "gpt-b"
				logQuotaDataCache(&next)
			}
			CacheQuotaDataLock.Unlock()

			var output bytes.Buffer
			common.LogWriterMu.Lock()
			previousWriter, previousErrorWriter := gin.DefaultWriter, gin.DefaultErrorWriter
			gin.DefaultWriter, gin.DefaultErrorWriter = &output, &output
			common.LogWriterMu.Unlock()
			t.Cleanup(func() {
				common.LogWriterMu.Lock()
				gin.DefaultWriter, gin.DefaultErrorWriter = previousWriter, previousErrorWriter
				common.LogWriterMu.Unlock()
			})

			var operations []string
			recording, failed := true, false
			forcedError := errors.New("forced quota data failure")
			for _, callback := range []struct {
				operation string
				before    func(string, func(*gorm.DB)) error
				after     func(string, func(*gorm.DB)) error
				remove    func(string) error
			}{
				{"UPDATE", DB.Callback().Update().Before("gorm:update").Register, DB.Callback().Update().After("gorm:commit_or_rollback_transaction").Register, DB.Callback().Update().Remove},
				{"SELECT", DB.Callback().Query().Before("gorm:query").Register, DB.Callback().Query().After("gorm:query").Register, DB.Callback().Query().Remove},
				{"INSERT", DB.Callback().Create().Before("gorm:create").Register, DB.Callback().Create().After("gorm:commit_or_rollback_transaction").Register, DB.Callback().Create().Remove},
			} {
				beforeName := "test:quota_data_before_" + callback.operation
				afterName := "test:quota_data_after_" + callback.operation
				t.Cleanup(func() {
					require.NoError(t, callback.remove(beforeName))
					require.NoError(t, callback.remove(afterName))
				})
				require.NoError(t, callback.before(beforeName, func(tx *gorm.DB) {
					if !recording || tx.Statement.Table != "quota_data" {
						return
					}
					operations = append(operations, callback.operation)
					if tc.failAt == callback.operation && !tc.afterCommit && !failed {
						failed = true
						tx.AddError(forcedError)
					}
				}))
				require.NoError(t, callback.after(afterName, func(tx *gorm.DB) {
					if !recording || tx.Statement.Table != "quota_data" {
						return
					}
					if tc.noChange && callback.operation == "UPDATE" {
						// Exercise MySQL's changed-row semantics on SQLite too.
						tx.RowsAffected = 0
					}
					if tc.failAt == callback.operation && tc.afterCommit && !failed {
						failed = true
						tx.AddError(forcedError)
					}
				}))
			}

			SaveQuotaDataCache()
			assert.Equal(t, tc.wantOps, operations)
			assert.Contains(t, output.String(), common.LogText("saved dashboard data, %d records", tc.wantSaved))
			if tc.failAt != "" {
				assert.Contains(t, output.String(), common.LogText("failed to save dashboard data (%s): %s", tc.failAt, forcedError))
			}
			SaveQuotaDataCache()
			assert.Equal(t, tc.wantOps, operations, "flushing again must not replay the batch")
			recording = false
			var rows []QuotaData
			require.NoError(t, DB.Find(&rows).Error)
			require.Len(t, rows, tc.wantRows)
			for _, row := range rows {
				assert.Equal(t, tc.wantTotals, [3]int{row.Count, row.Quota, row.TokenUsed})
			}
		})
	}
}

func TestSaveQuotaDataCacheDimensionsAndDuplicates(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  QuotaData
	}{
		{"user", QuotaData{UserID: 1}},
		{"username", QuotaData{Username: "alice"}},
		{"model", QuotaData{ModelName: "gpt-a"}},
		{"hour", QuotaData{CreatedAt: 7200}},
		{"group", QuotaData{UseGroup: "vip"}},
		{"token", QuotaData{TokenID: 11}},
		{"channel", QuotaData{ChannelID: 1}},
		{"node", QuotaData{NodeName: "node-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncateTables(t)
			resetFlowQuotaDataCache(t)
			neighbor := tc.row
			if neighbor.CreatedAt == 0 {
				neighbor.CreatedAt = 3600
			}
			neighbor.Count, neighbor.Quota, neighbor.TokenUsed = 4, 200, 80
			require.NoError(t, DB.Create(&neighbor).Error)
			params := QuotaDataLogParams{CreatedAt: 3600, Quota: 50, TokenUsed: 20}
			LogQuotaData(params)
			SaveQuotaDataCache()

			var rows []QuotaData
			require.NoError(t, DB.Order("id").Find(&rows).Error)
			require.Len(t, rows, 2)
			assert.Equal(t, neighbor, rows[0])
			assert.Equal(t, QuotaData{Id: rows[1].Id, CreatedAt: 3600, Count: 1, Quota: 50, TokenUsed: 20}, rows[1])

			duplicate := QuotaData{CreatedAt: 3600, Count: 2, Quota: 100, TokenUsed: 40}
			require.NoError(t, DB.Create(&duplicate).Error)
			LogQuotaData(params)
			SaveQuotaDataCache()
			require.NoError(t, DB.Order("id").Find(&rows).Error)
			require.Len(t, rows, 3)
			assert.Equal(t, neighbor, rows[0])
			assert.Equal(t, QuotaData{Id: rows[1].Id, CreatedAt: 3600, Count: 2, Quota: 100, TokenUsed: 40}, rows[1])
			assert.Equal(t, QuotaData{Id: duplicate.Id, CreatedAt: 3600, Count: 3, Quota: 150, TokenUsed: 60}, rows[2])
		})
	}
}

func TestSaveQuotaDataCacheConcurrentBatches(t *testing.T) {
	var version string
	require.NoError(t, DB.Raw("SELECT sqlite_version()").Scan(&version).Error)
	t.Logf("SQLite version: %s", version)

	for _, tc := range []struct {
		name      string
		nextBatch bool
	}{
		{name: "new data waits for the next batch", nextBatch: true},
		{name: "empty cache waits for the active flush"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncateTables(t)
			resetFlowQuotaDataCache(t)

			updateStarted := make(chan struct{}, 3)
			releaseUpdates := make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseUpdates) })
			var workers sync.WaitGroup
			const callbackName = "test:block_quota_data_flush"
			require.NoError(t, DB.Callback().Update().Before("gorm:begin_transaction").Register(callbackName, func(tx *gorm.DB) {
				// Block before acquiring a connection so assertion queries can still run.
				if tx.Statement.Table != "quota_data" {
					return
				}
				updateStarted <- struct{}{}
				<-releaseUpdates
			}))
			t.Cleanup(func() {
				release()
				workers.Wait()
				require.NoError(t, DB.Callback().Update().Remove(callbackName))
			})

			params := QuotaDataLogParams{
				UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3661,
				UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-a",
				Quota: 100, TokenUsed: 40,
			}
			LogQuotaData(params)
			firstDone := make(chan struct{})
			workers.Go(func() {
				SaveQuotaDataCache()
				close(firstDone)
			})
			select {
			case <-updateStarted:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "first flush did not reach the database")
			}

			if tc.nextBatch {
				logged := make(chan struct{})
				workers.Go(func() {
					next := params
					next.Quota, next.TokenUsed = 50, 20
					LogQuotaData(next)
					next.ModelName = "gpt-b"
					next.Quota, next.TokenUsed = 25, 10
					LogQuotaData(next)
					close(logged)
				})
				select {
				case <-logged:
				case <-time.After(5 * time.Second):
					require.FailNow(t, "cache writes blocked on the database flush")
				}
			}

			secondStarted := make(chan struct{})
			secondDone := make(chan struct{})
			workers.Go(func() {
				close(secondStarted)
				SaveQuotaDataCache()
				close(secondDone)
			})
			<-secondStarted
			select {
			case <-updateStarted:
				require.FailNow(t, "second flush reached the database before the first finished")
			case <-secondDone:
				require.FailNow(t, "second flush returned before the first finished")
			case <-time.After(100 * time.Millisecond):
			}

			// Complete only the first update; the second batch remains blocked.
			releaseUpdates <- struct{}{}
			select {
			case <-firstDone:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "first flush did not finish")
			}
			var rows []QuotaData
			require.NoError(t, DB.Order("model_name").Find(&rows).Error)
			require.Len(t, rows, 1)
			assert.Equal(t, "gpt-a", rows[0].ModelName)
			assert.Equal(t, 1, rows[0].Count)
			assert.Equal(t, 100, rows[0].Quota)
			assert.Equal(t, 40, rows[0].TokenUsed)

			release()
			select {
			case <-secondDone:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "second flush did not finish")
			}
			SaveQuotaDataCache()
			require.NoError(t, DB.Order("model_name").Find(&rows).Error)
			if tc.nextBatch {
				require.Len(t, rows, 2)
				assert.Equal(t, "gpt-a", rows[0].ModelName)
				assert.Equal(t, 2, rows[0].Count)
				assert.Equal(t, 150, rows[0].Quota)
				assert.Equal(t, 60, rows[0].TokenUsed)
				assert.Equal(t, "gpt-b", rows[1].ModelName)
				assert.Equal(t, 1, rows[1].Count)
				assert.Equal(t, 25, rows[1].Quota)
				assert.Equal(t, 10, rows[1].TokenUsed)
			} else {
				require.Len(t, rows, 1)
				assert.Equal(t, 1, rows[0].Count)
				assert.Equal(t, 100, rows[0].Quota)
				assert.Equal(t, 40, rows[0].TokenUsed)
			}
		})
	}
}
