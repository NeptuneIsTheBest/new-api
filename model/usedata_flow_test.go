package model

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func seedFlowQuotaData(t *testing.T, quotaData QuotaData) {
	t.Helper()
	require.NoError(t, DB.Create(&quotaData).Error)
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
	CacheQuotaDataLock.Lock()
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaDataLock.Unlock()

	LogQuotaData(QuotaDataLogParams{
		UserID:              1,
		Username:            "alice",
		ModelName:           "gpt-a",
		CreatedAt:           3661,
		UseGroup:            "vip",
		TokenID:             11,
		ChannelID:           1,
		NodeName:            "node-a",
		Quota:               100,
		TokenUsed:           40,
		CacheReadTokens:     20,
		CacheCreationTokens: 5,
	})
	LogQuotaData(QuotaDataLogParams{
		UserID:              1,
		Username:            "alice",
		ModelName:           "gpt-a",
		CreatedAt:           3700,
		UseGroup:            "vip",
		TokenID:             11,
		ChannelID:           1,
		NodeName:            "node-a",
		Quota:               50,
		TokenUsed:           20,
		CacheReadTokens:     10,
		CacheCreationTokens: 2,
	})
	LogQuotaData(QuotaDataLogParams{
		UserID:              1,
		Username:            "alice",
		ModelName:           "gpt-a",
		CreatedAt:           3700,
		UseGroup:            "default",
		TokenID:             11,
		ChannelID:           1,
		NodeName:            "node-a",
		Quota:               25,
		TokenUsed:           10,
		CacheReadTokens:     -10,
		CacheCreationTokens: -2,
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
	assert.Equal(t, int64(30), rows[0].CacheReadTokens)
	assert.Equal(t, int64(7), rows[0].CacheCreationTokens)
	require.Equal(t, "default", rows[1].UseGroup)
	require.Equal(t, 25, rows[1].Quota)
	assert.Zero(t, rows[1].CacheReadTokens)
	assert.Zero(t, rows[1].CacheCreationTokens)

	for _, query := range []struct {
		name  string
		fetch func() ([]*QuotaData, error)
	}{
		{"username", func() ([]*QuotaData, error) { return GetQuotaDataByUsername("alice", 3600, 7200) }},
		{"user ID", func() ([]*QuotaData, error) { return GetQuotaDataByUserId(1, 3600, 7200) }},
		{"grouped by user", func() ([]*QuotaData, error) { return GetQuotaDataGroupByUser(3600, 7200) }},
		{"all models", func() ([]*QuotaData, error) { return GetAllQuotaDates(3600, 7200, "") }},
	} {
		t.Run(query.name, func(t *testing.T) {
			stats, err := query.fetch()
			require.NoError(t, err)
			require.Len(t, stats, 1)
			assert.Equal(t, 175, stats[0].Quota)
			assert.Equal(t, 70, stats[0].TokenUsed)
			assert.Equal(t, int64(30), stats[0].CacheReadTokens)
			assert.Equal(t, int64(7), stats[0].CacheCreationTokens)
		})
	}
}

func setupQuotaDataCacheTestDB(t *testing.T, dialect string) *gorm.DB {
	t.Helper()
	var driver gorm.Dialector
	switch dialect {
	case "sqlite":
		driver = sqlite.Open(":memory:")
	case "mysql":
		dsn := os.Getenv("TEST_MYSQL_DSN")
		if dsn == "" {
			t.Skip("TEST_MYSQL_DSN is not configured")
		}
		driver = mysql.Open(dsn)
	case "postgres":
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
	default:
		t.Fatalf("unsupported test database %q", dialect)
	}
	db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{
		TablePrefix: fmt.Sprintf("quota_cache_%d_", time.Now().UnixNano()),
	}})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	previousDB := DB
	DB = db
	CacheQuotaDataLock.Lock()
	previousCache := CacheQuotaData
	CacheQuotaData = make(map[string]*QuotaData)
	CacheQuotaDataLock.Unlock()
	t.Cleanup(func() {
		DB = previousDB
		CacheQuotaDataLock.Lock()
		CacheQuotaData = previousCache
		CacheQuotaDataLock.Unlock()
		assert.NoError(t, db.Migrator().DropTable(&QuotaData{}))
		assert.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&QuotaData{}))
	return db
}

func TestSaveQuotaDataCacheDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := setupQuotaDataCacheTestDB(t, dialect)
			var version string
			versionQuery := "SELECT version()"
			if dialect == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s", version)
			recorder := &migrationSQLRecorder{}
			DB = db.Session(&gorm.Session{Logger: recorder})
			params := QuotaDataLogParams{
				UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3661,
				UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-a",
				Quota: 100, TokenUsed: 40,
				CacheReadTokens: 20, CacheCreationTokens: 5,
			}
			zeroUsage := params
			zeroUsage.Quota, zeroUsage.TokenUsed = 0, 0
			zeroUsage.CacheReadTokens, zeroUsage.CacheCreationTokens = 0, 0
			for _, tc := range []struct {
				name       string
				params     *QuotaDataLogParams
				operations []string
				count      int
				usageCount int
			}{
				{name: "empty", operations: []string{}},
				{name: "new bucket", params: &params, operations: []string{"UPDATE", "INSERT"}, count: 1, usageCount: 1},
				{name: "existing bucket", params: &params, operations: []string{"UPDATE"}, count: 2, usageCount: 2},
				{name: "existing bucket with zero usage", params: &zeroUsage, operations: []string{"UPDATE"}, count: 3, usageCount: 2},
				{name: "already flushed", operations: []string{}, count: 3, usageCount: 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if tc.params != nil {
						LogQuotaData(*tc.params)
					}
					recorder.reset()
					SaveQuotaDataCache()
					operations := make([]string, 0, len(recorder.statements))
					for _, statement := range recorder.statements {
						operations = append(operations, strings.Fields(statement)[0])
					}
					assert.Equal(t, tc.operations, operations)
					var rows []QuotaData
					require.NoError(t, db.Find(&rows).Error)
					if tc.count == 0 {
						assert.Empty(t, rows)
						return
					}
					require.Len(t, rows, 1)
					assert.Equal(t, tc.count, rows[0].Count)
					assert.Equal(t, 100*tc.usageCount, rows[0].Quota)
					assert.Equal(t, 40*tc.usageCount, rows[0].TokenUsed)
					assert.Equal(t, int64(20*tc.usageCount), rows[0].CacheReadTokens)
					assert.Equal(t, int64(5*tc.usageCount), rows[0].CacheCreationTokens)
					assert.Equal(t, int64(3600), rows[0].CreatedAt)
				})
			}
		})
	}
}

func TestSaveQuotaDataCacheAcceptsRecordsDuringFlush(t *testing.T) {
	db := setupQuotaDataCacheTestDB(t, "sqlite")
	params := QuotaDataLogParams{
		UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3661,
		UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-a",
		Quota: 100, TokenUsed: 40,
		CacheReadTokens: 20, CacheCreationTokens: 5,
	}
	LogQuotaData(params)
	entered, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	var once sync.Once
	require.NoError(t, db.Callback().Update().Before("gorm:begin_transaction").Register("quota_cache_pause", func(tx *gorm.DB) {
		once.Do(func() {
			close(entered)
			<-resume
		})
	}))
	var workers sync.WaitGroup
	t.Cleanup(func() {
		release()
		workers.Wait()
		assert.NoError(t, db.Callback().Update().Remove("quota_cache_pause"))
	})
	workers.Go(SaveQuotaDataCache)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not reach the database callback")
	}
	recorded := make(chan struct{})
	workers.Go(func() {
		LogQuotaData(params)
		close(recorded)
	})
	select {
	case <-recorded:
	case <-time.After(5 * time.Second):
		t.Fatal("recording statistics blocked on the paused database write")
	}
	workers.Go(SaveQuotaDataCache)
	release()
	workers.Wait()
	var rows []QuotaData
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, 2, rows[0].Count)
	assert.Equal(t, 200, rows[0].Quota)
	assert.Equal(t, 80, rows[0].TokenUsed)
	assert.Equal(t, int64(40), rows[0].CacheReadTokens)
	assert.Equal(t, int64(10), rows[0].CacheCreationTokens)
}

func TestSaveQuotaDataCacheRetriesOnlyFailedBuckets(t *testing.T) {
	for _, operation := range []string{"update", "insert"} {
		t.Run(operation, func(t *testing.T) {
			db := setupQuotaDataCacheTestDB(t, "sqlite")
			params := QuotaDataLogParams{
				UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3661,
				UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-a",
				Quota: 100, TokenUsed: 40,
				CacheReadTokens: 20, CacheCreationTokens: 5,
			}
			initialCount, initialQuota, initialTokens := 0, 0, 0
			var initialRead, initialCreation int64
			if operation == "update" {
				initialCount, initialQuota, initialTokens = 1, 10, 4
				initialRead, initialCreation = 2, 1
				require.NoError(t, db.Create(&QuotaData{
					UserID: 1, Username: "alice", ModelName: "gpt-a", CreatedAt: 3600,
					UseGroup: "vip", TokenID: 11, ChannelID: 1, NodeName: "node-a",
					Count: initialCount, Quota: initialQuota, TokenUsed: initialTokens,
					CacheReadTokens: initialRead, CacheCreationTokens: initialCreation,
				}).Error)
			}
			LogQuotaData(params)
			success := params
			success.UserID, success.Username = 2, "bob"
			LogQuotaData(success)
			newUsage := params
			newUsage.Quota, newUsage.TokenUsed = 50, 20
			newUsage.CacheReadTokens, newUsage.CacheCreationTokens = 10, 2
			writeErr := errors.New("forced dashboard write failure")
			callbacks, after := db.Callback().Update(), "gorm:update"
			if operation == "insert" {
				callbacks, after = db.Callback().Create(), "gorm:create"
			}
			require.NoError(t, callbacks.After(after).Register("quota_cache_failure", func(tx *gorm.DB) {
				if tx.Error != nil || tx.RowsAffected == 0 {
					return
				}
				if operation == "insert" {
					row := tx.Statement.Dest.(*QuotaData)
					if row.UserID != params.UserID {
						return
					}
					assert.NotZero(t, row.Id, "the rolled-back insert assigned a primary key")
				}
				LogQuotaData(newUsage)
				tx.AddError(writeErr)
			}))
			removeFailure := sync.OnceFunc(func() {
				assert.NoError(t, callbacks.Remove("quota_cache_failure"))
			})
			t.Cleanup(removeFailure)
			SaveQuotaDataCache()
			removeFailure()
			var rows []QuotaData
			require.NoError(t, db.Order("user_id").Find(&rows).Error)
			if operation == "update" {
				require.Len(t, rows, 2)
				assert.Equal(t, initialCount, rows[0].Count)
				assert.Equal(t, initialQuota, rows[0].Quota)
				assert.Equal(t, initialTokens, rows[0].TokenUsed)
				assert.Equal(t, initialRead, rows[0].CacheReadTokens)
				assert.Equal(t, initialCreation, rows[0].CacheCreationTokens)
			} else {
				require.Len(t, rows, 1)
				assert.Equal(t, success.UserID, rows[0].UserID)
			}
			// Occupy any primary key released by the failed insert before retrying it.
			require.NoError(t, db.Create(&QuotaData{UserID: 3, CreatedAt: 3600, Count: 1}).Error)
			SaveQuotaDataCache()
			SaveQuotaDataCache()
			rows = nil
			require.NoError(t, db.Order("user_id").Find(&rows).Error)
			require.Len(t, rows, 3)
			assert.Equal(t, initialCount+2, rows[0].Count)
			assert.Equal(t, initialQuota+150, rows[0].Quota)
			assert.Equal(t, initialTokens+60, rows[0].TokenUsed)
			assert.Equal(t, initialRead+30, rows[0].CacheReadTokens)
			assert.Equal(t, initialCreation+7, rows[0].CacheCreationTokens)
			assert.Equal(t, 1, rows[1].Count, "successfully saved buckets must not be retried")
			assert.Equal(t, success.Quota, rows[1].Quota)
			assert.Equal(t, success.TokenUsed, rows[1].TokenUsed)
			assert.Equal(t, success.CacheReadTokens, rows[1].CacheReadTokens)
			assert.Equal(t, success.CacheCreationTokens, rows[1].CacheCreationTokens)
			assert.Equal(t, 1, rows[2].Count)
		})
	}
}
